package estimate

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/store"
)

// Learning parameters.
const (
	// window is how many recent runs per fingerprint feed the estimate.
	window = 20
	// alpha is the EWMA weight of the newest run.
	alpha = 0.3
	// priorWeight is how many runs the prior is worth when blending a
	// learned speed ratio with it.
	priorWeight = 2.0
	minSpeed    = 0.2
	maxSpeed    = 5.0
	minCPUs     = 0.1
	minMem      = 64 << 20
	cacheSize   = 2048
	// familySpeedKey persists per-(family, node) speed ratios in the store KV.
	familySpeedKey = "estimate.family_speed.v1"
)

// Options configure an estimator beyond New.
type Options struct {
	// Self is this node's name: the origin of every job this estimator
	// observes. Empty = taken from observed jobs' Origin.
	Self string
	// Score returns a node's SingleCoreScore (0 when unknown); it is the
	// speed prior before any run of a job was measured on that node.
	Score func(node string) float64
	Logf  func(format string, args ...any)
}

// NewWithOptions is New with node-aware priors. st may be nil (estimates are
// then kept in memory only).
func NewWithOptions(st store.Store, opts Options) Estimator {
	e := &estimator{st: st, opts: opts, cache: map[string]*fpEntry{}, famSpeed: map[string]map[string]*ewma{},
		groups: map[string]*group{}}
	if e.opts.Logf == nil {
		e.opts.Logf = func(string, ...any) {}
	}
	e.loadFamilySpeed()
	e.loadGroups()
	return e
}

func newEstimator(st store.Store) Estimator { return NewWithOptions(st, Options{}) }

type estimator struct {
	st   store.Store
	opts Options

	mu sync.Mutex
	// saveMu orders KV writes of the repo-family tier: the last write
	// holds the newest state.
	saveMu sync.Mutex
	cache  map[string]*fpEntry
	tick   uint64 // LRU clock
	// famSpeed[family][node] is the duration ratio node/origin learned
	// across all fingerprints of a family: a better prior for a new
	// fingerprint than the CPU benchmark.
	famSpeed map[string]map[string]*ewma
	self     string
	// groups is the repo-family tier (group.go), keyed by groupID.
	groups      map[string]*group
	groupSeq    uint64
	groupsDirty bool
}

type fpEntry struct {
	samples []store.Sample // newest first, ≤ window
	family  string         // from the last Estimate/Observe; prior lookup before any sample
	used    uint64
}

type ewma struct {
	V float64 `json:"v"`
	N int     `json:"n"`
}

func (w *ewma) add(x float64) {
	if w.N == 0 {
		w.V = x
	} else {
		w.V = alpha*x + (1-alpha)*w.V
	}
	if w.N < window {
		w.N++
	}
}

func (e *estimator) Family(argv []string) string            { return family(argv) }
func (e *estimator) Fingerprint(spec *proto.JobSpec) string { return fingerprint(spec) }

func (e *estimator) selfName() string {
	if e.opts.Self != "" {
		return e.opts.Self
	}
	return e.self
}

func storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

// entry returns the cached samples of fp, loading them from the store.
// Callers hold e.mu.
func (e *estimator) entry(fp string) *fpEntry {
	e.tick++
	if en, ok := e.cache[fp]; ok {
		en.used = e.tick
		return en
	}
	en := &fpEntry{used: e.tick}
	if e.st != nil && fp != "" {
		ctx, cancel := storeCtx()
		s, err := e.st.Samples(ctx, fp, window)
		cancel()
		if err != nil {
			e.opts.Logf("estimate: load samples: %v", err)
		}
		en.samples = s
	}
	if len(e.cache) >= cacheSize {
		e.evict()
	}
	e.cache[fp] = en
	return en
}

func (e *estimator) evict() {
	var oldest string
	var at uint64 = math.MaxUint64
	for k, v := range e.cache {
		if v.used < at {
			oldest, at = k, v.used
		}
	}
	delete(e.cache, oldest)
}

