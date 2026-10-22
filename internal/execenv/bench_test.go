package execenv

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// BenchmarkCommit30k: first materialization of a 30k-file manifest into an
// empty slot, and the no-op diff every later job of an unchanged repo pays.
func BenchmarkCommit30k(b *testing.B) {
	bs, err := newBlobStore(layout{state: b.TempDir()}, b.Logf)
	if err != nil {
		b.Fatal(err)
	}
	m := &proto.Manifest{Keep: []string{"node_modules/"}}
	for i := 0; i < 30000; i++ {
		c := fmt.Sprintf("file %d content %0100d", i, i)
		h := hashOf(c)
		bs.put(b.Context(), h, int64(len(c)), strings.NewReader(c))
		m.Entries = append(m.Entries, proto.ManifestEntry{Path: fmt.Sprintf("d%02d/s%02d/f%05d.txt", i%50, i%37, i), Mode: 0o644, Size: int64(len(c)), MTime: int64(i) * 1000, Hash: h})
	}
	b.Run("initial", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			slot := fmt.Sprintf("%s/slot%d", b.TempDir(), i)
			if _, err := commitLocal(slot, m, bs, 8, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	slot := b.TempDir() + "/slot"
	if _, err := commitLocal(slot, m, bs, 8, false); err != nil {
		b.Fatal(err)
	}
	b.Run("noop-diff", func(b *testing.B) {
		for b.Loop() {
			p, err := diffLocal(slot, m, false)
			if err != nil || p.changed() != 0 {
				b.Fatal(err)
			}
		}
	})
}
