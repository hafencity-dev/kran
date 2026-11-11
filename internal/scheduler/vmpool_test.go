package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Container jobs in a VM (SPEC §6.3): devmac as measured on 2026-11-11
// (DEVMAC-UNDERUSE-20261111): 28 cores, 96 GiB, a Colima VM of 24 CPUs and
// 39.09 GiB guest memory whose 40 GB footprint is resident on the host (top
// shows "40G", read as 39 GiB), 32 GiB available inside the VM, memory
// status level 78.
const vmGuestMem = 41971261440 // docker info MemTotal of the 40 GiB VM

func devmacColima(now time.Time, hostAvail, resident int64) proto.NodeState {
	d := devmac(now)
	d.Info.Runtimes[1].CPUs = 24
	d.Info.Runtimes[1].Mem = vmGuestMem
	d.Metrics.MemAvailable = hostAvail
	d.Metrics.MemPressure = 22
	d.Metrics.CPUBusy = 0.14
	d.Metrics.Runtimes = map[proto.RuntimeKind]proto.RuntimeMetrics{
		proto.RuntimeContainer: {CPUBusy: 0.02, MemAvailable: 32 * gib, HostResident: resident},
	}
	return d
}

// devmacSched is devmac's own scheduler: the one whose Admit decides.
func devmacSched(clk *fakeClock, n proto.NodeState) *sched {
	s := New(Options{Origin: "devmac", Now: clk.Now}).(*sched)
	s.UpdateNode(self(n))
	return s
}

func admitErr(s *sched, id string, mem int64, kind proto.RuntimeKind) error {
	_, err := s.Admit(newJob(id, "x", 1, mem, 60), kind)
	return err
}

// runningFor records a running job (admitted earlier, when it fit) and lets
// it run past the ramp with live use, as the 17 GiB container build
// 0hfqv566gd did (8 GiB peak).
func runningFor(t *testing.T, clk *fakeClock, s *sched, n proto.NodeState, id string, kind proto.RuntimeKind, res, live int64) proto.NodeState {
	t.Helper()
	s.Restore(newJob(id, "x", 4, res, 3600), Placement{Node: "devmac", Runtime: kind, Alloc: proto.Resources{CPUs: 4, Mem: res}})
	clk.Advance(2 * time.Minute)
	n.Metrics.Time = clk.t
	s.UpdateNode(self(n))
	s.UpdateJobUsage(id, proto.Resources{CPUs: 4, Mem: live})
	return n
}

const paid = 39 * gib // the measured footprint, rounded down

