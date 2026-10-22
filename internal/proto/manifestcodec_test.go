package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func sampleManifest(n int) *Manifest {
	m := &Manifest{RepoID: "repo1", Root: "/work/proj", Keep: []string{"node_modules/", ".venv/"}}
	for i := 0; i < n; i++ {
		m.Entries = append(m.Entries, ManifestEntry{
			Path: fmt.Sprintf("src/pkg%04d/file%05d.go", i/10, i), Mode: 0o644,
			Size: int64(i * 3), MTime: 1_700_000_000_000_000_000 + int64(i), Hash: hashHex(fmt.Sprint(i)),
		})
	}
	m.Entries = append(m.Entries,
		ManifestEntry{Path: "zlink", Kind: EntrySymlink, Mode: 0o777, MTime: -5, Target: "src/pkg0000/file00000.go"},
		ManifestEntry{Path: "zz/empty", Kind: EntryDir, Mode: 0o755, MTime: 7})
	return m
}

func TestManifestCodecRoundTrip(t *testing.T) {
	for _, m := range []*Manifest{{}, sampleManifest(1), sampleManifest(500)} {
		var b bytes.Buffer
		if err := EncodeManifest(&b, m); err != nil {
			t.Fatal(err)
		}
		got, err := DecodeManifest(bytes.NewReader(b.Bytes()), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Entries) == 0 {
			got.Entries = nil
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("round trip differs:\n got %+v\nwant %+v", got, m)
		}
		if ManifestHash(got) != ManifestHash(m) || ManifestHash(m) == "" {
			t.Fatal("hash differs after a round trip")
		}
	}
}

func TestManifestHashSensitivity(t *testing.T) {
	base := sampleManifest(20)
	h := ManifestHash(base)
	mods := []func(m *Manifest){
		func(m *Manifest) { m.Root = "/other" },
		func(m *Manifest) { m.RepoID = "repo2" },
		func(m *Manifest) { m.Keep = nil },
		func(m *Manifest) { m.Entries[3].MTime++ },
		func(m *Manifest) { m.Entries[3].Mode = 0o755 },
		func(m *Manifest) { m.Entries[3].Hash = hashHex("x") },
		func(m *Manifest) { m.Entries = m.Entries[1:] },
		func(m *Manifest) { m.Entries[len(m.Entries)-2].Target = "src" },
	}
	for i, f := range mods {
		m := sampleManifest(20)
		f(m)
		if ManifestHash(m) == h {
			t.Errorf("modification %d kept the hash", i)
		}
	}
	// Order does not matter for the hash (it sorts a copy).
	m := sampleManifest(20)
	m.Entries[0], m.Entries[5] = m.Entries[5], m.Entries[0]
	if ManifestHash(m) != h {
		t.Error("hash depends on entry order")
	}
	if m.Entries[0].Path != base.Entries[5].Path {
		t.Error("ManifestHash reordered the caller's entries")
	}
}

func TestManifestCodecRejects(t *testing.T) {
	m := sampleManifest(3)
	m.Entries[1], m.Entries[2] = m.Entries[2], m.Entries[1]
	if err := EncodeManifest(&bytes.Buffer{}, m); err != ErrUnsorted {
		t.Fatalf("unsorted: %v", err)
	}
	m = sampleManifest(3)
	m.Entries[1].Path = m.Entries[0].Path
	if err := EncodeManifest(&bytes.Buffer{}, m); err != ErrUnsorted {
		t.Fatalf("duplicate: %v", err)
	}
	m = sampleManifest(3)
	m.Entries[0].Hash = strings.ToUpper(m.Entries[0].Hash)
	if err := EncodeManifest(&bytes.Buffer{}, m); err == nil {
		t.Fatal("uppercase hash encoded (would not round trip)")
	}

	var b bytes.Buffer
	EncodeManifest(&b, sampleManifest(50))
	enc := b.Bytes()
	if _, err := DecodeManifest(bytes.NewReader(enc), 10); err == nil {
		t.Fatal("entry limit not enforced")
	}
	if _, err := DecodeManifest(bytes.NewReader(append(append([]byte(nil), enc...), 0)), 1000); err == nil {
		t.Fatal("trailing data accepted")
	}
	// Every truncation fails cleanly.
	for i := 0; i < len(enc); i += 7 {
		if _, err := DecodeManifest(bytes.NewReader(enc[:i]), 1000); err == nil {
			t.Fatalf("truncated at %d accepted", i)
		}
	}
	// Corruption never panics.
	for i := 4; i < len(enc); i += 3 {
		c := append([]byte(nil), enc...)
		c[i] ^= 0x5a
		DecodeManifest(bytes.NewReader(c), 1000)
	}
}

