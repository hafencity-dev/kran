package estimate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

// The repo-family tier sits between a fingerprint's own history and the
// static family defaults: what jobs of one command family in one repo
// really used. Agents rarely repeat a command exactly (another package
// list, another -run regex), so most fingerprints have no history, while
// "go test in this repo" has plenty.
const (
	groupKV = "estimate.repo_family.v1"
	// groupWindow is how many recent runs a group keeps; groupMax how many
	// groups are kept (least recently updated dropped). Together they bound
	// the KV value to well under 100 KB.
	groupWindow = 24
	groupMax    = 64
	// groupMinRuns runs are needed before a quantity replaces the static
	// default; the default stays in the blend worth groupPriorWeight runs.
	groupMinRuns     = 3
	groupPriorWeight = 2.0
	// For memory the default is worth more runs: undersizing memory kills
	// a job (its cap), oversizing only delays the next one.
	memPriorWeight = 5.0
	// Memory: a high quantile of the peaks plus headroom, and at least
	// memOfMax of the largest peak in the window: the job's cap is twice
	// the estimate (SPEC §6.8), so every peak seen stays 20% under it.
	memHeadroom = 1.25
	memOfMax    = 0.6
	minGroupMem = 256 << 20
	// A failed run longer than failedHangFactor × the group's usual
	// duration hung or hit a timeout: it teaches memory only.
	failedHangFactor = 4.0
	// CPU: the typical average core use plus headroom, at most a little
	// above the most any run used. CPU is shared, not exhausted: a job that
	// gets less runs slower but does not fail. Container jobs run under a
	// --cpus cap, though: a run that used most of its cap may have wanted
	// more, so it counts as cpuCappedGrowth × its use.
	cpuHeadroom     = 1.5
	cpuOfMax        = 1.2
	cpuCappedShare  = 0.85
	cpuCappedGrowth = 1.5
	minGroupCPUs    = 0.5
	// Runs shorter than this say little about CPU use (process start-up).
	minCPUWall = 1.0
)

// groupRun is one observed run, stored compactly.
type groupRun struct {
	// W is the wall time in origin seconds; 0 when the run is not
	// representative of the duration (failed early).
	W float64 `json:"w,omitempty"`
	// C is the average number of cores used; 0 when unknown.
	C float64 `json:"c,omitempty"`
	// Capped: C was bound by the job's CPU cap.
	Capped bool  `json:"k,omitempty"`
	M      int64 `json:"m,omitempty"` // peak memory
}

type group struct {
	Family string     `json:"f"`
	Runs   []groupRun `json:"r"` // oldest first, ≤ groupWindow
	Seq    uint64     `json:"s"` // update order, for eviction
}

// groupID keys the tier by repo and family variant. Hashed: repo IDs of
// non-git directories contain paths.
func groupID(repo, variant string) string {
	if repo == "" || variant == "" {
		return ""
	}
	h := sha256.Sum256([]byte(repo + "\x00" + variant))
	return hex.EncodeToString(h[:8])
}

// variantOf refines the family where one flag changes the resource profile
// by a large factor: the race detector multiplies memory and time.
func variantOf(fam string, words []string) string {
	if fam == "go-test" || fam == "go-build" {
		if hasFlag(words, "-race") {
			return fam + "+race"
		}
	}
	return fam
}

// groupEst is what a group says, per quantity (zero = not enough runs).
type groupEst struct {
	runs     int
	duration float64
	cpus     float64
	mem      int64
}

// estimate blends the group's measurements with the static default d.
func (g *group) estimate(d familyDefault) groupEst {
	var ge groupEst
	if g == nil {
		return ge
	}
	ge.runs = len(g.Runs)
	var durs, cpus []float64
	var mems []int64
	for _, r := range g.Runs {
		if r.W > 0 {
			durs = append(durs, r.W)
		}
		if r.C > 0 {
			c := r.C
			if r.Capped {
				c *= cpuCappedGrowth
			}
			cpus = append(cpus, c)
		}
		if r.M > 0 {
			mems = append(mems, r.M)
		}
	}
	if n := len(durs); n >= groupMinRuns {
		ge.duration = blendLog(robustMean(durs), d.Duration, n, groupPriorWeight)
	}
	if n := len(cpus); n >= groupMinRuns {
		c := math.Min(robustMean(cpus)*cpuHeadroom, quantile(cpus, 1)*cpuOfMax)
		ge.cpus = math.Max(minGroupCPUs, blendLog(c, d.CPUs, n, groupPriorWeight))
	}
	if n := len(mems); n >= groupMinRuns {
		f := make([]float64, n)
		for i, m := range mems {
			f[i] = float64(m)
		}
		// Few runs: the largest seen (safe while little is known); from
		// ten runs p90, so one outlier does not set the allocation, though
		// the floor keeps it under the cap.
		top := quantile(f, 1)
		q := top
		if n >= 10 {
			q = quantile(f, 0.9)
		}
		learned := math.Max(q*memHeadroom, top*memOfMax)
		// Blending only ever raises it: memory must not be undersized.
		m := math.Max(learned, blendLog(learned, float64(d.Mem), n, memPriorWeight))
		ge.mem = max(minGroupMem, int64(m))
	}
	return ge
}

