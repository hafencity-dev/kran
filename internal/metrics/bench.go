package metrics

import (
	"runtime"
	"sort"
	"sync/atomic"
	"time"
)

// Single-core benchmark for proto.NodeInfo.SingleCoreScore: a fixed mix of
// what compilers and test runners stress (hash-map updates, byte scanning
// with hashing, small sorts, integer mixing through an L1-sized table). A
// pure dependency-chain loop misranks cores (an M3 P-core scored below a
// Cortex-X925), so the mix matters more than any single kernel. It runs in
// short slices for ~200 ms and keeps the fastest slice, so a shared or busy
// machine (other processes stealing the core mid-slice) still reports the
// core's real speed.

const (
	benchSlices   = 10
	benchSliceDur = 20 * time.Millisecond
	// benchScale maps units/second to the score: a Cortex-X925 (GX10
	// performance core) scores 1000, a Cortex-A725 ~775, an Apple M3 Ultra
	// P-core ~930. It is only a prior; kran learns real per-job speed.
	benchScale = 1000.0 / benchCalib
	benchCalib = 278700
)

// benchSink defeats dead-code elimination of the workload.
var benchSink uint64

type benchState struct {
	tab [256]uint64
	m   map[uint32]uint32
	buf []byte
	arr []int
}

func newBenchState() *benchState {
	s := &benchState{m: make(map[uint32]uint32, 1024), buf: make([]byte, 512), arr: make([]int, 64)}
	for i := range s.tab {
		s.tab[i] = uint64(i)*0x2545F4914F6CDD1D + 1
	}
	return s
}

func xorshift(x uint64) uint64 {
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	return x
}

func benchUnit(s *benchState, x uint64) uint64 {
	for i := 0; i < 64; i++ {
		x = xorshift(x)
		k := uint32(x) & 1023
		s.m[k] += uint32(i)
		x += uint64(s.m[(k*7)&1023])
	}
	h := uint64(14695981039346656037)
	for i := range s.buf {
		s.buf[i] = byte(x >> (i & 31))
		if s.buf[i] == ' ' {
			h ^= 0xff
		}
		h = (h ^ uint64(s.buf[i])) * 1099511628211
	}
	for i := range s.arr {
		s.arr[i] = int((x ^ uint64(i)*2654435761) & 0xffff)
	}
	sort.Ints(s.arr)
	x ^= h ^ uint64(s.arr[7])
	for i := 0; i < 256; i++ {
		x = xorshift(x)
		j := x & 255
		v := s.tab[j]*0x9E3779B97F4A7C15 + x
		s.tab[j] = v
		x += s.tab[(j+1)&255] ^ (v >> 3)
	}
	return x
}

// runBenchmark returns the score. pin restricts the benchmark thread to
// performance cores where the OS allows it.
func runBenchmark(pin func()) float64 {
	res := make(chan float64, 1)
	go func() {
		// The thread is never unlocked: when this goroutine exits the
		// runtime discards it, so the affinity change cannot leak into
		// other goroutines.
		runtime.LockOSThread()
		if pin != nil {
			pin()
		}
		res <- benchRate(benchSlices, benchSliceDur)
	}()
	return benchScaleRate(<-res)
}

func benchScaleRate(unitsPerSec float64) float64 {
	score := unitsPerSec * benchScale
	return float64(int64(score*10+0.5)) / 10
}

// benchRate returns the best units/second over n slices of length d.
func benchRate(n int, d time.Duration) float64 {
	st := newBenchState()
	x := uint64(88172645463325252)
	best := 0.0
	for s := 0; s < n; s++ {
		start := time.Now()
		units := 0
		for {
			x = benchUnit(st, x)
			units++
			if units%16 == 0 {
				if el := time.Since(start); el >= d {
					if r := float64(units) / el.Seconds(); r > best {
						best = r
					}
					break
				}
			}
		}
	}
	atomic.AddUint64(&benchSink, x)
	return best
}
