package execenv

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// clkTck is USER_HZ; 100 on every Linux architecture Go supports.
const clkTck = 100

func listProcs() ([]procInfo, error) {
	d, err := os.Open("/proc")
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return nil, err
	}
	page := int64(os.Getpagesize())
	out := make([]procInfo, 0, len(names))
	for _, n := range names {
		pid, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		if p, ok := readStat(pid, page); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func readStat(pid int, page int64) (procInfo, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procInfo{}, false
	}
	// comm may contain spaces and parens; fields start after the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return procInfo{}, false
	}
	f := strings.Fields(string(b[i+2:]))
	// f[0]=state f[1]=ppid f[2]=pgrp ... f[11]=utime f[12]=stime f[19]=starttime f[21]=rss
	if len(f) < 22 {
		return procInfo{}, false
	}
	atoi := func(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
	if f[0] == "Z" {
		return procInfo{}, false // zombies hold no resources and cannot be killed
	}
	return procInfo{
		pid:   pid,
		ppid:  int(atoi(f[1])),
		pgid:  int(atoi(f[2])),
		cpu:   float64(atoi(f[11])+atoi(f[12])) / clkTck,
		start: uint64(atoi(f[19])),
		rss:   atoi(f[21]) * page,
	}, true
}

func procOwnedBy(pid, uid int) bool {
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/"+strconv.Itoa(pid), &st); err != nil {
		return false
	}
	return int(st.Uid) == uid
}

func procEnviron(pid int) []string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range bytes.Split(b, []byte{0}) {
		if len(e) > 0 {
			out = append(out, string(e))
		}
	}
	return out
}
