package execenv

import (
	"os"
	"strconv"
	"strings"
)

func meminfo(key string) int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, key+":"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				v, _ := strconv.ParseInt(f[0], 10, 64)
				return v * 1024
			}
		}
	}
	return 0
}

func memTotal() int64     { return meminfo("MemTotal") }
func memAvailable() int64 { return meminfo("MemAvailable") }

// hostPipePages reads this kernel's fs.pipe-user-pages-soft (0 = unknown).
func hostPipePages() int64 {
	b, err := os.ReadFile("/proc/sys/fs/pipe-user-pages-soft")
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}
