package execenv

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestRemoteExitReasonsWithoutCap: a remote job has a memory cap only from
// an explicit --mem (SPEC §6.8). Without one, an OOM kill in its cgroup or
// container (the node or its Linux VM ran out) and a signal nobody asked
// for through kran are named in the reason; a cap that was hit says it
// came from --mem; a signal the origin passed on stays quiet.
func TestRemoteExitReasonsWithoutCap(t *testing.T) {
	s := &supervisor{spec: superSpec{Node: "gx10"}}
	ex := s.mainExit(stepResult{code: 143, signal: "TERM", oom: true})
	if ex.State != proto.JobFailed || ex.Code != 143 ||
		ex.Reason != "the kernel's out-of-memory killer ended a process of the job on gx10: gx10 ran out of memory (the job has no memory cap of its own)" {
		t.Errorf("OOM without a cap: %+v", ex)
	}
	if ex := s.mainExit(stepResult{code: 137, signal: "KILL"}); ex.Reason != "job ended by signal KILL on gx10" {
		t.Errorf("signal: %+v", ex)
	}
	if ex := s.mainExit(stepResult{code: 1}); ex.Reason != "" {
		t.Errorf("a plain failure: %+v", ex)
	}
	if ex := s.mainExit(stepResult{code: 141, signal: "PIPE"}); ex.Reason != "" {
		t.Errorf("a closed pipe: %+v", ex)
	}
	s.sent = map[string]bool{"INT": true}
	if ex := s.mainExit(stepResult{code: 130, signal: "INT"}); ex.Reason != "" {
		t.Errorf("Ctrl-C passed on by kran: %+v", ex)
	}
	s.spec.Setup = &setupStep{Run: "pnpm install"}
	if ex := s.setupExit(stepResult{code: 137, oom: true}); !strings.Contains(ex.Reason, "out-of-memory killer ended setup `pnpm install` on gx10") {
		t.Errorf("setup OOM without a cap: %+v", ex)
	}

	capped := &supervisor{spec: superSpec{Node: "gx10", MemMax: 2 << 30}}
	if ex := capped.mainExit(stepResult{code: 137, signal: "KILL", oom: true}); ex.Reason != "job hit its 2 GiB memory cap on gx10 (from --mem) — rerun with --mem 4G" {
		t.Errorf("cap: %+v", ex)
	}

	vm := &supervisor{spec: superSpec{Node: "devmac", Docker: &dockerStep{CLI: "docker"}}}
	want := "devmac ran out of memory"
	if runtime.GOOS != "linux" {
		want = "devmac's Linux VM ran out of memory"
	}
	if ex := vm.mainExit(stepResult{code: 137, oom: true}); !strings.Contains(ex.Reason, want) {
		t.Errorf("container: %+v", ex)
	}
}

func TestSigNameNamesEverySignal(t *testing.T) {
	if n := sigName(11); n != "SEGV" {
		t.Errorf("signal 11 = %q, want SEGV", n)
	}
}

// TestJobMemCapPerKind (review 117, B3): which target-side job gets which
// cap. --mem always caps (twice it, at least 2 GiB). Without --mem a native
// job in its scope under kran.slice gets none (the slice bounds all kran
// jobs together); a container job (a Mac's Docker VM, or Docker's own
// cgroup on Linux) and a native job with no slice above it (macOS, Linux
// without a systemd user session) keep the estimate-based default.
func TestJobMemCapPerKind(t *testing.T) {
	const avail = 100 * gib
	for _, c := range []struct {
		name     string
		explicit bool
		rt       proto.RuntimeKind
		inSlice  bool
		want     int64
		def      bool
	}{
		{"native in kran.slice, no --mem", false, proto.RuntimeNative, true, 0, false},
		{"native in kran.slice, --mem 3G", true, proto.RuntimeNative, true, 6 * gib, false},
		{"native without a slice (macOS), no --mem", false, proto.RuntimeNative, false, 8 * gib, true},
		{"container, no --mem", false, proto.RuntimeContainer, false, 8 * gib, true},
		{"container on Linux with scopes, no --mem", false, proto.RuntimeContainer, true, 8 * gib, true},
		{"container, --mem 3G", true, proto.RuntimeContainer, false, 6 * gib, false},
	} {
		got, def := jobMemCap(3*gib, avail, c.explicit, c.rt, c.inSlice)
		if got != c.want || def != c.def {
			t.Errorf("%s: cap %d default %v, want %d %v", c.name, got, def, c.want, c.def)
		}
	}
	if got, _ := jobMemCap(6*gib, avail, false, proto.RuntimeContainer, false); got != 12*gib {
		t.Errorf("container default of a 6 GiB estimate: %d, want 12 GiB", got)
	}
	if got, _ := jobMemCap(6*gib, 9*gib, false, proto.RuntimeContainer, false); got != 9*gib {
		t.Errorf("default cap bounded by free memory: %d", got)
	}
}

// TestDefaultCapReasonDoesNotBlameMem: a job that hit the node's default
// cap (no --mem) is told so and how to give it more, never "(from --mem)".
func TestDefaultCapReasonDoesNotBlameMem(t *testing.T) {
	s := &supervisor{spec: superSpec{Node: "devmac", MemMax: 8 << 30, MemDefault: true}}
	ex := s.mainExit(stepResult{code: 137, signal: "KILL", oom: true})
	if strings.Contains(ex.Reason, "from --mem") || !strings.Contains(ex.Reason, "job hit its 8 GiB memory cap on devmac (the node's default for a job without --mem") ||
		!strings.Contains(ex.Reason, "rerun with --mem 16G") {
		t.Errorf("%q", ex.Reason)
	}
	s.spec.Setup = &setupStep{Run: "pnpm install"}
	if ex := s.setupExit(stepResult{code: 137, oom: true}); strings.Contains(ex.Reason, "from --mem") || !strings.Contains(ex.Reason, "default for a job without --mem") {
		t.Errorf("setup: %q", ex.Reason)
	}
}

// TestNativeJobWithoutSliceKeepsDefaultCap: a native job started where no
// systemd scope puts it under kran.slice (the test manager runs without
// scopes, as a Mac does) gets the default cap in its supervisor spec; with
// --mem the explicit one.
func TestNativeJobWithoutSliceKeepsDefaultCap(t *testing.T) {
	te := newTestEnv(t)
	if te.m.systemdRun != "" {
		t.Skip("this manager runs jobs in scopes")
	}
	ref, err := te.m.AcquireSlot(context.Background(), testRepo, "wt", proto.RuntimeNative)
	if err != nil {
		t.Fatal(err)
	}
	spec := func(p Process) superSpec {
		var s superSpec
		if err := readJSON(filepath.Join(te.dir, "jobs", p.JobID(), fileSpec), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	p := te.start(ref, sh("true"), func(r *StartRequest) { r.Alloc.Mem = gib })
	waitExit(t, p, 30*time.Second)
	if s := spec(p); s.MemMax < gib || !s.MemDefault {
		t.Errorf("no --mem, no slice: cap %d default %v, want the default cap", s.MemMax, s.MemDefault)
	}
	te.m.ReleaseSlot(ref)
	ref, _ = te.m.AcquireSlot(context.Background(), testRepo, "wt", proto.RuntimeNative)
	p = te.start(ref, sh("true"), func(r *StartRequest) { r.Alloc.Mem = gib; r.Spec.Req.Mem = gib })
	waitExit(t, p, 30*time.Second)
	if s := spec(p); s.MemMax < 2*gib || s.MemDefault {
		t.Errorf("--mem 1G: cap %d default %v, want the explicit cap", s.MemMax, s.MemDefault)
	}
}
