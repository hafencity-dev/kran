package scheduler

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// cand is one (node, runtime) that may run a job.
type cand struct {
	v          *nodeView
	rt         rtView
	cpu        float64 // CPU need clamped to the runtime's size
	mem        int64
	fits       bool
	fitsNoHold bool
	oversize   bool    // larger than the runtime could ever hold
	run        float64 // expected run seconds there
	press      pressure
	overhead   float64 // sync + setup seconds
	wait       float64 // seconds until it fits (0 when it fits)
}

func (c *cand) total() float64 { return c.run + c.overhead }

func (c *cand) label() string {
	if c.rt.vm {
		return c.v.name + " (Linux container)"
	}
	return c.v.name
}

// dispatch places every queued job that can run now, in fair order, and
// refreshes the reasons of the rest. Callers hold s.mu.
func (s *sched) dispatch(now time.Time) {
	names := s.nodeNames()
	views := make(map[string]*nodeView, len(names))
	for _, n := range names {
		views[n] = s.view(n, now)
	}
	queue := s.ordered(now)
	holds := map[string]hold{}
	order := s.order[:0]
	// Within a pass free resources only shrink, so a job shape that fits
	// nowhere keeps fitting nowhere until a placement or hold changes the
	// numbers its reason quotes. Deep queues of similar jobs (agent
	// bursts) then cost one evaluation per shape, not per job.
	memo := map[string]string{}
	for _, j := range queue {
		key := ""
		if j.rejects == 0 && now.Sub(j.queuedAt) < s.opts.StarvationAfter {
			if j.shape == "" {
				j.shape = j.shapeKey()
			}
			key = j.shape
			if r, ok := memo[key]; ok {
				j.reason = r
				order = append(order, j.id)
				j.t.push(j.reason)
				continue
			}
		}
		res := s.tryPlace(j, views, names, now, holds)
		switch res {
		case placedJob, heldNode:
			clear(memo)
		case noFitAnywhere:
			if key != "" {
				memo[key] = j.reason
			}
		}
		if res == placedJob {
			continue
		}
		order = append(order, j.id)
		j.t.push(j.reason)
	}
	s.order = order
	s.holds = holds
	s.advanceVirtualTime()
}

// ordered returns queued jobs in dispatch order: starving jobs (queued
// longer than StarvationAfter) oldest first; then jobs re-queued after a
// failed start; then weighted fair queueing across owners, where an owner's
// k-th job (by priority, then FIFO) gets tag vt(owner) + Σ 1/weight.
func (s *sched) ordered(now time.Time) []*sjob {
	byOwner := map[string][]*sjob{}
	for _, j := range s.jobs {
		if j.alloc == nil {
			byOwner[j.owner] = append(byOwner[j.owner], j)
		}
	}
	type entry struct {
		j     *sjob
		class int
		tag   float64
	}
	var all []entry
	for owner, js := range byOwner {
		sort.Slice(js, func(a, b int) bool {
			if prioRank[js[a].prio] != prioRank[js[b].prio] {
				return prioRank[js[a].prio] > prioRank[js[b].prio]
			}
			return js[a].seq < js[b].seq
		})
		tag := s.owners[owner].vt
		for _, j := range js {
			tag += 1 / prioWeight[j.prio]
			class := 2
			switch {
			case now.Sub(j.queuedAt) >= s.opts.StarvationAfter:
				class = 0
			case j.requeued:
				class = 1
			}
			all = append(all, entry{j, class, tag})
		}
	}
	sort.Slice(all, func(a, b int) bool {
		x, y := all[a], all[b]
		if x.class != y.class {
			return x.class < y.class
		}
		if x.class == 0 && !x.j.queuedAt.Equal(y.j.queuedAt) {
			return x.j.queuedAt.Before(y.j.queuedAt)
		}
		if x.class == 2 && x.tag != y.tag {
			return x.tag < y.tag
		}
		return x.j.seq < y.j.seq
	})
	out := make([]*sjob, len(all))
	for i, e := range all {
		out[i] = e.j
	}
	return out
}

func (s *sched) advanceVirtualTime() {
	minVT, maxVT := math.Inf(1), s.vnow
	for _, o := range s.owners {
		if o.queued > 0 && o.vt < minVT {
			minVT = o.vt
		}
		if o.vt > maxVT {
			maxVT = o.vt
		}
	}
	if math.IsInf(minVT, 1) {
		s.vnow = maxVT
	} else if minVT > s.vnow {
		s.vnow = minVT
	}
	for k, o := range s.owners {
		if o.queued == 0 && o.vt <= s.vnow {
			delete(s.owners, k) // would restart at vnow anyway
		}
	}
}

