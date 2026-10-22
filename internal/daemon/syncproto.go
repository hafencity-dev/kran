package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

// Workspace sync, peer protocol 1.1 (SPEC §6.7, §8.3). A 1.1 target says
// so in its slot.acquire answer ("sync": 1). The origin then sends the
// manifest of slot.diff in the compact encoding, and against a base the
// target holds (the last manifest this origin synced from the same
// worktree to that node) only as a delta, or not at all when it is the
// same. Without the field (a 1.0 target) the origin sends zstd JSON as
// before; a 1.0 origin sends no slot.diff body and gets the same.

// syncRevision is what this node's slot.acquire answers in "sync".
const syncRevision = 1

// codeManifestBase: the target does not hold the base manifest a slot.diff
// refers to (restarted, evicted) or the result does not hash as the origin
// said; the origin sends the whole manifest instead.
const codeManifestBase = "manifest_base"

// maxManifestEntries bounds a decoded manifest or delta.
const maxManifestEntries = 4 << 20

// slotAcquireReply is the slot.acquire answer: the slot, plus the sync
// revision the target speaks (absent from 1.0 targets).
type slotAcquireReply struct {
	proto.SlotRef
	Sync int `json:"sync,omitempty"`
}

// slotDiffBody is the slot.diff request body (absent from 1.0 origins).
type slotDiffBody struct {
	Format string `json:"format"`         // proto.Manifest{JSON,KMF1,Delta,Same}
	Hash   string `json:"hash,omitempty"` // proto.ManifestHash of the manifest
	Base   string `json:"base,omitempty"` // hash of the base (kmd1, same)
}

// originManifests is the origin's memory of what it synced where.
type originManifests struct {
	cache *manifestCache // by hash
	base  lastSent       // node + worktree → hash the target accepted
}

func newOriginManifests() *originManifests {
	return &originManifests{cache: newManifestCache(manifestCacheEntries)}
}

// remoteDiff sends the manifest and returns the target's plan. rev is the
// target's sync revision from slot.acquire.
func (d *Daemon) remoteDiff(ctx context.Context, sess transport.Session, id, node string, rev int, m *proto.Manifest) (proto.SyncPlan, error) {
	if rev < 1 {
		return d.sendDiff(ctx, sess, id, nil, func(w io.Writer) error { return json.NewEncoder(w).Encode(m) })
	}
	full, err := proto.EncodeManifestBytes(m)
	if err != nil { // unsorted (never from Scan): the old way works for any manifest
		return d.sendDiff(ctx, sess, id, nil, func(w io.Writer) error { return json.NewEncoder(w).Encode(m) })
	}
	sum := sha256.Sum256(full)
	hash := hex.EncodeToString(sum[:])
	om := d.manifests
	body := &slotDiffBody{Format: proto.ManifestKMF1, Hash: hash}
	payload := full
	if base := om.base.get(node, m.Root); base == hash {
		body.Format, body.Base, payload = proto.ManifestSame, base, nil
	} else if bm := om.cache.get(base); bm != nil {
		if delta, err := proto.DiffManifests(base, bm, m); err == nil {
			var b bytes.Buffer
			if proto.EncodeDelta(&b, delta) == nil && b.Len() < len(full) {
				body.Format, body.Base, payload = proto.ManifestDelta, base, b.Bytes()
			}
		}
	}
	send := func(w io.Writer) error {
		if payload == nil {
			return nil
		}
		_, err := w.Write(payload)
		return err
	}
	plan, err := d.sendDiff(ctx, sess, id, body, send)
	if peerCode(err) == codeManifestBase && body.Format != proto.ManifestKMF1 {
		body = &slotDiffBody{Format: proto.ManifestKMF1, Hash: hash}
		payload = full
		plan, err = d.sendDiff(ctx, sess, id, body, send)
	}
	if err != nil {
		return plan, err
	}
	om.cache.put(hash, m)
	om.base.set(node, m.Root, hash)
	return plan, nil
}

