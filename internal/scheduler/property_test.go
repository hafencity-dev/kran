package scheduler

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// harness drives a scheduler with a stream of random operations and
// checks invariants after each one. The operation stream comes from next(),
// backed by math/rand (property tests) or fuzz input bytes.
type harness struct {
	t       testing.TB
	clk     *fakeClock
	s       *sched
	nodes   map[string]proto.NodeState
	tickets map[string]Ticket
	placed  map[string]Placement
	foreign map[string]bool
	next    func(n int) int
	trace   []string
	id      int
}

func newHarness(t testing.TB, next func(n int) int) *harness {
	h := &harness{
		t: t, clk: &fakeClock{t0}, next: next,
		nodes:   map[string]proto.NodeState{},
		tickets: map[string]Ticket{},
		placed:  map[string]Placement{},
		foreign: map[string]bool{},
	}
	h.s = New(Options{Origin: "gx10", Now: h.clk.Now}).(*sched)
	g, d := self(gx10(t0)), devmac(t0)
	box := proto.NodeState{
		Info: proto.NodeInfo{Name: "box", OS: "linux", Arch: "amd64", CPUs: 8, MemTotal: 32 * gib, SingleCoreScore: 700,
			Runtimes: []proto.Runtime{{Kind: proto.RuntimeNative, OS: "linux", Arch: "amd64", CPUs: 8, Mem: 32 * gib, Available: true}}},
		Status: proto.NodeOnline, Accepts: true, Reserve: proto.Resources{CPUs: 1, Mem: 2 * gib},
		Metrics: proto.NodeMetrics{Time: t0, MemAvailable: 28 * gib, DiskFree: 100 * gib},
	}
	for _, n := range []proto.NodeState{g, d, box} {
		h.nodes[n.Info.Name] = n
		h.s.UpdateNode(n)
	}
	return h
}

var owners = []string{"s1", "s2", "s3", "s4", "s5"}

func (h *harness) step() {
	switch op := h.next(100); {
	case op < 35:
		if len(h.tickets) > 150 {
			h.release() // keep the queue at a realistic depth
			break
		}
		h.submit()
	case op < 55:
		h.release()
	case op < 60:
		h.cancel()
	case op < 66:
		h.reject()
	case op < 76:
		h.metrics()
	case op < 79:
		h.status()
	case op < 87:
		h.clk.Advance(time.Duration(1+h.next(60)) * time.Second)
		h.s.Tick(h.clk.Now())
		h.log("tick")
	case op < 90:
		h.setPriority()
	case op < 95:
		h.admit()
	default:
		h.usage()
	}
	h.collect()
	h.check()
}

func (h *harness) log(format string, args ...any) {
	h.trace = append(h.trace, fmt.Sprintf(format, args...))
}

func (h *harness) pick(m map[string]bool) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[h.next(len(keys))]
}

func (h *harness) queuedSet() map[string]bool {
	m := map[string]bool{}
	for id := range h.tickets {
		if _, ok := h.placed[id]; !ok {
			m[id] = true
		}
	}
	return m
}

func (h *harness) placedSet(remoteOnly bool) map[string]bool {
	m := map[string]bool{}
	for id, pl := range h.placed {
		if !remoteOnly || !pl.Local {
			m[id] = true
		}
	}
	return m
}

func (h *harness) submit() {
	h.id++
	id := fmt.Sprintf("j%04d", h.id)
	owner := owners[h.next(len(owners))]
	sizes := []int64{512 * mib, 2 * gib, 4 * gib, 6 * gib, 16 * gib, 24 * gib, 150 * gib}
	mem := sizes[h.next(len(sizes))]
	cpus := float64(1 + h.next(12))
	dur := []float64{2, 20, 60, 120, 600}[h.next(5)]
	j := newJob(id, owner, cpus, mem, dur)
	j.QueuedAt = h.clk.Now()
	switch h.next(20) {
	case 0:
		j.Spec.Req.Prefer = proto.PreferLocal
	case 1:
		j.Spec.Req.Prefer = proto.PreferRemote
	case 2:
		j.Spec.Req.Node = []string{"gx10", "devmac", "box"}[h.next(3)]
	case 3:
		j.Spec.Req.GPU = true
	case 4, 5:
		j.Spec.Req.OS = proto.OSAny
	case 6:
		j.Spec.TTY = &proto.TTY{}
	case 7:
		j.Spec.Priority = proto.PriorityLow
	case 8:
		j.Spec.NoFallback = true
	}
	h.tickets[id] = h.s.Submit(j)
	h.log("submit %s %s cpu=%v mem=%d dur=%v", id, owner, cpus, mem, dur)
}