func (s *sched) speed(j *sjob, v *nodeView) float64 {
	if v.self {
		return 1
	}
	if s.opts.Speed != nil {
		// Exactly 1 means "unknown" in the estimator's contract; fall
		// through to the benchmark prior then.
		if sp := s.opts.Speed(j.fp, v.name); sp > 0 && sp != 1 {
			return sp
		}
	}
	if o := s.nodes[s.opts.Origin]; o != nil {
		so, sn := o.st.Info.SingleCoreScore, v.e.st.Info.SingleCoreScore
		if so > 0 && sn > 0 {
			return math.Min(5, math.Max(0.2, so/sn))
		}
	}
	return 1
}

// syncSeconds is the expected sync + setup time of the job's workspace on
// a remote node: learned per repo and node from finished jobs, else a
// cold-start guess from the link's RTT and bandwidth.
func (s *sched) syncSeconds(j *sjob, v *nodeView) float64 {
	if v.self {
		return 0
	}
	if s.opts.SyncCost != nil {
		return math.Max(0, s.opts.SyncCost(j.repo, v.name))
	}
	if c := s.syncCost[j.repo+"\x00"+v.name]; c != nil && j.repo != "" {
		return c.v
	}
	rtt := v.e.st.Link.RTTMillis / 1000
	if rtt <= 0 {
		rtt = 0.01
	}
	bw := v.e.st.Link.BandwidthBps
	if bw <= 0 {
		bw = 20e6
	}
	// First sync of a repo: ~50 MB of sources and a dependency install.
	return 1 + 10*rtt + 50e6/bw + 15
}

func (s *sched) candidate(j *sjob, v *nodeView, rt rtView, now time.Time) *cand {
	c := &cand{v: v, rt: rt, mem: j.est.Mem}
	c.cpu = math.Min(j.est.CPUs, v.capCPU(rt))
	c.oversize = c.mem > v.capMem(rt)
	if !c.oversize {
		c.fits = v.fits(rt, c.cpu, c.mem, false)
		c.fitsNoHold = c.fits || v.fits(rt, c.cpu, c.mem, true)
	}
	// A job squeezed onto fewer cores than it would use runs longer, and
	// so does one on a node thrashing on memory or I/O.
	c.press = noPressure()
	if !s.ignorePressure {
		c.press = v.pressure(rt)
	}
	c.run = j.est.Duration * s.speed(j, v) * (j.est.CPUs / c.cpu) * c.press.slow
	c.overhead = s.syncSeconds(j, v)
	switch {
	case c.fits:
	case c.oversize:
		c.wait = math.Inf(1)
	default:
		c.wait = v.waitFor(rt, c.cpu, c.mem, now)
	}
	return c
}

// better orders candidates: expected completion, then origin, then lower
// load, then native runtime, then name (determinism).
func better(a, b *cand) bool {
	ta, tb := math.Round(a.total()*100), math.Round(b.total()*100)
	if ta != tb {
		return ta < tb
	}
	if a.v.self != b.v.self {
		return a.v.self
	}
	if la, lb := a.v.load(), b.v.load(); la != lb {
		return la < lb
	}
	if a.rt.rt.Kind != b.rt.rt.Kind {
		return a.rt.rt.Kind == proto.RuntimeNative
	}
	return a.v.name < b.v.name
}

type outcome int

const (
	placedJob     outcome = iota
	heldNode              // not placed; a starving job now holds a node
	noFitAnywhere         // not placed; the verdict depends only on the job's shape
	deferred              // not placed; chose to wait for a better node
)

// shapeKey identifies jobs that admission and reasons treat identically.
func (j *sjob) shapeKey() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%g|%d|%t|%s|%s|%t|%s|%t|%s|%s", j.est.CPUs, j.est.Mem, j.est.Duration < shortJobSeconds,
		j.req.Prefer, j.req.Node, j.req.GPU, j.req.OS, j.req.OriginOnly, j.req.OriginOnlyReason, j.prio)
	if len(j.req.Toolchains) > 0 {
		keys := make([]string, 0, len(j.req.Toolchains))
		for k := range j.req.Toolchains {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("|" + k + "@" + j.req.Toolchains[k])
		}
	}
	return b.String()
}

