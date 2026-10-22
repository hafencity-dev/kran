package execenv

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// startCmd starts cmd directly inside the job cgroup (clone3 with
// CLONE_INTO_CGROUP), so not even the first instruction of the job runs
// outside it. Kernels or sandboxes without clone3 fall back to moving the
// process right after start.
func (s *supervisor) startCmd(cmd *exec.Cmd) error {
	if s.cg == nil {
		return cmd.Start()
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = s.cg.fd
	err := cmd.Start()
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOEXEC) {
		return err
	}
	s.logf("start in cgroup failed (%v); starting normally", err)
	cmd.SysProcAttr.UseCgroupFD = false
	cmd.SysProcAttr.CgroupFD = 0
	cmd.Process, cmd.ProcessState = nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(s.cg.dir, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0)
	return nil
}

func maxRSS(ru *syscall.Rusage) int64 { return ru.Maxrss * 1024 } // KiB on Linux
