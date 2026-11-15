//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScopeCgroupFromProc(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "cgroup")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const scope = "kran-local-0hk9rn22sb.scope"
	in := "0::/user.slice/user-1000.slice/user@1000.service/kran.slice/" + scope + "\n"
	if got := scopeCgroupFrom(write(in), scope); got != "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/kran.slice/"+scope {
		t.Errorf("in the scope: %q", got)
	}
	// A child cgroup of the scope still names the scope.
	if got := scopeCgroupFrom(write("0::/kran.slice/"+scope+"/inner\n"), scope); got != "/sys/fs/cgroup/kran.slice/"+scope {
		t.Errorf("below the scope: %q", got)
	}
	// systemd-run has not moved it yet, or the pid now belongs to another process.
	if got := scopeCgroupFrom(write("0::/user.slice/user-1000.slice/session-3.scope\n"), scope); got != "" {
		t.Errorf("elsewhere: %q", got)
	}
	if got := scopeCgroupFrom(filepath.Join(dir, "missing"), scope); got != "" {
		t.Errorf("gone: %q", got)
	}
}
