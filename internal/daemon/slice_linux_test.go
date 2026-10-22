//go:build linux

package daemon

import (
	"context"
	"strings"
	"testing"
)

func TestSliceMax(t *testing.T) {
	const G = int64(1 << 30)
	cases := []struct{ cur, avail, total, reserve, want int64 }{
		{0, 100 * G, 120 * G, 8 * G, 92 * G},
		{10 * G, 100 * G, 120 * G, 8 * G, 102 * G},
		{50 * G, 100 * G, 120 * G, 8 * G, 112 * G}, // capped at total − reserve
		{0, 4 * G, 120 * G, 8 * G, G},              // floor
	}
	for _, c := range cases {
		if got := sliceMax(c.cur, c.avail, c.total, c.reserve); got != c.want {
			t.Errorf("sliceMax(%d,%d,%d,%d) = %d, want %d", c.cur/G, c.avail/G, c.total/G, c.reserve/G, got/G, c.want/G)
		}
	}
}

func TestSliceRefreshSkipsSmallChanges(t *testing.T) {
	var calls []string
	s := &systemdSlice{reserve: 8 << 30, logf: t.Logf, available: true,
		run: func(ctx context.Context, args ...string) (string, error) {
			if args[0] == "set-property" {
				calls = append(calls, strings.Join(args, " "))
			}
			if args[0] == "show" {
				return "/user.slice/kran.slice", nil
			}
			return "", nil
		},
		readFile: func(string) ([]byte, error) { return []byte("0\n"), nil },
	}
	avail := int64(40 << 30)
	s.meminfo = func() (int64, int64, error) { return 64 << 30, avail, nil }
	s.Refresh()
	avail += 10 << 20 // tiny change: no systemd call
	s.Refresh()
	avail += 1 << 30
	s.Refresh()
	if len(calls) != 2 || !strings.Contains(calls[0], "MemoryMax=34359738368") || !strings.Contains(calls[0], "MemoryHigh=infinity") {
		t.Fatalf("calls %q", calls)
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail, err := parseMeminfo([]byte("MemTotal:       131072 kB\nMemFree: 1 kB\nMemAvailable:    65536 kB\n"))
	if err != nil || total != 131072*1024 || avail != 65536*1024 {
		t.Fatalf("%d %d %v", total, avail, err)
	}
}
