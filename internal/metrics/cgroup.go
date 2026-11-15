package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// CgroupRoot is where cgroup v2 is mounted on Linux.
const CgroupRoot = "/sys/fs/cgroup"

// CgroupUsage is the cumulative accounting of a cgroup v2 group.
type CgroupUsage struct {
	MemCurrent int64 // memory.current
	// MemFile is the group's page cache on the file LRUs (memory.stat
	// active_file + inactive_file): memory the kernel can take back, which
	// MemAvailable already counts as available. shmem/tmpfs is not in it
	// (it sits on the anon LRUs and cannot simply be dropped).
	MemFile    int64
	MemPeak    int64   // memory.peak (0 when the kernel lacks it, < 5.19)
	CPUSeconds float64 // cpu.stat usage_usec
}

// CgroupStats reads a cgroup v2 group (absolute, or relative to
// CgroupRoot). The daemon uses it for a job's final Usage (peak memory,
// CPU seconds) when its scope ends.
func CgroupStats(path string) (CgroupUsage, error) {
	dir := cgroupDir(path)
	var u CgroupUsage
	cur, err := readInt(filepath.Join(dir, "memory.current"))
	if err != nil {
		return u, fmt.Errorf("cgroup %s: %w", dir, err)
	}
	u.MemCurrent = cur
	if b, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		u.MemFile = statKey(string(b), "active_file") + statKey(string(b), "inactive_file")
	}
	if peak, err := readInt(filepath.Join(dir, "memory.peak")); err == nil {
		u.MemPeak = peak
	}
	b, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return u, fmt.Errorf("cgroup %s: %w", dir, err)
	}
	usec, err := cgroupCPUUsec(string(b))
	if err != nil {
		return u, fmt.Errorf("cgroup %s: %w", dir, err)
	}
	u.CPUSeconds = float64(usec) / 1e6
	return u, nil
}

func cgroupDir(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(CgroupRoot, path)
}

func (p *prober) cgroupUsage(path string, now time.Time) (proto.Resources, error) {
	u, err := CgroupStats(path)
	if err != nil {
		return proto.Resources{}, err
	}
	// The group dir's ctime is its creation time: lets the first reading
	// report average cores instead of zero.
	age := 0.0
	if fi, err := os.Stat(cgroupDir(path)); err == nil {
		age = now.Sub(fi.ModTime()).Seconds()
		if c := ctimeOf(fi); !c.IsZero() {
			age = now.Sub(c).Seconds()
		}
	}
	cores := p.rates.cores("cg:"+path, u.CPUSeconds, now, age)
	return proto.Resources{CPUs: cores, Mem: u.MemUsed()}, nil
}

// MemUsed is what the group holds that the kernel cannot simply give back:
// memory.current without its file page cache (anonymous memory, shmem,
// kernel memory). The scheduler takes a running job's use off its
// reservation (SPEC §6.3); counting a build's page cache as use would book
// it as using what MemAvailable already shows as available.
func (u CgroupUsage) MemUsed() int64 { return max(0, u.MemCurrent-u.MemFile) }

// statKey reads one "key value" line of a cgroup stat file (0 if absent).
func statKey(s, key string) int64 {
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(line, key+" "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return -1, nil
	}
	return strconv.ParseInt(s, 10, 64)
}
