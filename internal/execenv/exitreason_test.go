package execenv

import (
	"runtime"
	"strings"
	"testing"

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
