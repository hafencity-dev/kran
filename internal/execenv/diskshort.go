package execenv

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// A command that fails because a disk it writes to ran full looks like any
// other failure (exit 1, often with the error cut off by `| tail`). The
// supervisor marks such failures (proto.Exit.DiskShort, a reason starting
// "disk short:") so callers can rerun them instead of counting a red test.

// diskShortFree: a disk with less free than this when a failed job ended
// counts as full (kran refuses to start jobs below its 10 GiB floor, so
// the job or its neighbours filled it while it ran).
const diskShortFree = 1 << 30

var enospcMarks = [][]byte{[]byte("No space left on device"), []byte("no space left on device"), []byte("ENOSPC")}

// enospcScan looks for an ENOSPC message in one output stream, also across
// chunk boundaries.
type enospcScan struct{ tail []byte }

func (e *enospcScan) see(b []byte) bool {
	const keep = 32 // longer than every mark
	buf := append(e.tail, b...)
	found := false
	for _, m := range enospcMarks {
		if bytes.Contains(buf, m) {
			found = true
			break
		}
	}
	if len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	e.tail = append(e.tail[:0], buf...)
	return found
}

// diskShortReason explains why a failed job's disk counts as short, or "".
func (s *supervisor) diskShortReason() string {
	if s.enospc.Load() {
		return fmt.Sprintf("disk short: the job's output says no space left on device on %s; rerun it", s.spec.Node)
	}
	for _, p := range s.spec.DiskPaths {
		if free, _, err := diskSpace(p); err == nil && free < diskShortFree {
			return fmt.Sprintf("disk short: the disk on %s had %s free when the job failed; rerun it", s.spec.Node, fmtGiB(free))
		}
	}
	if len(s.spec.DiskExec) > 0 && s.spec.Docker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.spec.Docker.CLI, s.spec.DiskExec...)
		cmd.Env = s.spec.Docker.Env
		if out, err := cmd.Output(); err == nil {
			if free, _, err := parseDF(string(out)); err == nil && free < diskShortFree {
				return fmt.Sprintf("disk short: the Docker disk on %s had %s free when the job failed; rerun it", s.spec.Node, fmtGiB(free))
			}
		}
	}
	return ""
}
