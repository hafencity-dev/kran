package proto

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Compact manifest encoding (peer protocol 1.1, SPEC §8.3). JSON spends
// ~150 bytes and a reflective decode per entry; this spends ~45 bytes
// (paths share their prefix with the previous entry, hashes are raw) and
// decodes an order of magnitude faster. The encoding is canonical: one
// manifest has exactly one encoding, so its SHA-256 (ManifestHash) names
// the manifest, and a delta (ManifestDelta) can refer to a base by hash.
//
//	manifest: "KMF1" | str repo_id | str root | uv n_keep | str keep… |
//	          uv n | entry…            (entries strictly sorted by path)
//	entry:    uv shared_prefix | str path_suffix | u8 kind | u8 flags |
//	          uv mode | v size | v mtime | [32 hash] | [str target]
//	          flags: 1 = hash present, 2 = target present
//	delta:    "KMD1" | 32 base | str repo_id | str root | uv n_keep |
//	          str keep… | uv n_remove | path… | uv n_upsert | entry…
//	          (removed paths and upserts each strictly sorted, prefix-
//	          compressed like manifest paths)
//
// uv = unsigned varint, v = signed (zigzag) varint, str = uv length + bytes.

// Manifest wire formats (slot.diff body "format").
const (
	ManifestJSON  = "json" // zstd JSON (protocol 1.0)
	ManifestKMF1  = "kmf1" // zstd compact encoding of the whole manifest
	ManifestDelta = "kmd1" // zstd compact delta against a base manifest
	ManifestSame  = "same" // nothing: the base manifest is unchanged
)

const (
	kmfMagic = "KMF1"
	kmdMagic = "KMD1"

	flagHash   = 1
	flagTarget = 2

	maxCodecString = 1 << 20 // a path, root, keep pattern or link target
)

// ErrUnsorted: the manifest's entries are not strictly sorted by path
// (Scan sorts them; duplicates are never valid).
var ErrUnsorted = errors.New("manifest entries not strictly sorted by path")

// SortedByPath reports whether entries are strictly increasing by path.
func SortedByPath(entries []ManifestEntry) bool {
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path >= entries[i].Path {
			return false
		}
	}
	return true
}

type codecWriter struct {
	w   *bufio.Writer
	buf [binary.MaxVarintLen64]byte
	err error
}

func (c *codecWriter) uv(v uint64) {
	c.w.Write(c.buf[:binary.PutUvarint(c.buf[:], v)])
}

func (c *codecWriter) v(v int64) {
	c.w.Write(c.buf[:binary.PutVarint(c.buf[:], v)])
}

func (c *codecWriter) str(s string) {
	c.uv(uint64(len(s)))
	c.w.WriteString(s)
}

func (c *codecWriter) header(repo, root string, keep []string) {
	c.str(repo)
	c.str(root)
	c.uv(uint64(len(keep)))
	for _, k := range keep {
		c.str(k)
	}
}

