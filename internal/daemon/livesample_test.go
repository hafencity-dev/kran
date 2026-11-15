package daemon

import (
	"errors"
	"sync"
	"testing"

	"github.com/hafencity-dev/kran/internal/metrics"
	"github.com/hafencity-dev/kran/internal/proto"
)

// refProber answers JobUsage per reference and records what was asked.
type refProber struct {
	*fakeProber
	mu    sync.Mutex
	cg    map[string]proto.Resources
	pg    map[int]proto.Resources
	asked []metrics.UsageRef
}

func (p *refProber) JobUsage(ref metrics.UsageRef) (proto.Resources, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, ref)
	if ref.CgroupPath != "" {
		if r, ok := p.cg[ref.CgroupPath]; ok {
			return r, nil
		}
		return proto.Resources{}, errors.New("no such cgroup")
	}
	if r, ok := p.pg[ref.PGID]; ok {
		return r, nil
	}
	return proto.Resources{}, errors.New("no such process group")
}

// TestLocalJobLiveUsageComesFromItsScope: a local job in a systemd scope
// is measured by the scope's cgroup, which holds every process it started
// (a precheck's tools move to process groups of their own: measured by its
// process group it looked like 160 MiB while it used gigabytes, and the
// scheduler counted its whole reservation again on top of what gx10
// already used). A job without a scope, or whose scope cannot be read,
// is measured by its process group as before.
func TestLocalJobLiveUsageComesFromItsScope(t *testing.T) {
	sched := newFakeSched("a", alwaysLocal)
	pr := &refProber{fakeProber: newFakeProber("a"),
		cg: map[string]proto.Resources{"/sys/fs/cgroup/kran.slice/kran-local-j1.scope": {CPUs: 7.5, Mem: 6 << 30}},
		pg: map[int]proto.Resources{4001: {CPUs: 0.02, Mem: 160 << 20}, 4002: {CPUs: 1, Mem: 1 << 30}, 4003: {CPUs: 2, Mem: 2 << 30}}}
	resolved := map[string]int{}
	d, err := New(Deps{Config: NodeConfig{Name: "a"}, Store: newMemStore(), Sched: sched, Est: &fakeEst{}, Prober: pr, NoListen: true,
		Dirs: Dirs{State: t.TempDir()},
		ScopeCgroup: func(pid int, scope string) string {
			resolved[scope]++
			switch scope {
			case "kran-local-j1.scope":
				return "/sys/fs/cgroup/kran.slice/kran-local-j1.scope"
			case "kran-local-j3.scope":
				return "/sys/fs/cgroup/kran.slice/kran-local-j3.scope" // gone: not readable
			}
			return ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	add := func(id string, pid int, scope string) {
		e := d.jobs.newEntry(proto.Job{Spec: proto.JobSpec{ID: id}, Local: true, State: proto.JobRunning}, nil)
		e.pid, e.pgid, e.scope, e.started = pid, pid, scope, true
		d.jobs.add(e)
	}
	add("j1", 4001, "kran-local-j1.scope") // in a scope
	add("j2", 4002, "")                    // no systemd: process group
	add("j3", 4003, "kran-local-j3.scope") // scope unreadable: process group
	d.sampleLocalJobs()
	d.sampleLocalJobs()
	sched.mu.Lock()
	got := map[string]proto.Resources{}
	for k, v := range sched.usage {
		got[k] = v
	}
	sched.mu.Unlock()
	if got["j1"].Mem != 6<<30 || got["j1"].CPUs != 7.5 {
		t.Errorf("j1 (scope) live %+v, want the scope's 6 GiB and 7.5 CPUs", got["j1"])
	}
	if got["j2"].Mem != 1<<30 || got["j3"].Mem != 2<<30 {
		t.Errorf("without a readable scope the process group counts: j2 %+v j3 %+v", got["j2"], got["j3"])
	}
	for _, ref := range pr.asked {
		if ref.PGID == 4001 {
			t.Errorf("j1 was measured by its process group although its scope was readable")
		}
	}
	if resolved["kran-local-j1.scope"] != 1 {
		t.Errorf("the scope's cgroup was looked up %d times, want once (it is kept)", resolved["kran-local-j1.scope"])
	}
	if e := d.jobs.get("j1"); e == nil || e.snapshot().Live == nil || e.snapshot().Live.Mem != 6<<30 {
		t.Errorf("kran jobs does not show the scope's usage for j1")
	}
}