// Estimate: explicit requirements win for what they set (cpu, mem); every
// other quantity comes from the most specific history that has it: this
// fingerprint's runs, then the family's runs in this repo, then the
// command family's defaults scaled to nodeCPUs.
func (e *estimator) Estimate(spec *proto.JobSpec, fp string, nodeCPUs int) proto.Estimate {
	fam, words := specFamily(spec)
	est := e.familyEstimate(spec, fam, nodeCPUs)
	prior := familyDefault{CPUs: est.CPUs, Mem: est.Mem, Duration: est.Duration}
	durSource := est.Source

	if fp == "" {
		fp = fingerprint(spec)
	}
	variant := variantOf(fam, words)
	e.mu.Lock()
	en := e.entry(fp)
	en.family = fam
	learned := e.learn(en.samples, fam)
	ge := e.groups[e.groupIDOf(spec, fam, variant)].estimate(prior)
	if base, ok := strings.CutSuffix(variant, runVariant); ok && ge.duration == 0 && ge.cpus == 0 && ge.mem == 0 {
		// Too few filtered runs yet: the package's unfiltered runs say more
		// than the static default.
		variant, ge = base, e.groups[e.groupIDOf(spec, fam, base)].estimate(prior)
	}
	e.mu.Unlock()

	capCPUs := func(c float64) float64 {
		if nodeCPUs > 0 && c > float64(nodeCPUs) {
			return float64(nodeCPUs)
		}
		return c
	}
	if ge.duration > 0 || ge.cpus > 0 || ge.mem > 0 {
		est.Source = "repo:" + variant
		est.RepoSamples = ge.runs
		if ge.duration > 0 {
			est.Duration = ge.duration
			durSource = est.Source
		}
		if ge.cpus > 0 {
			est.CPUs = capCPUs(ge.cpus)
		}
		if ge.mem > 0 {
			est.Mem = ge.mem
		}
	}
	if learned.samples > 0 {
		est.Samples = learned.samples
		est.Source = "learned"
		if learned.haveRun {
			est.Duration = learned.duration
			durSource = "learned"
			est.CPUs = capCPUs(learned.cpus)
		}
		if learned.mem > 0 {
			est.Mem = learned.mem
		}
	}
	est.DurationSource = durSource

	explicit := false
	if spec.Req.CPUs > 0 {
		est.CPUs = spec.Req.CPUs
		explicit = true
	}
	if spec.Req.Mem > 0 {
		est.Mem = spec.Req.Mem
		explicit = true
	}
	if explicit {
		est.Source = "flag"
		if spec.Labels[LabelReqSource] == "rule" {
			est.Source = "rule"
		}
	}
	est.CPUs = math.Max(minCPUs, roundTo(est.CPUs, 0.1))
	if est.Mem < minMem {
		est.Mem = minMem
	}
	est.ResultOnly = resultOnly(spec)
	return est
}

// specFamily returns the job's family and the words it was derived from.
func specFamily(spec *proto.JobSpec) (string, []string) {
	if spec.Shell != "" {
		return shellFamily(spec.Shell), shellWords(spec.Shell)
	}
	return family(spec.Argv), spec.Argv
}

// groupIDOf is the repo-family group of a job; "" for the catch-all
// family, whose members have nothing in common.
func (e *estimator) groupIDOf(spec *proto.JobSpec, fam, variant string) string {
	if fam == "other" {
		return ""
	}
	return groupID(spec.Workspace.RepoID, variant)
}

// LabelReqSource is the job label the CLI sets to "rule" when Req.CPUs/Mem
// came from a .kran.toml rule rather than a flag (only changes Source).
const LabelReqSource = "kran.req_source"

func (e *estimator) familyEstimate(spec *proto.JobSpec, fam string, nodeCPUs int) proto.Estimate {
	d, ok := families[fam]
	if !ok {
		d = families["other"]
	}
	cpus := d.CPUs
	mem := d.Mem
	words := spec.Argv
	if spec.Shell != "" {
		words = shellWords(spec.Shell)
	}
	switch fam {
	case "make":
		if n := makeJobs(words); n > 0 {
			cpus = float64(n)
		}
	case "go-test":
		if hasFlag(words, "-race") {
			// The race detector multiplies memory use.
			mem = mem * 3 / 2
		}
	case "other":
		if spec.Req.GPU {
			// GPU jobs load models into (often unified) memory.
			cpus, mem = 2, 8*gib
		}
	}
	if nodeCPUs > 0 && cpus > float64(nodeCPUs) {
		cpus = float64(nodeCPUs)
	}
	return proto.Estimate{CPUs: cpus, Mem: mem, Duration: d.Duration, Source: "family:" + fam}
}

type learned struct {
	samples  int
	haveRun  bool // at least one complete run (wall time known)
	duration float64
	cpus     float64
	mem      int64
}