func (h *harness) release() {
	id := h.pick(h.placedSet(false))
	if id == "" {
		return
	}
	h.s.Release(id, &proto.Usage{WallSeconds: 10, SyncSeconds: float64(h.next(5))})
	delete(h.placed, id)
	delete(h.tickets, id)
	h.log("release %s", id)
	if fid := h.pick(h.foreign); fid != "" && h.next(2) == 0 {
		h.s.Release(fid, nil)
		delete(h.foreign, fid)
		h.log("release foreign %s", fid)
	}
}

func (h *harness) cancel() {
	id := h.pick(h.queuedSet())
	if id == "" {
		return
	}
	h.s.Cancel(id)
	delete(h.tickets, id)
	h.log("cancel %s", id)
}

func (h *harness) reject() {
	id := h.pick(h.placedSet(true))
	if id == "" {
		return
	}
	node := h.placed[id].Node
	delete(h.placed, id)
	h.tickets[id] = h.s.Reject(id, node, errors.New("sync failed"))
	h.log("reject %s on %s", id, node)
}

func (h *harness) metrics() {
	names := []string{"box", "devmac", "gx10"}
	n := h.nodes[names[h.next(3)]]
	n.Metrics.Time = h.clk.Now()
	n.Metrics.MemAvailable = n.Info.MemTotal * int64(h.next(101)) / 100
	n.Metrics.CPUBusy = float64(h.next(101)) / 100
	if rm, ok := n.Metrics.Runtimes[proto.RuntimeContainer]; ok {
		rm.MemAvailable = 32 * gib * int64(h.next(101)) / 100
		n.Metrics.Runtimes = map[proto.RuntimeKind]proto.RuntimeMetrics{proto.RuntimeContainer: rm}
	}
	h.nodes[n.Info.Name] = n
	h.s.UpdateNode(n)
	h.log("metrics %s mem=%d busy=%.2f", n.Info.Name, n.Metrics.MemAvailable, n.Metrics.CPUBusy)
}

func (h *harness) status() {
	names := []string{"box", "devmac", "gx10"}
	n := h.nodes[names[h.next(3)]]
	n.Status = []proto.NodeStatus{proto.NodeOnline, proto.NodeOnline, proto.NodeDraining, proto.NodeOffline}[h.next(4)]
	if n.Self && n.Status == proto.NodeOffline {
		n.Status = proto.NodeOnline
	}
	h.nodes[n.Info.Name] = n
	h.s.UpdateNode(n)
	h.log("status %s %s", n.Info.Name, n.Status)
}

func (h *harness) setPriority() {
	id := h.pick(h.queuedSet())
	if id == "" {
		return
	}
	p := []proto.Priority{proto.PriorityLow, proto.PriorityNormal, proto.PriorityHigh}[h.next(3)]
	h.s.SetPriority(id, p)
	h.log("prio %s %s", id, p)
}

func (h *harness) admit() {
	h.id++
	id := fmt.Sprintf("f%04d", h.id)
	j := newJob(id, "remote", float64(1+h.next(8)), int64(1+h.next(30))*gib, 60)
	rt := []proto.RuntimeKind{proto.RuntimeNative, proto.RuntimeContainer}[h.next(2)]
	if _, err := h.s.Admit(j, rt); err == nil {
		h.foreign[id] = true
		h.log("admit %s", id)
	} else {
		var ae *AdmitError
		if !errors.As(err, &ae) || ae.Reason == "" {
			h.t.Fatalf("admit error without reason: %v", err)
		}
		h.log("admit refused %s: %s", id, ae.Code)
	}
}

func (h *harness) usage() {
	id := h.pick(h.placedSet(false))
	if id == "" {
		return
	}
	pl := h.placed[id]
	h.s.UpdateJobUsage(id, proto.Resources{CPUs: pl.Alloc.CPUs / 2, Mem: pl.Alloc.Mem * int64(h.next(120)) / 100})
	h.log("usage %s", id)
}

// collect records newly resolved tickets.
func (h *harness) collect() {
	ids := make([]string, 0, len(h.tickets))
	for id := range h.tickets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, done := h.placed[id]; done {
			continue
		}
		tk := h.tickets[id]
		select {
		case <-tk.(*ticket).done:
		default:
			continue
		}
		pl, err := isPlacedErr(tk)
		if err != nil {
			delete(h.tickets, id)
			h.log("resolved %s: %v", id, err)
			continue
		}
		h.placed[id] = pl
		h.log("placed %s on %s/%s alloc=%v fallback=%v", id, pl.Node, pl.Runtime, pl.Alloc, pl.Fallback)
	}
}