func sharedPrefix(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func (c *codecWriter) path(prev, p string) {
	k := sharedPrefix(prev, p)
	c.uv(uint64(k))
	c.str(p[k:])
}

func (c *codecWriter) entry(prev string, e *ManifestEntry) {
	if c.err != nil {
		return
	}
	c.path(prev, e.Path)
	var flags byte
	var h [32]byte
	if e.Hash != "" {
		if !decodeHash(h[:], e.Hash) {
			c.err = fmt.Errorf("%s: hash %q is not lowercase sha256 hex", e.Path, e.Hash)
			return
		}
		flags |= flagHash
	}
	if e.Target != "" {
		flags |= flagTarget
	}
	c.w.WriteByte(byte(e.Kind))
	c.w.WriteByte(flags)
	c.uv(uint64(e.Mode))
	c.v(e.Size)
	c.v(e.MTime)
	if flags&flagHash != 0 {
		c.w.Write(h[:])
	}
	if flags&flagTarget != 0 {
		c.str(e.Target)
	}
}

// decodeHash decodes 64 lowercase hex digits into dst (only those round
// trip through the encoding).
func decodeHash(dst []byte, s string) bool {
	if len(s) != 2*len(dst) {
		return false
	}
	val := func(c byte) (byte, bool) {
		switch {
		case c >= '0' && c <= '9':
			return c - '0', true
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10, true
		}
		return 0, false
	}
	for i := range dst {
		hi, ok1 := val(s[2*i])
		lo, ok2 := val(s[2*i+1])
		if !ok1 || !ok2 {
			return false
		}
		dst[i] = hi<<4 | lo
	}
	return true
}

// EncodeManifest writes the compact encoding of m. Its entries must be
// strictly sorted by path.
func EncodeManifest(w io.Writer, m *Manifest) error {
	if !SortedByPath(m.Entries) {
		return ErrUnsorted
	}
	c := &codecWriter{w: bufio.NewWriterSize(w, 64<<10)}
	c.w.WriteString(kmfMagic)
	c.header(m.RepoID, m.Root, m.Keep)
	c.uv(uint64(len(m.Entries)))
	prev := ""
	for i := range m.Entries {
		c.entry(prev, &m.Entries[i])
		prev = m.Entries[i].Path
	}
	if c.err != nil {
		return c.err
	}
	return c.w.Flush()
}

// ManifestHash returns the hex SHA-256 of m's compact encoding, sorting a
// copy of the entries first when needed. "" if m cannot be encoded.
func ManifestHash(m *Manifest) string {
	if !SortedByPath(m.Entries) {
		cp := *m
		cp.Entries = append([]ManifestEntry(nil), m.Entries...)
		sort.Slice(cp.Entries, func(i, j int) bool { return cp.Entries[i].Path < cp.Entries[j].Path })
		m = &cp
	}
	h := sha256.New()
	if EncodeManifest(h, m) != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

type codecReader struct {
	r *bufio.Reader
}

func (c *codecReader) uv() (uint64, error) { return binary.ReadUvarint(c.r) }
func (c *codecReader) v() (int64, error)   { return binary.ReadVarint(c.r) }

func (c *codecReader) str() (string, error) {
	n, err := c.uv()
	if err != nil {
		return "", err
	}
	if n > maxCodecString {
		return "", fmt.Errorf("manifest string of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func (c *codecReader) magic(want string) error {
	var b [4]byte
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return err
	}
	if string(b[:]) != want {
		return fmt.Errorf("not a %s stream", want)
	}
	return nil
}

func (c *codecReader) header() (repo, root string, keep []string, err error) {
	if repo, err = c.str(); err != nil {
		return
	}
	if root, err = c.str(); err != nil {
		return
	}
	var n uint64
	if n, err = c.uv(); err != nil {
		return
	}
	if n > 4096 {
		err = fmt.Errorf("manifest with %d keep patterns", n)
		return
	}
	for i := uint64(0); i < n; i++ {
		var k string
		if k, err = c.str(); err != nil {
			return
		}
		keep = append(keep, k)
	}
	return
}

func (c *codecReader) count(max int) (int, error) {
	n, err := c.uv()
	if err != nil {
		return 0, err
	}
	if n > uint64(max) {
		return 0, fmt.Errorf("manifest of %d entries exceeds %d", n, max)
	}
	return int(n), nil
}

// path reads a prefix-compressed path that must sort after prev.
func (c *codecReader) path(prev string) (string, error) {
	k, err := c.uv()
	if err != nil {
		return "", err
	}
	if k > uint64(len(prev)) {
		return "", errors.New("manifest path prefix out of range")
	}
	suf, err := c.str()
	if err != nil {
		return "", err
	}
	p := prev[:k] + suf
	if p <= prev {
		return "", ErrUnsorted
	}
	return p, nil
}

func (c *codecReader) entry(prev string, e *ManifestEntry) error {
	p, err := c.path(prev)
	if err != nil {
		return err
	}
	e.Path = p
	kind, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	flags, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	e.Kind = EntryKind(kind)
	mode, err := c.uv()
	if err != nil {
		return err
	}
	if mode > 1<<32-1 {
		return fmt.Errorf("%s: mode out of range", p)
	}
	e.Mode = uint32(mode)
	if e.Size, err = c.v(); err != nil {
		return err
	}
	if e.MTime, err = c.v(); err != nil {
		return err
	}
	if flags&flagHash != 0 {
		var h [32]byte
		if _, err := io.ReadFull(c.r, h[:]); err != nil {
			return err
		}
		e.Hash = hex.EncodeToString(h[:])
	}
	if flags&flagTarget != 0 {
		if e.Target, err = c.str(); err != nil {
			return err
		}
	}
	return nil
}

func (c *codecReader) end() error {
	if _, err := c.r.ReadByte(); err != io.EOF {
		return errors.New("trailing data after manifest")
	}
	return nil
}

// DecodeManifest reads a compact manifest of at most maxEntries entries
// (paths strictly sorted, which also rules out duplicates).
func DecodeManifest(r io.Reader, maxEntries int) (*Manifest, error) {
	c := &codecReader{r: bufio.NewReaderSize(r, 64<<10)}
	if err := c.magic(kmfMagic); err != nil {
		return nil, err
	}
	m := &Manifest{}
	var err error
	if m.RepoID, m.Root, m.Keep, err = c.header(); err != nil {
		return nil, fmt.Errorf("manifest header: %w", err)
	}
	n, err := c.count(maxEntries)
	if err != nil {
		return nil, err
	}
	m.Entries = make([]ManifestEntry, n)
	prev := ""
	for i := range m.Entries {
		if err := c.entry(prev, &m.Entries[i]); err != nil {
			return nil, fmt.Errorf("manifest entry %d: %w", i, err)
		}
		prev = m.Entries[i].Path
	}
	if err := c.end(); err != nil {
		return nil, err
	}
	return m, nil
}

// Delta is a manifest expressed against a base manifest the receiver
// already holds: the header in full, then removed paths and new or changed
// entries.
type Delta struct {
	Base   string // ManifestHash of the base
	RepoID string
	Root   string
	Keep   []string
	Remove []string
	Upsert []ManifestEntry
}

// DiffManifests returns next as a delta against base (both sorted).
func DiffManifests(baseHash string, base, next *Manifest) (*Delta, error) {
	if !SortedByPath(base.Entries) || !SortedByPath(next.Entries) {
		return nil, ErrUnsorted
	}
	d := &Delta{Base: baseHash, RepoID: next.RepoID, Root: next.Root, Keep: next.Keep}
	a, b := base.Entries, next.Entries
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j == len(b) || (i < len(a) && a[i].Path < b[j].Path):
			d.Remove = append(d.Remove, a[i].Path)
			i++
		case i == len(a) || b[j].Path < a[i].Path:
			d.Upsert = append(d.Upsert, b[j])
			j++
		default:
			if a[i] != b[j] {
				d.Upsert = append(d.Upsert, b[j])
			}
			i++
			j++
		}
	}
	return d, nil
}

// EncodeDelta writes the compact encoding of d.
func EncodeDelta(w io.Writer, d *Delta) error {
	var base [32]byte
	if !decodeHash(base[:], d.Base) {
		return fmt.Errorf("delta base %q is not a manifest hash", d.Base)
	}
	if !sort.StringsAreSorted(d.Remove) || !SortedByPath(d.Upsert) {
		return ErrUnsorted
	}
	c := &codecWriter{w: bufio.NewWriterSize(w, 64<<10)}
	c.w.WriteString(kmdMagic)
	c.w.Write(base[:])
	c.header(d.RepoID, d.Root, d.Keep)
	c.uv(uint64(len(d.Remove)))
	prev := ""
	for _, p := range d.Remove {
		c.path(prev, p)
		prev = p
	}
	c.uv(uint64(len(d.Upsert)))
	prev = ""
	for i := range d.Upsert {
		c.entry(prev, &d.Upsert[i])
		prev = d.Upsert[i].Path
	}
	if c.err != nil {
		return c.err
	}
	return c.w.Flush()
}

// DecodeDelta reads a compact delta with at most maxEntries removals and
// upserts each.
func DecodeDelta(r io.Reader, maxEntries int) (*Delta, error) {
	c := &codecReader{r: bufio.NewReaderSize(r, 64<<10)}
	if err := c.magic(kmdMagic); err != nil {
		return nil, err
	}
	var base [32]byte
	if _, err := io.ReadFull(c.r, base[:]); err != nil {
		return nil, err
	}
	d := &Delta{Base: hex.EncodeToString(base[:])}
	var err error
	if d.RepoID, d.Root, d.Keep, err = c.header(); err != nil {
		return nil, fmt.Errorf("delta header: %w", err)
	}
	n, err := c.count(maxEntries)
	if err != nil {
		return nil, err
	}
	prev := ""
	for i := 0; i < n; i++ {
		p, err := c.path(prev)
		if err != nil {
			return nil, fmt.Errorf("delta removal %d: %w", i, err)
		}
		d.Remove = append(d.Remove, p)
		prev = p
	}
	if n, err = c.count(maxEntries); err != nil {
		return nil, err
	}
	d.Upsert = make([]ManifestEntry, n)
	prev = ""
	for i := range d.Upsert {
		if err := c.entry(prev, &d.Upsert[i]); err != nil {
			return nil, fmt.Errorf("delta entry %d: %w", i, err)
		}
		prev = d.Upsert[i].Path
	}
	if err := c.end(); err != nil {
		return nil, err
	}
	return d, nil
}

// Apply returns the manifest d describes, given its base (sorted). The
// base is not modified. Removing a path the base lacks is an error: the
// two sides disagree about the base.
func (d *Delta) Apply(base *Manifest) (*Manifest, error) {
	if !SortedByPath(base.Entries) {
		return nil, ErrUnsorted
	}
	out := &Manifest{RepoID: d.RepoID, Root: d.Root, Keep: d.Keep,
		Entries: make([]ManifestEntry, 0, len(base.Entries)+len(d.Upsert)-len(d.Remove))}
	a, rm, up := base.Entries, d.Remove, d.Upsert
	for len(a) > 0 || len(up) > 0 {
		if len(rm) > 0 && (len(a) == 0 || rm[0] < a[0].Path) {
			return nil, fmt.Errorf("delta removes %q, which the base lacks", rm[0])
		}
		switch {
		case len(a) > 0 && len(rm) > 0 && rm[0] == a[0].Path:
			if len(up) > 0 && up[0].Path == a[0].Path {
				return nil, fmt.Errorf("delta both removes and updates %q", rm[0])
			}
			rm, a = rm[1:], a[1:]
		case len(up) == 0 || (len(a) > 0 && a[0].Path < up[0].Path):
			out.Entries = append(out.Entries, a[0])
			a = a[1:]
		case len(a) > 0 && a[0].Path == up[0].Path:
			out.Entries = append(out.Entries, up[0])
			a, up = a[1:], up[1:]
		default:
			out.Entries = append(out.Entries, up[0])
			up = up[1:]
		}
	}
	if len(rm) > 0 {
		return nil, fmt.Errorf("delta removes %q, which the base lacks", rm[0])
	}
	return out, nil
}

// EncodeManifestBytes is EncodeManifest into memory.
func EncodeManifestBytes(m *Manifest) ([]byte, error) {
	var b bytes.Buffer
	if err := EncodeManifest(&b, m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
