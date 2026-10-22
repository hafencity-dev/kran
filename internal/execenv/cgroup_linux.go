package execenv

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

// jobCgroup is the job's own cgroup inside the supervisor's delegated
// systemd scope:
//
//	kran.slice/kran-job-<id>.scope/          (Delegate=yes)
//	├── supervisor/   the supervisor process
//	└── job/          the job tree: memory cap, OOM group kill, usage
//
// Every process the job creates stays in job/ (setsid and double fork do
// not leave a cgroup), so killing the cgroup kills the whole tree.
type jobCgroup struct {
	dir string
	fd  int // O_PATH fd of dir, for clone3 CLONE_INTO_CGROUP
}

// ownCgroup returns this process's cgroup v2 directory.
func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join(cgroupRoot, p), nil
		}
	}
	return "", errors.New("not on the unified cgroup hierarchy")
}

// setupJobCgroup moves the supervisor into supervisor/ and creates job/
// with the memory cap. scope is the expected scope unit name; a mismatch
// means we are not in our own delegated scope and must not touch cgroups.
func setupJobCgroup(scope string, memMax int64) (*jobCgroup, error) {
	base, err := ownCgroup()
	if err != nil {
		return nil, err
	}
	if filepath.Base(base) != scope {
		return nil, fmt.Errorf("running in %s, not in scope %s", base, scope)
	}
	sup := filepath.Join(base, "supervisor")
	if err := os.Mkdir(sup, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(sup, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return nil, fmt.Errorf("move supervisor: %w", err)
	}
	ctrl, _ := os.ReadFile(filepath.Join(base, "cgroup.controllers"))
	var enable []string
	for _, c := range strings.Fields(string(ctrl)) {
		if c == "memory" || c == "cpu" || c == "pids" {
			enable = append(enable, "+"+c)
		}
	}
	if len(enable) > 0 {
		if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte(strings.Join(enable, " ")), 0); err != nil {
			return nil, fmt.Errorf("enable controllers: %w", err)
		}
	}
	job := filepath.Join(base, "job")
	if err := os.Mkdir(job, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if memMax > 0 {
		if err := os.WriteFile(filepath.Join(job, "memory.max"), []byte(strconv.FormatInt(memMax, 10)), 0); err != nil {
			return nil, fmt.Errorf("memory cap: %w", err)
		}
		// Without this the cap only pushes the job into swap; with it the
		// job is OOM-killed at the cap, which is what the user is told.
		os.WriteFile(filepath.Join(job, "memory.swap.max"), []byte("0"), 0)
		// Kill the whole job on OOM rather than one random process.
		os.WriteFile(filepath.Join(job, "memory.oom.group"), []byte("1"), 0)
	}
	fd, err := unix.Open(job, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &jobCgroup{dir: job, fd: fd}, nil
}

func (c *jobCgroup) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(c.dir, name))
	return strings.TrimSpace(string(b))
}

func (c *jobCgroup) keyed(name, key string) int64 {
	for _, line := range strings.Split(c.read(name), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			return v
		}
	}
	return 0
}

func (c *jobCgroup) pids() []int {
	var out []int
	for _, f := range strings.Fields(c.read("cgroup.procs")) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func (c *jobCgroup) populated() bool { return c.keyed("cgroup.events", "populated") != 0 }

func (c *jobCgroup) signal(sig syscall.Signal) {
	for _, pid := range c.pids() {
		syscall.Kill(pid, sig)
	}
}

// kill SIGKILLs everything in the cgroup (cgroup.kill, Linux 5.14+, falls
// back to signalling each member) and waits until it is empty.
func (c *jobCgroup) kill(wait time.Duration) {
	if os.WriteFile(filepath.Join(c.dir, "cgroup.kill"), []byte("1"), 0) != nil {
		c.signal(syscall.SIGKILL)
	}
	deadline := time.Now().Add(wait)
	for c.populated() && time.Now().Before(deadline) {
		c.signal(syscall.SIGKILL)
		time.Sleep(20 * time.Millisecond)
	}
}

type cgUsage struct {
	mem, peak int64
	cpu       float64 // seconds
	oomKills  int64
}

func (c *jobCgroup) usage() cgUsage {
	u := cgUsage{cpu: float64(c.keyed("cpu.stat", "usage_usec")) / 1e6}
	u.mem, _ = strconv.ParseInt(c.read("memory.current"), 10, 64)
	u.peak, _ = strconv.ParseInt(c.read("memory.peak"), 10, 64)
	u.oomKills = c.keyed("memory.events", "oom_kill") + c.keyed("memory.events", "oom_group_kill")
	return u
}

func (c *jobCgroup) close() {
	unix.Close(c.fd)
	os.Remove(c.dir)
}
