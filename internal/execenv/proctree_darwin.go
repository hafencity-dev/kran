package execenv

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// listProcs uses the kernel process table for pid/ppid/pgid (cheap, no
// exec) and ps for RSS and CPU time, which kinfo_proc does not carry.
func listProcs() ([]procInfo, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]procInfo, 0, len(kps))
	idx := make(map[int]int, len(kps))
	for _, k := range kps {
		pid := int(k.Proc.P_pid)
		if pid <= 0 || k.Proc.P_stat == 5 { // SZOMB
			continue
		}
		idx[pid] = len(out)
		out = append(out, procInfo{
			pid:   pid,
			ppid:  int(k.Eproc.Ppid),
			pgid:  int(k.Eproc.Pgid),
			start: uint64(k.Proc.P_starttime.Sec)*1e6 + uint64(k.Proc.P_starttime.Usec),
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,rss=,time=").Output()
	if err != nil {
		return out, nil // structure is still useful for kills
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		i, ok := idx[pid]
		if !ok {
			continue
		}
		rss, _ := strconv.ParseInt(f[1], 10, 64)
		out[i].rss = rss * 1024
		out[i].cpu = parsePSTime(f[2])
	}
	return out, nil
}

// parsePSTime parses ps "time": [[dd-]hh:]mm:ss[.cc].
func parsePSTime(s string) float64 {
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		v, _ := strconv.ParseFloat(d, 64)
		days, s = v, rest
	}
	parts := strings.Split(s, ":")
	total := 0.0
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		total = total*60 + v
	}
	return days*86400 + total
}

func procOwnedBy(pid, uid int) bool {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false
	}
	return int(k.Eproc.Ucred.Uid) == uid
}

// procEnviron reads a process's environment via kern.procargs2
// (same-user processes only; others yield nil).
func procEnviron(pid int) []string {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(b[:4]))
	b = b[4:]
	// exec path, then NUL padding
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return nil
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	parts := bytes.Split(b, []byte{0})
	if len(parts) <= argc {
		return nil
	}
	var out []string
	for _, e := range parts[argc:] {
		if len(e) == 0 {
			break
		}
		out = append(out, string(e))
	}
	return out
}