// sendDiff runs one slot.diff exchange: the body, the zstd-compressed
// payload written by write (nothing for an empty payload), the plan.
func (d *Daemon) sendDiff(ctx context.Context, sess transport.Session, id string, body *slotDiffBody, write func(io.Writer) error) (proto.SyncPlan, error) {
	var plan proto.SyncPlan
	dctx, cancel := context.WithTimeout(ctx, d.t.PrestartTimeout)
	defer cancel()
	var reqBody any
	if body != nil {
		reqBody = body
	}
	pc, err := peerCall(dctx, sess, proto.PeerSlotDiff, id, reqBody, nil)
	if err != nil {
		return plan, err
	}
	defer pc.Close()
	stop := context.AfterFunc(dctx, func() { pc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if body == nil || body.Format != proto.ManifestSame {
		enc, err := zstd.NewWriter(frameWriter{pc}, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		if err != nil {
			return plan, err
		}
		if err := write(enc); err != nil {
			enc.Close()
			return plan, fmt.Errorf("send manifest: %w", ctxErr(dctx, err))
		}
		if err := enc.Close(); err != nil {
			return plan, fmt.Errorf("send manifest: %w", ctxErr(dctx, err))
		}
	}
	if err := pc.writeFrame(pfEnd, nil); err != nil {
		return plan, fmt.Errorf("send manifest: %w", ctxErr(dctx, err))
	}
	if err := pc.readResponse(proto.PeerSlotDiff, &plan); err != nil {
		return plan, ctxErr(dctx, err)
	}
	return plan, nil
}

// manifestKey scopes the target's cache to the origin that sent it.
func manifestKey(origin, hash string) string { return origin + "\x00" + hash }

// readManifest reads the manifest of a slot.diff stream (through pfEnd)
// in the format the body names. base is the manifest body.Base refers to
// (kmd1, same). It returns the manifest and its hash ("" for JSON).
func (t *targetSide) readManifest(pc *peerConn, b slotDiffBody, base *proto.Manifest) (*proto.Manifest, string, error) {
	fr := &frameReader{c: pc}
	defer fr.drain()
	if b.Format == proto.ManifestSame {
		if b.Hash != b.Base {
			return nil, "", &peerError{Code: codeBadRequest, Msg: "slot.diff: same manifest with two hashes"}
		}
		return base, b.Base, nil
	}
	dec, err := newZstdDecoder()
	if err != nil {
		return nil, "", err
	}
	defer dec.Close()
	if err := dec.Reset(fr); err != nil {
		return nil, "", err
	}
	if b.Format == "" || b.Format == proto.ManifestJSON {
		var m proto.Manifest
		if err := decodeManifest(dec, &m); err != nil {
			return nil, "", err
		}
		return &m, "", nil
	}
	lr := &io.LimitedReader{R: dec, N: maxManifest + 1}
	tooBig := func(err error) error {
		if lr.N <= 0 {
			return fmt.Errorf("manifest larger than %d MiB", maxManifest>>20)
		}
		return err
	}
	switch b.Format {
	case proto.ManifestKMF1:
		h := sha256.New()
		m, err := proto.DecodeManifest(io.TeeReader(lr, h), maxManifestEntries)
		if err != nil {
			return nil, "", tooBig(err)
		}
		hash := hex.EncodeToString(h.Sum(nil))
		if b.Hash != "" && b.Hash != hash {
			return nil, "", fmt.Errorf("manifest hashes to %.12s, not %.12s", hash, b.Hash)
		}
		return m, hash, nil
	case proto.ManifestDelta:
		delta, err := proto.DecodeDelta(lr, maxManifestEntries)
		if err != nil {
			return nil, "", tooBig(err)
		}
		if delta.Base != b.Base {
			return nil, "", errors.New("delta names another base than the request")
		}
		m, err := delta.Apply(base)
		if err != nil {
			return nil, "", &peerError{Code: codeManifestBase, Msg: err.Error()}
		}
		hash := proto.ManifestHash(m)
		if hash != b.Hash {
			return nil, "", &peerError{Code: codeManifestBase, Msg: "manifest after the delta differs from the origin's"}
		}
		return m, hash, nil
	}
	return nil, "", fmt.Errorf("unknown manifest format %q", b.Format)
}
