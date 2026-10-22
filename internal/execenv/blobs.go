package execenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// blobStore is the content-addressed store blobs/<sha[:2]>/<sha> shared by
// all repos (SPEC §6.7). Blobs are immutable once renamed into place; their
// mtime records last use for LRU trimming.
type blobStore struct {
	l    layout
	logf func(string, ...any)

	// trimMu: commits hold it shared while they clone from blobs, trim
	// holds it exclusively while deleting, so a blob never vanishes under
	// a running commit.
	trimMu sync.RWMutex
	total  atomic.Int64 // approximate bytes stored; -1 = unknown
}

func newBlobStore(l layout, logf func(string, ...any)) (*blobStore, error) {
	if err := os.MkdirAll(l.blobTmp(), 0o700); err != nil {
		return nil, fmt.Errorf("blob store: %w", err)
	}
	// Leftovers of interrupted uploads.
	if ents, err := os.ReadDir(l.blobTmp()); err == nil {
		for _, e := range ents {
			os.Remove(filepath.Join(l.blobTmp(), e.Name()))
		}
	}
	b := &blobStore{l: l, logf: logf}
	b.total.Store(-1)
	return b, nil
}

func (b *blobStore) path(hash string) string { return b.l.blob(hash) }

func (b *blobStore) has(hash string) bool {
	_, err := os.Lstat(b.l.blob(hash))
	return err == nil
}

// put stores one blob. It consumes exactly size bytes from r even when the
// blob already exists, so a caller streaming several blobs over one
// connection stays in sync. The size comes from a peer and bounds what
// lands on disk, so it must be known.
func (b *blobStore) put(ctx context.Context, hash string, size int64, r io.Reader) error {
	if err := validHash(hash); err != nil {
		return err
	}
	if size < 0 {
		return fmt.Errorf("blob %s: size %d", short(hash), size)
	}
	var src io.Reader = ctxReader{ctx, io.LimitReader(r, size)}
	if b.has(hash) {
		n, err := io.Copy(io.Discard, src)
		if err != nil {
			return fmt.Errorf("blob %s: %w", short(hash), err)
		}
		if n != size {
			return fmt.Errorf("blob %s: got %d of %d bytes: %w", short(hash), n, size, io.ErrUnexpectedEOF)
		}
		b.touch(hash)
		return nil
	}
	tmp, err := os.CreateTemp(b.l.blobTmp(), "put-*")
	if err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return fmt.Errorf("blob %s: %w", short(hash), err)
	}
	if n != size {
		return fmt.Errorf("blob %s: got %d of %d bytes: %w", short(hash), n, size, io.ErrUnexpectedEOF)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hash {
		return fmt.Errorf("blob %s: content hashes to %s: %w", short(hash), short(got), ErrBlobMismatch)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blob %s: %w", short(hash), err)
	}
	dst := b.l.blob(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	// Blobs are read-only so a stray write through a copied (non-cloned)
	// path can never corrupt the store.
	os.Chmod(tmp.Name(), 0o400)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("blob store: %w", err)
	}
	ok = true
	if t := b.total.Load(); t >= 0 {
		b.total.Add(n)
	}
	return nil
}

// touch marks a blob as recently used.
func (b *blobStore) touch(hash string) {
	now := time.Now()
	os.Chtimes(b.l.blob(hash), now, now)
}

type blobInfo struct {
	path  string
	size  int64
	mtime time.Time
}

func (b *blobStore) list() ([]blobInfo, int64) {
	var out []blobInfo
	var total int64
	filepath.WalkDir(b.l.blobsDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == b.l.blobTmp() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, blobInfo{p, fi.Size(), fi.ModTime()})
		total += fi.Size()
		return nil
	})
	return out, total
}

// trim deletes least recently used blobs until the store is at most 90% of
// limit (hysteresis, so trimming does not run on every pass).
func (b *blobStore) trim(limit int64) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	if t := b.total.Load(); t >= 0 && t <= limit {
		return 0, nil
	}
	b.trimMu.Lock()
	defer b.trimMu.Unlock()
	blobs, total := b.list()
	b.total.Store(total)
	if total <= limit {
		return 0, nil
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].mtime.Before(blobs[j].mtime) })
	target := limit / 10 * 9
	n := 0
	var errs []error
	for _, bl := range blobs {
		if total <= target {
			break
		}
		if err := os.Remove(bl.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		total -= bl.size
		n++
	}
	b.total.Store(total)
	return n, errors.Join(errs...)
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