func TestVMPoolAdmission(t *testing.T) {
	type step struct {
		id   string
		kind proto.RuntimeKind
		mem  int64
		ok   bool
	}
	cases := []struct {
		name     string
		hostFree int64 // host available above the 8 GiB reserve
		resident int64
		pressure float64 // memory pressure (100 − memorystatus level); 0 = the measured 22
		vmAvail  int64   // 0 = the measured 32 GiB
		running  int64   // a 17 GiB container job using this much (0 = none)
		steps    []step
	}{
		// (a) The measured failure: 32 GiB unused in the VM, the host shows
		// 0.3–5 GiB, and a 1.3 GiB container job waited 980 s.
		{name: "a: host 0.3 GiB, VM resident: 1.3 GiB container job admitted", hostFree: fgib(0.3), resident: paid,
			steps: []step{{"c", proto.RuntimeContainer, fgib(1.3), true}}},
		{name: "a: host 5 GiB, VM resident: 1.3 GiB container job admitted", hostFree: 5 * gib, resident: paid,
			steps: []step{{"c", proto.RuntimeContainer, fgib(1.3), true}}},
		{name: "a: before this rule (no reading) the same job is refused", hostFree: fgib(0.3),
			steps: []step{{"c", proto.RuntimeContainer, fgib(1.3), false}}},
		// (b) The 17 GiB container reservation (8 GiB used) no longer
		// blocks a native job that fits the host, nor a container job that
		// fits the VM's remaining room.
		{name: "b: 17 GiB container job running, native 12 GiB fits the host", hostFree: fgib(14.4), resident: paid, running: 8 * gib,
			steps: []step{{"n", proto.RuntimeNative, 12 * gib, true}}},
		{name: "b: 17 GiB container job running, container 12 GiB fits the VM", hostFree: fgib(14.4), resident: paid, running: 8 * gib,
			steps: []step{{"c", proto.RuntimeContainer, 12 * gib, true}}},
		{name: "b: before this rule the native job is refused", hostFree: fgib(14.4), running: 8 * gib,
			steps: []step{{"n", proto.RuntimeNative, 12 * gib, false}}},
		{name: "b: before this rule the container job is refused", hostFree: fgib(14.4), running: 8 * gib,
			steps: []step{{"c", proto.RuntimeContainer, 12 * gib, false}}},
		// (c) A native job is still refused when the host lacks memory,
		// while the VM takes a container job of the same size.
		{name: "c: host 2 GiB: native 4 GiB refused, container 4 GiB admitted", hostFree: 2 * gib, resident: paid,
			steps: []step{{"n", proto.RuntimeNative, 4 * gib, false}, {"c", proto.RuntimeContainer, 4 * gib, true}}},
		{name: "c: container jobs never make room for a native one", hostFree: 5 * gib, resident: paid,
			steps: []step{{"c1", proto.RuntimeContainer, 20 * gib, true}, {"n", proto.RuntimeNative, 4 * gib, true}, {"n2", proto.RuntimeNative, 2 * gib, false}}},
		// (d) A freshly booted VM: only what is resident earns credit.
		{name: "d: 12 of 39 GiB resident, host 5 GiB: 1.3 GiB container job admitted", hostFree: 5 * gib, resident: 12 * gib,
			steps: []step{{"c", proto.RuntimeContainer, fgib(1.3), true}}},
		{name: "d: 12 of 39 GiB resident, host 5 GiB: 6 GiB container job refused (VM may grow by 6)", hostFree: 5 * gib, resident: 12 * gib,
			steps: []step{{"c", proto.RuntimeContainer, 6 * gib, false}}},
		{name: "d: 36 GiB resident: container jobs count on the host only up to the unpaid 3.1 GiB", hostFree: 5 * gib, resident: 36 * gib,
			steps: []step{
				{"c1", proto.RuntimeContainer, 2 * gib, true}, // host share 2, room left 1.09
				{"n1", proto.RuntimeNative, fgib(1.9), true},  // host: 5 − 2 − 1.9 = 1.1 left
				{"c2", proto.RuntimeContainer, 4 * gib, true}, // host share 1.09 ≤ 1.1
				{"c3", proto.RuntimeContainer, 4 * gib, true}, // nothing unpaid left: host share 0
				{"n2", proto.RuntimeNative, fgib(0.1), false}, // the host is spoken for
			}},
		{name: "d: no reading (fresh boot): container job counts in full", hostFree: fgib(0.3),
			steps: []step{{"c", proto.RuntimeContainer, fgib(0.3), true}, {"c2", proto.RuntimeContainer, fgib(0.1), false}}},
		{name: "d: host under memory pressure: no credit", hostFree: fgib(0.3), resident: paid, pressure: 80,
			steps: []step{{"c", proto.RuntimeContainer, fgib(1.3), false}}},
		{name: "d: a footprint the host does not show in use earns no more than that", hostFree: 70 * gib, resident: paid,
			// host used 96 − 78 = 18 GiB caps the credit: 21 GiB unpaid
			steps: []step{{"c", proto.RuntimeContainer, 20 * gib, true}, {"n", proto.RuntimeNative, 51 * gib, false}, {"n2", proto.RuntimeNative, 50 * gib, true}}},
		// (e) The VM's compressed pages earn no credit: top's MEM (40G, read
		// as 39 GiB) holds about 10 GiB the host compressed or swapped, so
		// the resident part is 28 GiB (cmprs 10G read as 11) and a new job
		// writing into those guest pages needs real host memory.
		{name: "e: footprint fully resident, host 5 GiB: 6 GiB container job admitted", hostFree: 5 * gib, resident: paid,
			steps: []step{{"c", proto.RuntimeContainer, 6 * gib, true}}},
		{name: "e: 10 GiB of the footprint compressed: the same job is refused", hostFree: 5 * gib, resident: paid - 11*gib,
			steps: []step{{"c", proto.RuntimeContainer, 6 * gib, false}}},
		{name: "e: 10 GiB compressed, a job within the host's room is admitted", hostFree: 5 * gib, resident: paid - 11*gib,
			steps: []step{{"c", proto.RuntimeContainer, 4 * gib, true}}},
		// (f) The VM itself full: container jobs refused however much the
		// host has.
		{name: "f: VM full, host plenty: container job refused", hostFree: 60 * gib, resident: paid, vmAvail: 2 * gib,
			steps: []step{{"c", proto.RuntimeContainer, 4 * gib, false}, {"n", proto.RuntimeNative, 4 * gib, true}}},
		{name: "f: VM ledger full: second 20 GiB container job refused", hostFree: 60 * gib, resident: paid,
			steps: []step{{"c1", proto.RuntimeContainer, 20 * gib, true}, {"c2", proto.RuntimeContainer, 20 * gib, false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := &fakeClock{t0}
			n := devmacColima(t0, 8*gib+tc.hostFree, tc.resident)
			if tc.pressure > 0 {
				n.Metrics.MemPressure = tc.pressure
			}
			if tc.vmAvail > 0 {
				rm := n.Metrics.Runtimes[proto.RuntimeContainer]
				rm.MemAvailable = tc.vmAvail
				n.Metrics.Runtimes[proto.RuntimeContainer] = rm
			}
			s := devmacSched(clk, n)
			if tc.running > 0 {
				runningFor(t, clk, s, n, "0hfqv566gd", proto.RuntimeContainer, 17*gib, tc.running)
			}
			for _, st := range tc.steps {
				err := admitErr(s, st.id, st.mem, st.kind)
				if st.ok && err != nil {
					t.Errorf("%s %s %s: refused: %v", st.id, st.kind, fmtBytes(st.mem), err)
				}
				if !st.ok && err == nil {
					t.Errorf("%s %s %s: admitted, want refused", st.id, st.kind, fmtBytes(st.mem))
				}
				var ae *AdmitError
				if err != nil && (!errors.As(err, &ae) || ae.Code != "busy") {
					t.Errorf("%s: want busy, got %v", st.id, err)
				}
			}
		})
	}
}

// (e) A Linux node has no VM pool: its container runtime shares the host
// pool, and a host_resident it might report changes nothing.
func TestVMPoolLinuxNodeUnchanged(t *testing.T) {
	for _, resident := range []int64{0, 100 * gib} {
		clk := &fakeClock{t0}
		g := gx10(t0)
		g.Metrics.MemAvailable = 10 * gib // 2 GiB above the reserve
		g.Metrics.Runtimes = map[proto.RuntimeKind]proto.RuntimeMetrics{
			proto.RuntimeContainer: {MemAvailable: 10 * gib, HostResident: resident},
		}
		s := newTestSched(t, clk, self(g))
		var ae *AdmitError
		if _, err := s.Admit(newJob("c", "x", 1, 4*gib, 60), proto.RuntimeContainer); !errors.As(err, &ae) ||
			ae.Reason != "gx10: needs 1 CPU and 4.0 GiB, has 18 CPUs and 2.0 GiB free" {
			t.Errorf("resident %s: %v", fmtBytes(resident), err)
		}
		if _, err := s.Admit(newJob("c2", "x", 1, 2*gib, 60), proto.RuntimeContainer); err != nil {
			t.Errorf("resident %s: %v", fmtBytes(resident), err)
		}
		snap := s.Snapshot()
		if _, ok := snap.FreeVM["gx10"]; ok {
			t.Errorf("a Linux node has no VM pool: %+v", snap.FreeVM)
		}
		if f := snap.Free["gx10"]; f.Mem != 0 {
			t.Errorf("host free %s, want 0", fmtBytes(f.Mem))
		}
	}
}

// The origin's view of a peer follows the same rule: the measured 1.3 GiB
// container job (0hfrbejnc3) goes to devmac instead of waiting for the host.
func TestVMPoolOriginPlacesContainerJobOnPaidVM(t *testing.T) {
	clk := &fakeClock{t0}
	g := self(gx10(t0))
	g.Metrics.MemAvailable = 8 * gib // gx10 itself is full
	d := devmacColima(t0, 8*gib+fgib(0.33), 0)
	s := newTestSched(t, clk, g, d)
	tk := s.Submit(newJob("0hfrbejnc3", "a", 2, fgib(1.3), 120))
	pending(t, tk)
	if r := reason(s, "0hfrbejnc3"); !strings.Contains(r, "devmac has 337 MiB free") {
		t.Errorf("before the reading: reason %q", r)
	}
	d.Metrics.Runtimes[proto.RuntimeContainer] = proto.RuntimeMetrics{CPUBusy: 0.02, MemAvailable: 32 * gib, HostResident: paid}
	s.UpdateNode(d)
	pl := placed(t, tk)
	if pl.Node != "devmac" || pl.Runtime != proto.RuntimeContainer {
		t.Fatalf("%+v", pl)
	}
	// A 4 GiB container job on a full VM names the VM, not the host.
	d.Metrics.Runtimes[proto.RuntimeContainer] = proto.RuntimeMetrics{MemAvailable: 2 * gib, HostResident: paid}
	s.UpdateNode(d)
	pending(t, s.Submit(newJob("big", "a", 2, 4*gib, 120)))
	if r := reason(s, "big"); !strings.Contains(r, "devmac's Linux VM has") {
		t.Errorf("reason %q", r)
	}
}

// kran status: the node's two pools apart.
func TestVMPoolSnapshotShowsBothPools(t *testing.T) {
	clk := &fakeClock{t0}
	s := devmacSched(clk, devmacColima(t0, 8*gib+fgib(0.3), paid))
	snap := s.Snapshot()
	host, vm := snap.Free["devmac"], snap.FreeVM["devmac"]
	if host.Mem != fgib(0.3) {
		t.Errorf("host free %s, want 307 MiB", fmtBytes(host.Mem))
	}
	if vm.Mem != 32*gib || vm.CPUs < 23 {
		t.Errorf("VM free %+v, want 32 GiB and about 23.5 CPUs", vm)
	}
	// Without a reading the host limits container jobs too.
	s = devmacSched(clk, devmacColima(t0, 8*gib+fgib(0.3), 0))
	if vm := s.Snapshot().FreeVM["devmac"]; vm.Mem != fgib(0.3) {
		t.Errorf("no credit: VM free %s, want the host's 307 MiB", fmtBytes(vm.Mem))
	}
}

// Relieving a busy origin keeps half the VM free for a container job, not
// half the host (which the paid VM itself takes).
func TestVMPoolRelieveCountsTheVM(t *testing.T) {
	clk := &fakeClock{t0}
	s := devmacSched(clk, devmacColima(t0, 22*gib, paid))
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.view("devmac", clk.t)
	ctr := rtView{rt: v.e.st.Info.Runtimes[1], vm: true}
	if !v.keepsSpare(ctr, 4, 8*gib, relieveSpare) {
		t.Error("VM credit: an 8 GiB container job keeps half the VM free")
	}
	if v.keepsSpare(ctr, 4, 20*gib, relieveSpare) {
		t.Error("a 20 GiB container job takes more than half the VM")
	}
	if v.keepsSpare(rtView{rt: v.e.st.Info.Runtimes[0]}, 4, 8*gib, relieveSpare) {
		t.Error("a native job still keeps half of the host")
	}
}