func TestManifestDelta(t *testing.T) {
	base := sampleManifest(100)
	next := sampleManifest(100)
	next.Root = "/work/other-worktree"
	next.Entries = append(next.Entries[:10], next.Entries[12:]...) // two removed
	next.Entries[20].Hash = hashHex("changed")
	next.Entries[21].MTime++
	next.Entries = append(next.Entries, ManifestEntry{Path: "zzz/new.txt", Mode: 0o600, Size: 1, Hash: hashHex("n")})
	next.Entries = append([]ManifestEntry{{Path: "aaa", Mode: 0o644, Hash: hashHex("a")}}, next.Entries...)

	bh := ManifestHash(base)
	d, err := DiffManifests(bh, base, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Remove) != 2 || len(d.Upsert) != 4 {
		t.Fatalf("delta: %d removed, %d upserted", len(d.Remove), len(d.Upsert))
	}
	var b bytes.Buffer
	if err := EncodeDelta(&b, d); err != nil {
		t.Fatal(err)
	}
	d2, err := DecodeDelta(bytes.NewReader(b.Bytes()), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Base != bh {
		t.Fatal("base hash lost")
	}
	got, err := d2.Apply(base)
	if err != nil {
		t.Fatal(err)
	}
	if ManifestHash(got) != ManifestHash(next) || !reflect.DeepEqual(got, next) {
		t.Fatal("applied delta differs from the new manifest")
	}
	if ManifestHash(base) != bh {
		t.Fatal("Apply modified the base")
	}

	// A delta against the wrong base fails or yields another hash.
	other := sampleManifest(5)
	if m, err := d2.Apply(other); err == nil && ManifestHash(m) == ManifestHash(next) {
		t.Fatal("delta applied to the wrong base produced the right manifest")
	}

	// No change: empty delta.
	same, _ := DiffManifests(bh, base, sampleManifest(100))
	if len(same.Remove)+len(same.Upsert) != 0 {
		t.Fatal("unchanged manifest gives a non-empty delta")
	}
}

func TestDeltaApplyRejectsInconsistency(t *testing.T) {
	base := sampleManifest(3)
	d := &Delta{Remove: []string{"nope"}}
	if _, err := d.Apply(base); err == nil {
		t.Fatal("removal of an unknown path accepted")
	}
	d = &Delta{Remove: []string{base.Entries[0].Path}, Upsert: []ManifestEntry{base.Entries[0]}}
	if _, err := d.Apply(base); err == nil {
		t.Fatal("remove and update of one path accepted")
	}
}

func BenchmarkManifestCodec30k(b *testing.B) {
	m := sampleManifest(30000)
	var buf bytes.Buffer
	if err := EncodeManifest(&buf, m); err != nil {
		b.Fatal(err)
	}
	b.Run("encode", func(b *testing.B) {
		for b.Loop() {
			var w bytes.Buffer
			EncodeManifest(&w, m)
		}
	})
	b.Run("decode", func(b *testing.B) {
		for b.Loop() {
			DecodeManifest(bytes.NewReader(buf.Bytes()), 1<<20)
		}
	})
	b.ReportMetric(float64(buf.Len()), "bytes")
}
