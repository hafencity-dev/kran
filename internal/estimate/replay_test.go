package estimate

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestReplayJobHistory replays a recorded job history (`kran jobs -a
// --json`) through a fresh estimator in submission order: each job is
// estimated before its own run is observed, as the daemon does. It reports
// how the estimates compare with what the jobs used. Set
// KRAN_REPLAY_JOBS=<file> (and KRAN_REPLAY_CPUS, default 20) to run it.
func TestReplayJobHistory(t *testing.T) {
	path := os.Getenv("KRAN_REPLAY_JOBS")
	if path == "" {
		t.Skip("set KRAN_REPLAY_JOBS to a `kran jobs -a --json` file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []proto.Job
	if err := json.Unmarshal(b, &jobs); err != nil {
		t.Fatal(err)
	}
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].QueuedAt.Before(jobs[k].QueuedAt) })
	cpus := 20
	if v := os.Getenv("KRAN_REPLAY_CPUS"); v != "" {
		if err := json.Unmarshal([]byte(v), &cpus); err != nil {
			t.Fatal(err)
		}
	}
	self := ""
	if len(jobs) > 0 {
		self = jobs[0].Origin
	}
	e := NewWithOptions(nil, Options{Self: self})
	var online replayStats
	for i := range jobs {
		j := &jobs[i]
		fp := e.Fingerprint(&j.Spec)
		est := e.Estimate(&j.Spec, fp, cpus)
		u := j.Usage
		cmd := j.Spec.Shell
		if cmd == "" {
			cmd = strings.Join(j.Spec.Argv, " ")
		}
		if len(cmd) > 40 {
			cmd = cmd[:40]
		}
		cores := 0.0
		if u.WallSeconds > 0 {
			cores = u.CPUSeconds / u.WallSeconds
		}
		t.Logf("%-10s %-11s est %4.1f cpu %6.0f MiB %6.0f s %-16s | used %4.2f cpu %6.0f MiB %6.0f s | %s",
			j.State, family(j.Spec.Argv), est.CPUs, float64(est.Mem)/(1<<20), est.Duration, est.Source,
			cores, float64(u.PeakMem)/(1<<20), u.WallSeconds, cmd)
		online.add(j, est)
		if j.State == proto.JobSucceeded || j.State == proto.JobFailed {
			e.Observe(j)
		}
	}
	t.Logf("online, all runs:        %s", online.all)
	t.Logf("online, without flags:   %s", online.noFlag)

	// Leave one out: each job estimated from every other job of the
	// history except runs of its own fingerprint, i.e. a new command in a
	// repo with this history.
	var loo replayStats
	for i := range jobs {
		j := &jobs[i]
		e := NewWithOptions(nil, Options{Self: self})
		fp := e.Fingerprint(&j.Spec)
		for k := range jobs {
			o := &jobs[k]
			if k != i && e.Fingerprint(&o.Spec) != fp && (o.State == proto.JobSucceeded || o.State == proto.JobFailed) {
				e.Observe(o)
			}
		}
		loo.add(j, e.Estimate(&j.Spec, fp, cpus))
	}
	t.Logf("new commands, no flags:  %s", loo.noFlag)
}

type replayTotals struct {
	n, overCap                int
	allocCPU, usedCPU         float64
	allocMemGiBs, peakMemGiBs float64
	durErr                    []float64
	cpuRatio, memRatio        []float64 // per job: allocated / used
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func (r replayTotals) String() string {
	sort.Float64s(r.durErr)
	med := 0.0
	if len(r.durErr) > 0 {
		med = math.Exp(r.durErr[len(r.durErr)/2])
	}
	return fmt.Sprintf("%2d runs: cpu allocated %.1fx used (median job %.1fx), memory %.1fx the peak (median job %.1fx), %d over their memory cap, duration off by a median factor %.1f",
		r.n, r.allocCPU/math.Max(r.usedCPU, 1e-9), median(r.cpuRatio), r.allocMemGiBs/math.Max(r.peakMemGiBs, 1e-9), median(r.memRatio), r.overCap, med)
}

type replayStats struct{ all, noFlag replayTotals }

func (s *replayStats) add(j *proto.Job, est proto.Estimate) {
	u := j.Usage
	if u.WallSeconds <= 1 || (j.State != proto.JobSucceeded && j.State != proto.JobFailed) {
		return
	}
	for _, r := range []*replayTotals{&s.all, &s.noFlag} {
		if r == &s.noFlag && (j.Spec.Req.CPUs > 0 || j.Spec.Req.Mem > 0) {
			continue
		}
		r.n++
		r.allocCPU += est.CPUs * u.WallSeconds
		r.usedCPU += u.CPUSeconds
		r.allocMemGiBs += float64(est.Mem) / (1 << 30) * u.WallSeconds
		r.peakMemGiBs += float64(u.PeakMem) / (1 << 30) * u.WallSeconds
		if u.CPUSeconds > 0 {
			r.cpuRatio = append(r.cpuRatio, est.CPUs*u.WallSeconds/u.CPUSeconds)
		}
		if u.PeakMem > 0 {
			r.memRatio = append(r.memRatio, float64(est.Mem)/float64(u.PeakMem))
		}
		if u.PeakMem > 2*max(est.Mem, 1<<30) { // over the job's memory cap (SPEC §6.8)
			r.overCap++
		}
		if j.State == proto.JobSucceeded {
			r.durErr = append(r.durErr, math.Abs(math.Log(math.Max(est.Duration, 0.1)/u.WallSeconds)))
		}
	}
}