// tryPlace evaluates one queued job against current views; it places the
// job (updating views) or sets j.reason.
func (s *sched) tryPlace(j *sjob, views map[string]*nodeView, names []string, now time.Time, holds map[string]hold) outcome {
	var cands []*cand
	var whys []*ineligible
	for _, name := range names {
		v := views[name]
		rts, why := s.eligible(j, v, now)
		if why != nil {
			whys = append(whys, why)
			continue
		}
		for _, rt := range rts {
			cands = append(cands, s.candidate(j, v, rt, now))
		}
	}
	if len(cands) == 0 {
		j.reason = s.ineligibleReason(j, whys)
		return noFitAnywhere
	}

	var fits []*cand
	var selfCand *cand
	for _, c := range cands {
		if c.fits {
			fits = append(fits, c)
		}
		if c.v.self && selfCand == nil {
			selfCand = c
		}
	}
	starving := now.Sub(j.queuedAt) >= s.opts.StarvationAfter

	// Short jobs stay on the origin: sending them costs more than it saves.
	// They need only memory to start: run locally without kran they would
	// share the CPUs too, for a few seconds.
	if selfCand != nil && j.est.Duration < shortJobSeconds && j.req.Node == "" && !selfCand.oversize {
		if selfCand.fits || selfCand.v.fitsShort(selfCand.rt, selfCand.cpu, selfCand.mem) {
			s.place(j, selfCand, now, fmt.Sprintf("here: est. %s, too short to send elsewhere", fmtDur(j.est.Duration)))
			return placedJob
		}
		j.reason = "waiting: est. " + fmtDur(j.est.Duration) + ", too short to send elsewhere; " +
			strings.TrimPrefix(s.resourceReason(j, []*cand{selfCand}), "waiting: ")
		if s.maybeHold(j, []*cand{selfCand}, starving, holds) {
			return heldNode
		}
		return noFitAnywhere
	}

	if len(fits) == 0 {
		return s.noFit(j, cands, whys, starving, now, holds)
	}

	sort.SliceStable(fits, func(a, b int) bool { return better(fits[a], fits[b]) })
	best := fits[0]
	if !best.v.self && selfCand != nil && selfCand.fits {
		// Offload only when it saves real time over running here.
		hyst := math.Max(1, 0.05*selfCand.total())
		if best.total()+hyst >= selfCand.total() {
			best = selfCand
		}
	}

	// Waiting briefly for a clearly better node beats running now on a
	// much slower one (e.g. a short queue on the origin vs a cold sync).
	if !starving && now.Sub(j.queuedAt) < maxDefer {
		var bw *cand
		for _, c := range cands {
			if c.fits || c.oversize || math.IsInf(c.wait, 1) {
				continue
			}
			if bw == nil || c.wait+c.total() < bw.wait+bw.total() {
				bw = c
			}
		}
		if bw != nil && bw.wait+bw.total() < deferGain*best.total()-2 {
			j.reason = fmt.Sprintf("waiting: %s should free up in ~%s; est. %s there vs %s on %s",
				bw.label(), fmtDur(bw.wait), fmtDur(bw.total()), fmtDur(best.total()), best.label())
			return deferred
		}
	}

	s.place(j, best, now, s.placeReason(j, best, selfCand, cands, fits, whys))
	return placedJob
}

// noFit handles a job that fits nowhere right now.
func (s *sched) noFit(j *sjob, cands []*cand, whys []*ineligible, starving bool, now time.Time, holds map[string]hold) outcome {
	allOversize := true
	for _, c := range cands {
		if !c.oversize {
			allOversize = false
		}
	}
	if allOversize {
		// Larger than any node: run alone on the largest instead of
		// queueing forever (SPEC §6.3).
		big := cands[0]
		for _, c := range cands[1:] {
			if c.v.capMem(c.rt) > big.v.capMem(big.rt) {
				big = c
			}
		}
		capMem := big.v.capMem(big.rt)
		if big.v.empty(big.rt) {
			big.mem = capMem
			s.place(j, big, now, fmt.Sprintf("%s: needs %s, more than any node has; runs alone on the largest (%s)",
				big.label(), fmtBytes(j.est.Mem), fmtBytes(capMem)))
			return placedJob
		}
		j.reason = fmt.Sprintf("waiting: needs %s, more than any node has; will run alone on %s (%s) once its %d running jobs finish",
			fmtBytes(j.est.Mem), big.label(), fmtBytes(capMem), big.v.running)
		if starving && s.holdAll(j, big, holds) {
			j.reason += "; holding it"
			return heldNode
		}
		return noFitAnywhere
	}
	j.reason = s.resourceReason(j, cands) + otherReasons(whys)
	if s.maybeHold(j, cands, starving, holds) {
		return heldNode
	}
	return noFitAnywhere
}