func isPlacedErr(tk Ticket) (Placement, error) {
	t := tk.(*ticket)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pl, t.err
}

func (h *harness) check() {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	now := h.clk.Now()
	queued := 0
	perOwner := map[string]int{}
	inOrder := map[string]bool{}
	for _, id := range s.order {
		if inOrder[id] {
			h.t.Fatalf("job %s twice in queue order", id)
		}
		inOrder[id] = true
	}
	for id, j := range s.jobs {
		if j.alloc == nil {
			queued++
			perOwner[j.owner]++
			if !inOrder[id] {
				h.t.Fatalf("queued job %s missing from queue order", id)
			}
			if j.reason == "" {
				h.t.Fatalf("queued job %s has no reason", id)
			}
			continue
		}
		n := s.nodes[j.alloc.node]
		if n != nil && n.allocs[id] != j.alloc {
			h.t.Fatalf("placed job %s missing from node %s allocations", id, j.alloc.node)
		}
	}
	if queued != len(s.order) {
		h.t.Fatalf("queue order has %d jobs, %d queued", len(s.order), queued)
	}
	for k, o := range s.owners {
		if o.queued != perOwner[k] {
			h.t.Fatalf("owner %s counts %d queued, actual %d", k, o.queued, perOwner[k])
		}
	}
	for name := range s.nodes {
		v := s.view(name, now)
		// Memory accounting never exceeds what the node can schedule.
		if v.host.allocMem > v.host.capMem {
			h.t.Fatalf("%s: allocated %s > capacity %s\n%s", name, fmtBytes(v.host.allocMem), fmtBytes(v.host.capMem), h.tail())
		}
		if v.vm != nil && v.vm.allocMem > v.vm.capMem {
			h.t.Fatalf("%s VM: allocated %s > capacity %s", name, fmtBytes(v.vm.allocMem), fmtBytes(v.vm.capMem))
		}
		// CPU: ×1.25 for regular admission, up to ×2 with short local jobs.
		if v.host.allocCPU > v.host.capCPU*shortCPUOvercommit+1e-6 {
			h.t.Fatalf("%s: allocated %.1f CPUs > %.1f×%.2f", name, v.host.allocCPU, v.host.capCPU, shortCPUOvercommit)
		}
	}
	for id, pl := range h.placed {
		if pl.Alloc.Mem <= 0 || pl.Alloc.CPUs <= 0 || pl.Reason == "" {
			h.t.Fatalf("placement %s: %+v", id, pl)
		}
	}
}

func (h *harness) tail() string {
	from := max(0, len(h.trace)-15)
	return strings.Join(h.trace[from:], "\n")
}

func runRandom(t testing.TB, seed int64, steps int) []string {
	r := rand.New(rand.NewSource(seed))
	h := newHarness(t, r.Intn)
	for i := 0; i < steps; i++ {
		h.step()
	}
	snap := h.s.Snapshot()
	h.log("final queue=%v", snap.Queue)
	return h.trace
}

// KRAN_PROPERTY_LONG=1 runs 20 seeds × 3000 steps (about 2 min with -race).
func TestPropertyInvariantsRandom(t *testing.T) {
	steps, seeds := 1200, 6
	if os.Getenv("KRAN_PROPERTY_LONG") != "" {
		steps, seeds = 3000, 20
	}
	if testing.Short() {
		steps, seeds = 400, 3
	}
	for seed := int64(1); seed <= int64(seeds); seed++ {
		runRandom(t, seed, steps)
	}
}

func TestPropertyDeterminism(t *testing.T) {
	for seed := int64(100); seed < 103; seed++ {
		a := runRandom(t, seed, 800)
		b := runRandom(t, seed, 800)
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			for i := range a {
				if i >= len(b) || a[i] != b[i] {
					t.Fatalf("seed %d diverges at step %d:\n%s\n%s", seed, i, a[i], b[min(i, len(b)-1)])
				}
			}
		}
	}
}

// FuzzScheduler interprets fuzz input as an operation stream.
func FuzzScheduler(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte("submit release reject metrics status tick admit usage"))
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			data = data[:4096] // one operation per byte: keep inputs fast
		}
		pos := 0
		next := func(n int) int {
			if pos >= len(data) {
				return 0
			}
			b := int(data[pos])
			pos++
			return b % n
		}
		h := newHarness(t, next)
		for pos < len(data) {
			h.step()
		}
	})
}
