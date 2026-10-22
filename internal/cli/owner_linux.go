//go:build linux

package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type procfsReader struct{ root string }

func newProcReader() ProcReader { return procfsReader{root: "/proc"} }

func (r procfsReader) Proc(pid int) (Proc, error) {
	dir := fmt.Sprintf("%s/%d", r.root, pid)
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return Proc{}, err
	}
	// "pid (comm) S ppid …": comm may contain spaces and parens.
	s := string(stat)
	open, closeP := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || closeP < open {
		return Proc{}, fmt.Errorf("unparsable %s/stat", dir)
	}
	fields := strings.Fields(s[closeP+1:])
	if len(fields) < 2 {
		return Proc{}, fmt.Errorf("short %s/stat", dir)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return Proc{}, err
	}
	p := Proc{PID: pid, PPID: ppid, Comm: s[open+1 : closeP]}
	if cmd, err := os.ReadFile(dir + "/cmdline"); err == nil && len(cmd) > 0 {
		p.Args = strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
		if exe, err := os.Readlink(dir + "/exe"); err == nil {
			p.Comm = exe
		}
	}
	return p, nil
}

func ttyName() string {
	l, err := os.Readlink("/proc/self/fd/0")
	if err != nil || !strings.HasPrefix(l, "/dev/") {
		return ""
	}
	return strings.TrimPrefix(l, "/dev/")
}