// otherReasons lists nodes that are out for a temporary reason (status,
// failures, disk) after a resource reason: they are why the job does not
// simply go there. Permanent mismatches (no GPU, wrong OS) are left out.
func otherReasons(whys []*ineligible) string {
	var parts []string
	for _, w := range whys {
		switch w.kind {
		case whyStatus, whyBreaker, whyDisk, whyFailed:
			if len(parts) < 2 {
				parts = append(parts, w.reason)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

// maybeHold makes a starving job's best node keep freed resources for it:
// later jobs in the pass see them as taken, so the starving job fits as
// soon as enough running jobs end (no starvation by a stream of small jobs).
func (s *sched) maybeHold(j *sjob, cands []*cand, starving bool, holds map[string]hold) bool {
	if !starving {
		return false
	}
	var target *cand
	for _, c := range cands {
		if c.oversize {
			continue
		}
		if _, taken := holds[c.v.name]; taken {
			continue
		}
		if target == nil || c.wait < target.wait ||
			(c.wait == target.wait && math.IsInf(c.wait, 1) && c.v.capMem(c.rt) > target.v.capMem(target.rt)) {
			target = c
		}
	}
	if target == nil {
		return false
	}
	res := proto.Resources{CPUs: target.cpu, Mem: target.mem}
	for _, p := range target.v.pools(target.rt) {
		p.holdCPU += res.CPUs
		p.holdMem += res.Mem
	}
	holds[target.v.name] = hold{job: j.id, res: res, vm: target.rt.vm}
	j.reason += "; next in line on " + target.v.name
	return true
}

func (s *sched) holdAll(j *sjob, c *cand, holds map[string]hold) bool {
	if _, taken := holds[c.v.name]; taken {
		return false
	}
	res := proto.Resources{CPUs: c.v.capCPU(c.rt), Mem: c.v.capMem(c.rt)}
	for _, p := range c.v.pools(c.rt) {
		p.holdCPU += p.capCPU
		p.holdMem += p.capMem
	}
	holds[c.v.name] = hold{job: j.id, res: res, vm: c.rt.vm}
	return true
}

// loggedWait: placing a job that waited this long logs why it waited.
const loggedWait = 30 * time.Second

func (s *sched) place(j *sjob, c *cand, now time.Time, reason string) {
	res := proto.Resources{CPUs: c.cpu, Mem: c.mem}
	fallback := c.v.self && len(j.excluded) > 0
	if fallback {
		failed := make([]string, 0, len(j.excluded))
		for n := range j.excluded {
			failed = append(failed, n)
		}
		sort.Strings(failed)
		reason = fmt.Sprintf("here: fallback after %s failed to start it (%s)", joinList(failed), j.excluded[failed[0]])
	}
	pl := Placement{
		JobID:    j.id,
		Node:     c.v.name,
		Runtime:  c.rt.rt.Kind,
		Local:    c.v.self,
		Alloc:    res,
		Reason:   reason,
		Fallback: fallback,
	}
	a := &allocation{
		job: j.id, node: c.v.name, rt: c.rt.rt.Kind, vm: c.rt.vm, vmKnown: true, res: res, at: now,
		end:  now.Add(time.Duration((c.overhead + c.run) * float64(time.Second))),
		repo: j.repo, pl: pl,
	}
	c.v.e.allocs[j.id] = a
	c.v.allocs = append(c.v.allocs, a)
	sort.Slice(c.v.allocs, func(x, y int) bool { return c.v.allocs[x].end.Before(c.v.allocs[y].end) })
	for _, p := range c.v.pools(c.rt) {
		p.take(res)
	}
	c.v.running++
	j.alloc = a
	j.requeued = false
	waited := ""
	if w := now.Sub(j.queuedAt); w >= loggedWait && j.reason != "" {
		// Why it waited is otherwise lost with the queue entry.
		waited = fmt.Sprintf(" (after waiting %s: %s)", fmtDur(w.Seconds()), strings.TrimPrefix(j.reason, "waiting: "))
	}
	j.reason = ""
	s.dequeueOwner(j)
	if o := s.owners[j.owner]; o != nil {
		o.vt += 1 / prioWeight[j.prio]
	}
	s.opts.Logf("scheduler: placed %s on %s (%s): %s%s", j.id, c.v.name, c.rt.rt.Kind, reason, waited)
	j.t.resolve(pl, nil)
}

func (s *sched) placeReason(j *sjob, best, selfCand *cand, cands, fits []*cand, whys []*ineligible) string {
	if best.v.self {
		switch {
		case j.req.Prefer == proto.PreferLocal:
			return "here (--local)"
		case j.req.OriginOnly:
			if j.req.OriginOnlyReason != "" {
				return "here: " + j.req.OriginOnlyReason
			}
			return "here: this job can only run here"
		case j.req.Node != "":
			return "here (--node)"
		}
		var other *cand
		for _, c := range fits {
			if !c.v.self {
				other = c
				break
			}
		}
		here := "here: est. " + fmtDur(best.run) + paren(best.press.note())
		if other != nil {
			return fmt.Sprintf("%s; %s would take %s%s plus ~%s to sync", here,
				other.label(), fmtDur(other.run), paren(other.press.note()), fmtDur(other.overhead))
		}
		if why := s.elsewhereReason(j, cands, whys); why != "" {
			return here + "; " + why
		}
		return here
	}
	r := fmt.Sprintf("%s: %s free, est. %s%s", best.label(), fmtCPUs(nonNeg(best.v.freeCPUFor(best.rt))), fmtDur(best.total()),
		paren(best.press.note()))
	switch {
	case j.req.Node != "":
		r += " (--node)"
	case selfCand != nil && selfCand.fits:
		r += " vs " + fmtDur(selfCand.total()) + " here" + paren(selfCand.press.note())
	case selfCand != nil:
		r += "; here: " + strings.TrimPrefix(s.resourceReason(j, []*cand{selfCand}), "waiting: ")
	default:
		for _, w := range whys {
			if w.node == s.opts.Origin && w.kind != whyPolicy {
				r += "; " + w.reason
			}
		}
	}
	if j.req.GPU {
		if gpus := s.gpuNodes(); len(gpus) == 1 {
			r += " (only node with " + s.gpuWord() + ")"
		}
	}
	return r
}

// elsewhereReason says why no other node could take a job that runs here:
// "devmac's Linux VM has 1.5 CPUs free", "devmac: missing toolchain go@1.27".
func (s *sched) elsewhereReason(j *sjob, cands []*cand, whys []*ineligible) string {
	var parts []string
	var short []*cand
	for _, c := range cands {
		switch {
		case c.v.self || c.fits:
		case c.oversize:
			parts = append(parts, fmt.Sprintf("%s cannot hold %s", c.label(), fmtBytes(c.mem)))
		default:
			short = append(short, c)
		}
	}
	if len(short) > 0 {
		r := s.resourceReason(j, short)
		if r != "waiting: no node can take it yet" {
			parts = append(parts, strings.TrimPrefix(r, "waiting: "))
		}
	}
	for _, w := range whys {
		if w.node != s.opts.Origin && w.kind != whyPolicy {
			parts = append(parts, w.reason)
		}
	}
	return strings.Join(parts, "; ")
}

// paren wraps a non-empty note: " (note)".
func paren(note string) string {
	if note == "" {
		return ""
	}
	return " (" + note + ")"
}

func (s *sched) gpuNodes() []string {
	var out []string
	for _, n := range s.nodeNames() {
		if s.gpuMatch(s.nodes[n]) {
			out = append(out, n)
		}
	}
	return out
}

// resourceReason names the binding resource on every candidate node:
// "waiting: needs 6.0 GiB, devmac has 3.1 GiB free, gx10 has 1.2 GiB free".
func (s *sched) resourceReason(j *sjob, cands []*cand) string {
	type short struct {
		label          string
		mem, cpu       bool
		freeMem        int64
		freeCPU        float64
		held, maxJobs  bool
		running, limit int
	}
	var shorts []short
	seen := map[string]bool{}
	anyMem, anyCPU := false, false
	for _, c := range cands {
		if c.fits || c.oversize || seen[c.v.name] {
			continue
		}
		seen[c.v.name] = true
		sh := short{label: c.v.name}
		if lim := c.v.e.st.MaxJobs; lim > 0 && c.v.running >= lim {
			sh.maxJobs, sh.running, sh.limit = true, c.v.running, lim
			shorts = append(shorts, sh)
			continue
		}
		if c.fitsNoHold {
			sh.held = true
			shorts = append(shorts, sh)
			continue
		}
		for _, p := range c.v.pools(c.rt) {
			fm, fc := p.freeMem()+p.holdMem, p.freeCPU()+p.holdCPU
			m, cp := c.mem > fm, c.cpu > fc*cpuOvercommit+1e-9
			if !m && !cp {
				continue
			}
			if p == c.v.vm {
				sh.label = c.v.name + "'s Linux VM"
			}
			sh.mem, sh.cpu, sh.freeMem, sh.freeCPU = m, cp, fm, fc
			break
		}
		anyMem = anyMem || sh.mem
		anyCPU = anyCPU || sh.cpu
		shorts = append(shorts, sh)
	}
	if len(shorts) == 0 {
		return "waiting: no node can take it yet"
	}
	var need string
	switch {
	case anyMem && anyCPU:
		need = fmtCPUs(j.est.CPUs) + " and " + fmtBytes(j.est.Mem)
	case anyCPU:
		need = fmtCPUs(j.est.CPUs)
	default:
		need = fmtBytes(j.est.Mem)
	}
	var parts []string
	for _, sh := range shorts {
		switch {
		case sh.maxJobs:
			parts = append(parts, fmt.Sprintf("%s is at its limit of %d jobs", sh.label, sh.limit))
		case sh.held:
			parts = append(parts, sh.label+" is held for a job that has waited longer")
		case sh.mem && sh.cpu:
			parts = append(parts, fmt.Sprintf("%s has %s and %s free", sh.label, fmtCPUs(nonNeg(sh.freeCPU)), fmtBytes(sh.freeMem)))
		case sh.cpu && !anyMem:
			// "needs 8 CPUs, devmac has 2.5 free": the unit is already named.
			parts = append(parts, fmt.Sprintf("%s has %s free", sh.label, fmtNum(nonNeg(sh.freeCPU))))
		case sh.cpu:
			parts = append(parts, fmt.Sprintf("%s has %s free", sh.label, fmtCPUs(nonNeg(sh.freeCPU))))
		default:
			parts = append(parts, fmt.Sprintf("%s has %s free", sh.label, fmtBytes(sh.freeMem)))
		}
	}
	onlyMarkers := true
	for _, sh := range shorts {
		if sh.mem || sh.cpu {
			onlyMarkers = false
		}
	}
	var r string
	if onlyMarkers {
		r = "waiting: " + strings.Join(parts, ", ")
	} else {
		sep := ", "
		if anyMem && anyCPU {
			sep = "; "
		}
		r = "waiting: needs " + need + sep + strings.Join(parts, ", ")
	}
	switch {
	case j.req.Prefer == proto.PreferLocal:
		r += " (--local)"
	case j.req.OriginOnly && j.req.OriginOnlyReason != "":
		r += " (runs only here: " + j.req.OriginOnlyReason + ")"
	case j.req.GPU:
		if gpus := s.gpuNodes(); len(gpus) == 1 {
			r += " (only " + gpus[0] + " has " + s.gpuWord() + ")"
		}
	}
	return r
}

// ineligibleReason explains why no node may run the job at all.
func (s *sched) ineligibleReason(j *sjob, whys []*ineligible) string {
	var shown []*ineligible
	for _, w := range whys {
		if w.kind != whyPolicy {
			shown = append(shown, w)
		}
	}
	if j.req.GPU {
		gpus := s.gpuNodes()
		if len(gpus) == 0 {
			return "no node has " + s.gpuWord()
		}
		var gpuWhys []string
		for _, w := range shown {
			for _, g := range gpus {
				if w.node == g {
					gpuWhys = append(gpuWhys, strings.TrimPrefix(w.reason, g+": "))
				}
			}
		}
		if len(gpus) == 1 {
			r := "only " + gpus[0] + " has " + s.gpuWord()
			if len(gpuWhys) > 0 {
				r += " (" + gpus[0] + ": " + gpuWhys[0] + ")"
			}
			return r
		}
	}
	if len(shown) == 0 {
		return "waiting: no node matches the job's flags"
	}
	parts := make([]string, len(shown))
	for i, w := range shown {
		parts[i] = w.reason
	}
	return strings.Join(parts, "; ")
}