// learn derives the estimate from samples (newest first). Callers hold e.mu.
func (e *estimator) learn(samples []store.Sample, fam string) learned {
	var l learned
	if len(samples) == 0 {
		return l
	}
	l.samples = len(samples)
	var dur, cpu ewma
	var mems []int64
	for i := len(samples) - 1; i >= 0; i-- { // oldest → newest
		s := samples[i]
		if s.PeakMem > 0 {
			mems = append(mems, s.PeakMem)
		}
		if s.WallSeconds <= 0 {
			continue
		}
		sp := e.speedLocked(s.Fingerprint, s.Node, samples, fam)
		dur.add(s.WallSeconds / sp)
		if s.CPUSeconds > 0 {
			cpu.add(s.CPUSeconds / s.WallSeconds)
		}
	}
	if dur.N > 0 {
		l.haveRun = true
		l.duration = dur.V
		l.cpus = cpu.V
		if cpu.N == 0 {
			l.cpus = families["other"].CPUs
		}
	}
	l.mem = p90(mems)
	return l
}

func p90(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(0.9*float64(len(s)))) - 1
	return s[idx]
}

func roundTo(x, step float64) float64 { return math.Round(x/step) * step }

// Speed is the expected duration on node relative to the origin (self) for
// this fingerprint: learned per (fingerprint, node), blended with a prior
// (family-wide ratio for the node, else the single-core benchmark ratio).
func (e *estimator) Speed(fp, node string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if node == "" || node == e.selfName() {
		return 1
	}
	en := e.entry(fp)
	fam := en.family
	if fam == "" && len(en.samples) > 0 {
		fam = en.samples[0].Family
	}
	return e.speedLocked(fp, node, en.samples, fam)
}

func (e *estimator) speedLocked(fp, node string, samples []store.Sample, fam string) float64 {
	self := e.selfName()
	if self == "" && len(samples) > 0 {
		self = samples[0].Origin
	}
	if node == self || node == "" {
		return 1
	}
	prior := e.priorLocked(fam, node, self)
	var onNode, onSelf ewma
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if s.WallSeconds <= 0 {
			continue
		}
		switch s.Node {
		case node:
			onNode.add(s.WallSeconds)
		case self, "":
			onSelf.add(s.WallSeconds)
		}
	}
	if onNode.N == 0 || onSelf.N == 0 {
		return prior
	}
	ratio := clampSpeed(onNode.V / onSelf.V)
	w := float64(onNode.N) / (float64(onNode.N) + priorWeight)
	return clampSpeed(math.Exp(w*math.Log(ratio) + (1-w)*math.Log(prior)))
}

func (e *estimator) priorLocked(fam, node, self string) float64 {
	if fam != "" {
		if r := e.famSpeed[fam][node]; r != nil && r.N > 0 {
			return clampSpeed(r.V)
		}
	}
	if e.opts.Score != nil && self != "" {
		sn, ss := e.opts.Score(node), e.opts.Score(self)
		if sn > 0 && ss > 0 {
			// Higher score = faster = shorter duration.
			return clampSpeed(ss / sn)
		}
	}
	return 1
}

func clampSpeed(x float64) float64 {
	if math.IsNaN(x) || x <= 0 {
		return 1
	}
	return math.Min(maxSpeed, math.Max(minSpeed, x))
}

