package execenv

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// The apply stream lets the host drive slot updates inside a Docker volume
// it cannot write directly (macOS: the volume lives in the Docker VM). The
// host runs `kran internal apply --base /kran/slots` in the long-lived
// kran-sync container via `docker exec -i` and speaks this protocol over
// stdin/stdout:
//
//	host → apply   'Q' request {op, slot, manifest}
//	apply → host   'N' {needed hashes, changed, deleted}       (op commit/check)
//	host → apply   per needed hash: 'B' {hash,size} then 'D' chunks   (commit)
//	               or 'A' {error} to abort
//	apply → host   'R' {ok, error, changed, deleted}
//
// Frames are type:u8 | len:u32be | payload. Blob content is verified
// against its hash on the apply side too.

const (
	afReq    = 'Q'
	afNeed   = 'N'
	afBlob   = 'B'
	afData   = 'D'
	afAbort  = 'A'
	afResult = 'R'

	maxApplyFrame = 64 << 20
	applyChunk    = 256 << 10
)

type applyReq struct {
	Op       string          `json:"op"`   // commit | check | remove
	Slot     string          `json:"slot"` // "<repo>/<n>" below base
	Manifest *proto.Manifest `json:"manifest,omitempty"`
}

type applyNeed struct {
	Needed  []string `json:"needed"`
	Changed int      `json:"changed"`
	Deleted int      `json:"deleted"`
}

type applyBlob struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type applyResult struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Changed int    `json:"changed"`
	Deleted int    `json:"deleted"`
}

