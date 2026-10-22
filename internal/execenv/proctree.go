package execenv

import (
	"os"
	"strings"
	"syscall"
)

// procInfo is one row of the process table.
type procInfo struct {
	pid, ppid, pgid int
	rss             int64   // bytes
	cpu             float64 // user+system seconds
	start           uint64  // start time token (platform units), guards against pid reuse
}

// descendants returns root and every process below it (by ppid).
func descendants(procs []procInfo, root int) []procInfo {
	children := map[int][]int{}
	byPID := make(map[int]procInfo, len(procs))
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p.pid)
		byPID[p.pid] = p
	}
	var out []procInfo
	seen := map[int]bool{}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := byPID[pid]; ok {
			out = append(out, p)
		}
		queue = append(queue, children[pid]...)
	}
	return out
}

// jobProcs returns the processes belonging to a job tree in pgid mode: the
// descendants of root, members of its process group, and (sweep) any
// process carrying the job's KRAN_JOB_ID in its environment — the marker
// catches daemons that escaped with setsid and double fork. self is never
// included.
func jobProcs(root, pgid int, jobID string, sweep bool) []procInfo {
	procs, err := listProcs()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	set := map[int]procInfo{}
	for _, p := range descendants(procs, root) {
		set[p.pid] = p
	}
	marker := "KRAN_JOB_ID=" + jobID
	uid := os.Getuid()
	for _, p := range procs {
		if pgid > 0 && p.pgid == pgid {
			set[p.pid] = p
		}
		if sweep && jobID != "" {
			if _, ok := set[p.pid]; !ok && p.pid != self && procOwnedBy(p.pid, uid) && procEnvHas(p.pid, marker) {
				set[p.pid] = p
			}
		}
	}
	delete(set, self)
	delete(set, 0)
	delete(set, 1)
	out := make([]procInfo, 0, len(set))
	for _, p := range set {
		out = append(out, p)
	}
	return out
}

func treeUsage(ps []procInfo) (rss int64, cpu float64) {
	for _, p := range ps {
		rss += p.rss
		cpu += p.cpu
	}
	return
}

func signalProcs(ps []procInfo, sig syscall.Signal) {
	for _, p := range ps {
		syscall.Kill(p.pid, sig)
	}
}

func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func procEnvHas(pid int, kv string) bool {
	for _, e := range procEnviron(pid) {
		if e == kv {
			return true
		}
	}
	return false
}

func procEnvValue(pid int, key string) string {
	for _, e := range procEnviron(pid) {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}