// blendLog mixes a learned value from n runs with a prior worth k runs, in
// log space.
func blendLog(learned, prior float64, n int, k float64) float64 {
	if prior <= 0 {
		return learned
	}
	if learned <= 0 {
		return prior
	}
	w := float64(n) / (float64(n) + k)
	return math.Exp(w*math.Log(learned) + (1-w)*math.Log(prior))
}

// robustMean is the mean with the outer 10% on each side clamped to the
// p10/p90 values (the median below five values): a canceled-looking
// 25-minute run or a cached 0.1 s run moves it only a little.
func robustMean(v []float64) float64 {
	if len(v) < 5 {
		return quantile(v, 0.5)
	}
	lo, hi := quantile(v, 0.1), quantile(v, 0.9)
	t := 0.0
	for _, x := range v {
		t += math.Min(hi, math.Max(lo, x))
	}
	return t / float64(len(v))
}

// quantile is the nearest-rank p-quantile (p in (0,1]) of v.
func quantile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	return s[min(len(s)-1, max(0, idx))]
}

// addGroupRunLocked records r in group id. Callers hold e.mu.
func (e *estimator) addGroupRunLocked(id, variant string, r groupRun) {
	if id == "" || r.W <= 0 && r.C <= 0 && r.M <= 0 {
		return
	}
	g := e.groups[id]
	if g == nil {
		if len(e.groups) >= groupMax {
			e.evictGroupLocked()
		}
		g = &group{Family: variant}
		e.groups[id] = g
	}
	g.Runs = append(g.Runs, r)
	if len(g.Runs) > groupWindow {
		g.Runs = append([]groupRun(nil), g.Runs[len(g.Runs)-groupWindow:]...)
	}
	e.groupSeq++
	g.Seq = e.groupSeq
	e.groupsDirty = true
}

func (e *estimator) evictGroupLocked() {
	var oldest string
	var at uint64 = math.MaxUint64
	for k, g := range e.groups {
		if g.Seq < at || g.Seq == at && k < oldest {
			oldest, at = k, g.Seq
		}
	}
	delete(e.groups, oldest)
}

// saveGroups persists the tier when it changed.
func (e *estimator) saveGroups(ctx context.Context) {
	e.saveMu.Lock()
	defer e.saveMu.Unlock()
	e.mu.Lock()
	var b []byte
	var err error
	if e.groupsDirty {
		e.groupsDirty = false
		b, err = json.Marshal(e.groups)
	}
	e.mu.Unlock()
	if err != nil || b == nil {
		return
	}
	if err := e.st.Set(ctx, groupKV, string(b)); err != nil {
		e.opts.Logf("estimate: save repo-family history: %v", err)
	}
}

func (e *estimator) loadGroups() {
	if e.st == nil {
		return
	}
	ctx, cancel := storeCtx()
	defer cancel()
	v, ok, err := e.st.Get(ctx, groupKV)
	if err != nil || !ok {
		return
	}
	var m map[string]*group
	if json.Unmarshal([]byte(v), &m) != nil {
		e.opts.Logf("estimate: ignoring unreadable repo-family history")
		return
	}
	for k, g := range m {
		if g == nil || len(g.Runs) == 0 {
			delete(m, k)
			continue
		}
		if len(g.Runs) > groupWindow {
			g.Runs = g.Runs[len(g.Runs)-groupWindow:]
		}
		e.groupSeq = max(e.groupSeq, g.Seq)
	}
	e.groups = m
	for len(e.groups) > groupMax {
		e.evictGroupLocked()
	}
}
