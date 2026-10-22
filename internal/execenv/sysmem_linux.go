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
