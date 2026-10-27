// Package sysd holds what kran needs to know about systemd-run.
package sysd

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// NoExpand returns the systemd-run flag that keeps a command line as given:
// systemd 254 added --expand-environment, and newer releases expand "$VAR"
// and turn "$$" into "$" in the command by default, so `sh -c 'kill $$'` or
// `echo $HOME` would reach the job mangled. Older systemd-run has no such
// flag (and does not expand): then nil. Probed once per process.
func NoExpand() []string {
	expandOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, "systemd-run", "--help").Output()
		if strings.Contains(string(out), "--expand-environment") {
			expandFlag = []string{"--expand-environment=no"}
		}
	})
	return expandFlag
}

var (
	expandOnce sync.Once
	expandFlag []string
)
