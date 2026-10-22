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
	MemCurrent int64   // memory.current
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
	return proto.Resources{CPUs: cores, Mem: u.MemCurrent}, nil
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
