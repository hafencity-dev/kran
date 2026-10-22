package execenv

import (
	"os/exec"
	"syscall"
)

func (s *supervisor) startCmd(cmd *exec.Cmd) error { return cmd.Start() }

func maxRSS(ru *syscall.Rusage) int64 { return ru.Maxrss } // bytes on macOS