// Observe records a finished job. Successful runs teach duration, CPU and
// memory; failed runs teach memory (an OOM kill must raise the next
// estimate) and duration only when they ran about as long as usual;
// canceled/lost runs only memory.
func (e *estimator) Observe(job *proto.Job) {
	if job == nil || !job.State.Terminal() {
		return
	}
	u := job.Usage
	if u.WallSeconds <= 0 && u.PeakMem <= 0 {
		return
	}
	fp := job.Fingerprint
	if fp == "" {
		fp = fingerprint(&job.Spec)
	}
	node := job.Node
	if job.Local || node == "" {
		node = job.Origin
	}
	fam, words := specFamily(&job.Spec)
	variant := variantOf(fam, words)
	gid := e.groupIDOf(&job.Spec, fam, variant)
	pd := e.familyEstimate(&job.Spec, fam, 0)
	prior := familyDefault{CPUs: pd.CPUs, Mem: pd.Mem, Duration: pd.Duration}
	s := store.Sample{
		Fingerprint: fp, Node: node, Origin: job.Origin, Family: fam,
		CPUSeconds: u.CPUSeconds, PeakMem: u.PeakMem, WallSeconds: u.WallSeconds,
		At: job.EndedAt,
	}
	if s.At.IsZero() {
		s.At = time.Now()
	}

	e.mu.Lock()
	if e.opts.Self == "" && job.Origin != "" {
		e.self = job.Origin
	}
	en := e.entry(fp)
	en.family = fam
	sp := e.speedLocked(fp, node, en.samples, fam)
	switch job.State {
	case proto.JobSucceeded:
	case proto.JobFailed:
		// A run that failed about as late as a run usually takes (a
		// failing test in a full suite) teaches duration like a success;
		// one that failed early (does not compile, OOM kill) only memory.
		// "Usually" is the best estimate there is: this fingerprint's
		// runs, else its family's in this repo, else the static default.
		expected := prior.Duration
		if l := e.learn(en.samples, fam); l.haveRun {
			expected = l.duration
		} else if ge := e.groups[gid].estimate(prior); ge.duration > 0 {
			expected = ge.duration
		}
		if u.WallSeconds < 0.5*expected*sp {
			s.CPUSeconds, s.WallSeconds = 0, 0
		}
	default:
		s.CPUSeconds, s.WallSeconds = 0, 0
	}
	if s.WallSeconds <= 0 && s.PeakMem <= 0 {
		e.mu.Unlock()
		return
	}
	gr := groupRun{M: s.PeakMem}
	wall, cpuSec := s.WallSeconds, s.CPUSeconds
	if job.State == proto.JobFailed {
		// A failed run far longer than the family's usual hung or hit a
		// timeout. It stays in this fingerprint's history (the job may
		// just be slow) but does not speak for the family.
		if ge := e.groups[gid].estimate(prior); ge.duration > 0 && wall > failedHangFactor*ge.duration*sp {
			wall, cpuSec = 0, 0
		}
	}
	if wall > 0 {
		gr.W = wall / sp // origin-equivalent seconds
		if cpuSec > 0 && wall >= minCPUWall {
			gr.C = cpuSec / wall
			// Only container jobs run under a CPU cap (docker --cpus).
			gr.Capped = !job.Local && job.Runtime == proto.RuntimeContainer &&
				job.Alloc.CPUs > 0 && gr.C >= cpuCappedShare*job.Alloc.CPUs
		}
	}
	e.addGroupRunLocked(gid, variant, gr)
	e.updateFamilySpeedLocked(s, en.samples)
	en.samples = append([]store.Sample{s}, en.samples...)
	if len(en.samples) > window {
		en.samples = en.samples[:window]
	}
	fs := e.familySpeedJSONLocked(s)
	e.mu.Unlock()

	if e.st != nil {
		ctx, cancel := storeCtx()
		defer cancel()
		if err := e.st.AddSample(ctx, s); err != nil {
			e.opts.Logf("estimate: record sample for %s: %v", fp, err)
		}
		if fs != "" {
			if err := e.st.Set(ctx, familySpeedKey, fs); err != nil {
				e.opts.Logf("estimate: save speeds: %v", err)
			}
		}
		e.saveGroups(ctx)
	}
}

// updateFamilySpeedLocked learns how much slower/faster node is than self
// for this family, from a run on node compared with this fingerprint's
// runs on self.
func (e *estimator) updateFamilySpeedLocked(s store.Sample, prev []store.Sample) {
	self := e.selfName()
	if s.WallSeconds <= 0 || s.Node == self || self == "" {
		return
	}
	var onSelf ewma
	for i := len(prev) - 1; i >= 0; i-- {
		if p := prev[i]; p.WallSeconds > 0 && p.Node == self {
			onSelf.add(p.WallSeconds)
		}
	}
	if onSelf.N == 0 {
		return
	}
	m := e.famSpeed[s.Family]
	if m == nil {
		m = map[string]*ewma{}
		e.famSpeed[s.Family] = m
	}
	w := m[s.Node]
	if w == nil {
		w = &ewma{}
		m[s.Node] = w
	}
	w.add(clampSpeed(s.WallSeconds / onSelf.V))
}

func (e *estimator) familySpeedJSONLocked(s store.Sample) string {
	if s.Node == e.selfName() || s.WallSeconds <= 0 {
		return ""
	}
	b, err := json.Marshal(e.famSpeed)
	if err != nil {
		return ""
	}
	return string(b)
}

func (e *estimator) loadFamilySpeed() {
	if e.st == nil {
		return
	}
	ctx, cancel := storeCtx()
	defer cancel()
	v, ok, err := e.st.Get(ctx, familySpeedKey)
	if err != nil || !ok {
		return
	}
	var m map[string]map[string]*ewma
	if json.Unmarshal([]byte(v), &m) == nil && m != nil {
		e.famSpeed = m
	}
}
