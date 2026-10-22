package execenv

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func memTotal() int64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(v)
}

var vmStatRe = regexp.MustCompile(`Pages (free|inactive|speculative|purgeable):\s+(\d+)`)

// memAvailable approximates reclaimable memory like Activity Monitor:
// free + inactive + speculative + purgeable pages.
func memAvailable() int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return 0
	}
	page := int64(unix.Getpagesize())
	if m := regexp.MustCompile(`page size of (\d+) bytes`).FindSubmatch(out); m != nil {
		page, _ = strconv.ParseInt(string(m[1]), 10, 64)
	}
	var pages int64
	for _, m := range vmStatRe.FindAllSubmatch(out, -1) {
		v, _ := strconv.ParseInt(string(m[2]), 10, 64)
		pages += v
	}
	return pages * page
}
