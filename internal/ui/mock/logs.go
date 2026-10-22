package mock

import (
	"fmt"
	"math/rand/v2"
	"time"
)

// logBuf is a job's output: append-only bytes plus a broadcast channel that
// is closed (and replaced) on every append, so followers can wait cheaply.
type logBuf struct {
	data   []byte
	done   bool
	notify chan struct{}
}

const maxLog = 2 << 20

func (l *logBuf) init() { l.notify = make(chan struct{}) }

func (l *logBuf) append(_ time.Time, s string) {
	if l.done || len(l.data)+len(s) > maxLog {
		return
	}
	l.data = append(l.data, s...)
	close(l.notify)
	l.notify = make(chan struct{})
}

func (l *logBuf) close() {
	if l.done {
		return
	}
	l.done = true
	close(l.notify)
	l.notify = make(chan struct{})
}

// LogChunk returns output from offset, whether the job's output is complete,
// and a channel that is closed when more output (or the end) arrives.
func (s *Sim) LogChunk(id string, offset int) (chunk []byte, done bool, wait <-chan struct{}, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, false, nil, ErrNotFound
	}
	if offset < 0 || offset > len(j.log.data) {
		offset = len(j.log.data)
	}
	return append([]byte{}, j.log.data[offset:]...), j.log.done, j.log.notify, nil
}

const (
	green = "\x1b[32m"
	red   = "\x1b[31m"
	dim   = "\x1b[2m"
	bold  = "\x1b[1m"
	yel   = "\x1b[33m"
	cyan  = "\x1b[36m"
	reset = "\x1b[0m"
)

var goPkgs = []string{"indexplane", "indexplane/shard", "indexplane/merge", "indexplane/wal", "api", "auth", "store/pg", "queue", "search/rank", "render"}
var tsFiles = []string{"src/components/Upload.test.tsx", "src/lib/retry.test.ts", "src/routes/project.test.tsx", "src/hooks/useAutosave.test.ts", "src/lib/diff.test.ts", "src/components/Editor.test.tsx"}

func logLines(k logKind, r *rand.Rand, frac float64) string {
	switch k {
	case logGoTest:
		p := goPkgs[r.IntN(len(goPkgs))]
		if r.IntN(5) == 0 {
			return fmt.Sprintf("?   \tgithub.com/acme/app/internal/%s\t[no test files]\r\n", p)
		}
		return fmt.Sprintf("%sok%s  \tgithub.com/acme/app/internal/%s\t%.3fs\r\n", green, reset, p, 0.2+r.Float64()*6)
	case logVitest:
		f := tsFiles[r.IntN(len(tsFiles))]
		return fmt.Sprintf(" %s✓%s %s %s(%d tests)%s %dms\r\n", green, reset, f, dim, 3+r.IntN(20), reset, 20+r.IntN(900))
	case logTsc:
		return fmt.Sprintf("%s[%s]%s checking %d files\r\n", dim, time.Duration(frac*20*float64(time.Second)).Round(time.Second), reset, 400+r.IntN(1800))
	case logNext:
		steps := []string{"Creating an optimized production build ...", "Compiled successfully", "Linting and checking validity of types ...", "Collecting page data ...", "Generating static pages (%d/48)", "Finalizing page optimization ..."}
		i := min(len(steps)-1, int(frac*float64(len(steps))))
		line := steps[i]
		if i == 4 {
			line = fmt.Sprintf(line, 1+int(frac*47))
		}
		return fmt.Sprintf("   %s▲%s %s\r\n", cyan, reset, line)
	case logDocker:
		step := 1 + int(frac*9)
		cmds := []string{"FROM golang:1.26-bookworm AS build", "WORKDIR /src", "COPY go.mod go.sum ./", "RUN go mod download", "COPY . .", "RUN go build -o /out/api ./cmd/api", "FROM gcr.io/distroless/base-debian12", "COPY --from=build /out/api /api", "exporting layers"}
		return fmt.Sprintf("#%d [build %d/9] %s\r\n#%d %s%.1fs%s\r\n", step+4, step, cmds[min(step-1, 8)], step+4, dim, r.Float64()*14, reset)
	case logBench:
		s := int(frac * 1000)
		return fmt.Sprintf("step %4d/1000  loss=%.4f  tok/s=%d  gpu_mem=%.1fGiB\r\n", s, 2.1-frac*1.6+r.Float64()*0.05, 17000+r.IntN(3000), 18+r.Float64()*3)
	}
	return fmt.Sprintf("%schecked%s %d packages\r\n", dim, reset, 10+r.IntN(80))
}

func setupLog(t *template) string {
	return "Lockfile is up to date, resolution step is skipped\r\nProgress: resolved 1342, reused 1342, downloaded 0, added 1342, done\r\n" +
		dim + "dependencies: +214 -0" + reset + "\r\nDone in 6.2s using pnpm v10.18.1\r\n"
}

func failTail(k logKind) string {
	switch k {
	case logGoTest:
		return "--- FAIL: TestShardMerge/overlapping_ranges (0.04s)\r\n    merge_test.go:118: got 41 segments, want 40\r\n" +
			red + "FAIL" + reset + "\tgithub.com/acme/app/internal/indexplane/merge\t1.882s\r\n" + red + "FAIL" + reset + "\r\n"
	case logVitest, logTsc:
		return " " + red + "×" + reset + " src/lib/retry.test.ts > retries with backoff\r\n   " + red + "AssertionError: expected 3 to be 4" + reset + "\r\n\r\n " + bold + "Test Files" + reset + "  " + red + "1 failed" + reset + " | 23 passed (24)\r\n"
	}
	return red + "error:" + reset + " exit status 1\r\n"
}

func okTail(k logKind, wall float64) string {
	switch k {
	case logVitest:
		return fmt.Sprintf("\r\n %sTest Files%s  %s24 passed%s (24)\r\n      %sTests%s  %s312 passed%s (312)\r\n   Duration  %.2fs\r\n", bold, reset, green, reset, bold, reset, green, reset, wall)
	case logNext:
		return "\r\nRoute (app)                              Size     First Load JS\r\n┌ ○ /                                    5.2 kB         112 kB\r\n└ ○ /projects/[id]                       18.4 kB        141 kB\r\n"
	case logDocker:
		return "#14 naming to docker.io/acme/api:dev done\r\n"
	case logBench:
		return yel + "done:" + reset + " 1000 steps, mean tok/s 18942\r\n"
	}
	return ""
}
