package scheduler

import (
	"fmt"
	"testing"
)

// BenchmarkDispatch measures one pass over a deep queue: it runs on every
// event (node metrics every 2 s per node, submits, releases).
func BenchmarkDispatch(b *testing.B) {
	for _, n := range []int{50, 500, 2000} {
		b.Run(fmt.Sprintf("queued=%d", n), func(b *testing.B) {
			clk := &fakeClock{t0}
			s := New(Options{Origin: "gx10", Now: clk.Now}).(*sched)
			s.UpdateNode(self(gx10(t0)))
			s.UpdateNode(devmac(t0))
			for i := 0; i < n+40; i++ {
				s.Submit(newJob(fmt.Sprintf("j%05d", i), owners[i%len(owners)], 4, 6*gib, 120))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.Kick()
			}
		})
	}
}

func BenchmarkSnapshot(b *testing.B) {
	clk := &fakeClock{t0}
	s := New(Options{Origin: "gx10", Now: clk.Now}).(*sched)
	s.UpdateNode(self(gx10(t0)))
	s.UpdateNode(devmac(t0))
	for i := 0; i < 500; i++ {
		s.Submit(newJob(fmt.Sprintf("j%05d", i), owners[i%len(owners)], 4, 6*gib, 120))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Snapshot()
	}
}