func writeAF(w io.Writer, t byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = t
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func writeAFJSON(w io.Writer, t byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAF(w, t, b)
}

func readAF(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxApplyFrame {
		return 0, nil, fmt.Errorf("apply frame of %d bytes too large", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	return hdr[0], b, nil
}

// slotSubpath validates "<repo>/<n>".
func slotSubpath(s string) (string, error) {
	repo, idx, ok := strings.Cut(s, "/")
	if !ok {
		return "", fmt.Errorf("bad slot %q", s)
	}
	if err := validRepoID(repo); err != nil {
		return "", err
	}
	if n, err := strconv.Atoi(idx); err != nil || n < 0 || n > 99 || strconv.Itoa(n) != idx {
		return "", fmt.Errorf("bad slot index in %q", s)
	}
	return repo + "/" + idx, nil
}

// Apply is the entry point of `kran internal apply --base DIR` (runs inside
// the kran-sync container). It serves one request from stdin and returns
// the process exit code.
func Apply(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	base := fs.String("base", "/kran/slots", "directory holding the slots")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriter(os.Stdout)
	err := serveApply(*base, in, out)
	out.Flush()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kran apply:", err)
		return 1
	}
	return 0
}

type flusher interface{ Flush() error }

func flush(w io.Writer) {
	if f, ok := w.(flusher); ok {
		f.Flush()
	}
}

func serveApply(base string, r io.Reader, w io.Writer) error {
	t, b, err := readAF(r)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	if t != afReq {
		return fmt.Errorf("unexpected frame %q", t)
	}
	var req applyReq
	if err := json.Unmarshal(b, &req); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	res := applyResult{}
	err = handleApply(base, &req, r, w, &res)
	if err != nil {
		res.Error = err.Error()
	} else {
		res.OK = true
	}
	if werr := writeAFJSON(w, afResult, res); werr != nil {
		return werr
	}
	flush(w)
	return nil
}

func handleApply(base string, req *applyReq, r io.Reader, w io.Writer, res *applyResult) error {
	sub, err := slotSubpath(req.Slot)
	if err != nil {
		return err
	}
	root := filepath.Join(base, filepath.FromSlash(sub))
	switch req.Op {
	case "remove":
		return os.RemoveAll(root)
	case "commit", "check":
	default:
		return fmt.Errorf("unknown op %q", req.Op)
	}
	if req.Manifest == nil {
		return errors.New("request without manifest")
	}
	if err := validateManifest(req.Manifest, false); err != nil {
		return err
	}
	keep := keepMatcher(req.Manifest)
	state, err := scanSlot(root, keep)
	if err != nil {
		return err
	}
	p := computePlan(req.Manifest, state)
	res.Changed, res.Deleted = p.changed(), len(p.deletes)
	if err := writeAFJSON(w, afNeed, applyNeed{Needed: p.needed, Changed: res.Changed, Deleted: res.Deleted}); err != nil {
		return err
	}
	flush(w)
	if req.Op == "check" {
		return nil
	}
	s, err := openSlotFS(root)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.prepare(p); err != nil {
		return err
	}
	byHash := map[string][]int{}
	for _, i := range p.writes {
		h := p.m.Entries[i].Hash
		byHash[h] = append(byHash[h], i)
	}
	for _, want := range p.needed {
		t, b, err := readAF(r)
		if err != nil {
			return fmt.Errorf("read blob header: %w", err)
		}
		if t == afAbort {
			var a applyResult
			json.Unmarshal(b, &a)
			return fmt.Errorf("aborted by host: %s", a.Error)
		}
		var hdr applyBlob
		if t != afBlob || json.Unmarshal(b, &hdr) != nil || hdr.Hash != want {
			return fmt.Errorf("protocol: expected blob %s", short(want))
		}
		if err := s.writeStreamed(p, byHash[want], hdr, r); err != nil {
			return err
		}
	}
	return s.finishMeta(p)
}

// writeStreamed writes the first path needing hdr.Hash from the stream and
// copies it locally to the others.
func (s *slotFS) writeStreamed(p *plan, idx []int, hdr applyBlob, r io.Reader) error {
	first := &p.m.Entries[idx[0]]
	dir, name := splitRel(first.Path)
	fd, err := s.openDir(dir, true)
	if err != nil {
		return err
	}
	defer s.closeDir(fd)
	tmp := tmpPrefix + randHex(6)
	ffd, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", first.Path, err)
	}
	f := os.NewFile(uintptr(ffd), tmp)
	h := sha256.New()
	bw := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)
	var got int64
	for got < hdr.Size {
		t, b, err := readAF(r)
		if err != nil || t != afData {
			f.Close()
			unix.Unlinkat(fd, tmp, 0)
			return fmt.Errorf("read blob %s: protocol error (%v)", short(hdr.Hash), err)
		}
		bw.Write(b)
		got += int64(len(b))
	}
	werr := bw.Flush()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil && (got != hdr.Size || hex.EncodeToString(h.Sum(nil)) != hdr.Hash || first.Size != hdr.Size) {
		werr = fmt.Errorf("blob %s: %w", short(hdr.Hash), ErrBlobMismatch)
	}
	if werr != nil {
		unix.Unlinkat(fd, tmp, 0)
		return fmt.Errorf("write %s: %w", first.Path, werr)
	}
	if err := finishFile(fd, tmp, name, first.Mode, first.MTime); err != nil {
		return fmt.Errorf("write %s: %w", first.Path, err)
	}
	for _, i := range idx[1:] {
		if err := s.copyWithin(first.Path, &p.m.Entries[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *slotFS) copyWithin(srcRel string, e *proto.ManifestEntry) error {
	sdir, sname := splitRel(srcRel)
	sfd, err := s.openDir(sdir, false)
	if err != nil {
		return err
	}
	inFd, err := unix.Openat(sfd, sname, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	s.closeDir(sfd)
	if err != nil {
		return fmt.Errorf("copy %s: %w", e.Path, err)
	}
	in := os.NewFile(uintptr(inFd), sname)
	defer in.Close()
	dir, name := splitRel(e.Path)
	fd, err := s.openDir(dir, true)
	if err != nil {
		return err
	}
	defer s.closeDir(fd)
	tmp := tmpPrefix + randHex(6)
	ofd, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("copy %s: %w", e.Path, err)
	}
	out := os.NewFile(uintptr(ofd), tmp)
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		unix.Unlinkat(fd, tmp, 0)
		return fmt.Errorf("copy %s: %w", e.Path, err)
	}
	return finishFile(fd, tmp, name, e.Mode, e.MTime)
}

// blobOpener yields blob content for streaming.
type blobOpener func(hash string) (io.ReadCloser, int64, error)

// driveApply is the host side of the protocol over an apply process's
// stdin (w) and stdout (r). For op "check" open may be nil. It returns the
// apply side's need list and result.
func driveApply(ctx context.Context, w io.Writer, r io.Reader, req applyReq, open blobOpener) (applyNeed, applyResult, error) {
	var need applyNeed
	var res applyResult
	bw := bufio.NewWriterSize(w, 1<<20)
	if err := writeAFJSON(bw, afReq, req); err != nil {
		return need, res, fmt.Errorf("send request: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return need, res, fmt.Errorf("send request: %w", err)
	}
	t, b, err := readAF(r)
	if err != nil {
		return need, res, fmt.Errorf("read apply reply: %w", err)
	}
	if t == afResult {
		json.Unmarshal(b, &res)
		if !res.OK {
			return need, res, fmt.Errorf("apply: %s", res.Error)
		}
		return need, res, nil
	}
	if t != afNeed || json.Unmarshal(b, &need) != nil {
		return need, res, fmt.Errorf("apply: unexpected reply %q", t)
	}
	if req.Op == "commit" {
		// The apply side may run where jobs can replace it (the container
		// helper): send only blobs of this manifest.
		inManifest := map[string]bool{}
		if req.Manifest != nil {
			for _, e := range req.Manifest.Entries {
				if e.Kind == proto.EntryFile && e.Hash != "" {
					inManifest[e.Hash] = true
				}
			}
		}
		for _, h := range need.Needed {
			if !inManifest[h] {
				return need, res, abortApply(bw, r, fmt.Errorf("apply asked for blob %q, which is not in the manifest", h))
			}
		}
		buf := make([]byte, applyChunk)
		for _, h := range need.Needed {
			if err := ctx.Err(); err != nil {
				return need, res, abortApply(bw, r, err)
			}
			rc, size, err := open(h)
			if err != nil {
				return need, res, abortApply(bw, r, err)
			}
			err = sendBlob(bw, h, size, rc, buf)
			rc.Close()
			if err != nil {
				// The apply side may have failed first and closed its
				// stdin; its result says why.
				if t, b, rerr := readAF(r); rerr == nil && t == afResult && json.Unmarshal(b, &res) == nil && res.Error != "" {
					return need, res, fmt.Errorf("apply: %s", res.Error)
				}
				return need, res, err
			}
		}
		if err := bw.Flush(); err != nil {
			return need, res, fmt.Errorf("send blobs: %w", err)
		}
	}
	t, b, err = readAF(r)
	if err != nil {
		return need, res, fmt.Errorf("read apply result: %w", err)
	}
	if t != afResult || json.Unmarshal(b, &res) != nil {
		return need, res, fmt.Errorf("apply: unexpected result frame %q", t)
	}
	if !res.OK {
		return need, res, fmt.Errorf("apply: %s", res.Error)
	}
	return need, res, nil
}

// abortApply tells the apply side to stop and waits for its final result
// so the process exits cleanly.
func abortApply(w *bufio.Writer, r io.Reader, cause error) error {
	if writeAFJSON(w, afAbort, applyResult{Error: cause.Error()}) == nil && w.Flush() == nil {
		readAF(r)
	}
	return cause
}

func sendBlob(w io.Writer, hash string, size int64, r io.Reader, buf []byte) error {
	if err := writeAFJSON(w, afBlob, applyBlob{Hash: hash, Size: size}); err != nil {
		return fmt.Errorf("send blob %s: %w", short(hash), err)
	}
	var sent int64
	for sent < size {
		n, err := r.Read(buf[:min(int64(len(buf)), size-sent)])
		if n > 0 {
			if werr := writeAF(w, afData, buf[:n]); werr != nil {
				return fmt.Errorf("send blob %s: %w", short(hash), werr)
			}
			sent += int64(n)
		}
		if err != nil {
			if err == io.EOF && sent == size {
				break
			}
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("send blob %s: %w", short(hash), err)
		}
	}
	return nil
}
