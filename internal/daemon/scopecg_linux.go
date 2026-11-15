//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hafencity-dev/kran/internal/metrics"
)

// scopeCgroup returns the cgroup v2 directory of scope as seen from its
// main process pid ("" when pid is not in that scope: gone, reused, or
// systemd-run has not moved it there yet).
func scopeCgroup(pid int, scope string) string {
	return scopeCgroupFrom(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"), scope)
}

func scopeCgroupFrom(procCgroup, scope string) string {
	b, err := os.ReadFile(procCgroup)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		p, ok := strings.CutPrefix(line, "0::")
		if !ok || !filepath.IsAbs(p) {
			continue
		}
		// The job's process may sit in a child cgroup of the scope.
		for dir := filepath.Clean(p); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
			if filepath.Base(dir) == scope {
				return filepath.Join(metrics.CgroupRoot, dir)
			}
		}
	}
	return ""
}
