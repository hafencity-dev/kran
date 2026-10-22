//go:build !linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// psReader uses `ps -o ppid=,comm= -p PID` (macOS has no /proc).
type psReader struct{}

func newProcReader() ProcReader { return psReader{} }

func (psReader) Proc(pid int) (Proc, error) {
	out, err := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return Proc{}, err
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) < 2 {
		return Proc{}, fmt.Errorf("ps: no process %d", pid)
	}
	ppid, err := strconv.Atoi(f[0])
	if err != nil {
		return Proc{}, err
	}
	comm := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), f[0]))
	p := Proc{PID: pid, PPID: ppid, Comm: comm}
	if interpreters[baseName(comm)] {
		if a, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output(); err == nil {
			p.Args = strings.Fields(string(a))
		}
	}
	return p, nil
}

func ttyName() string {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return ""
	}
	defer f.Close()
	out, err := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return ""
	}
	t := strings.TrimSpace(string(out))
	if t == "?" || t == "??" {
		return ""
	}
	return t
}
