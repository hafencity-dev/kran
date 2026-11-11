package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestParseTopSizeRoundsDown(t *testing.T) {
	onePointFive := 1.5
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"40G", 39 << 30, true},
		{"2318M+", 2317 << 20, true},
		{"512K-", 511 << 10, true},
		{"1.5G", int64((onePointFive - 0.1) * (1 << 30)), true},
		{"0B", 0, true},
		{"1024B", 1024, true},
		{"1G*", 0, true},
		{"", 0, false},
		{"G", 0, false},
		{"12X", 0, false},
		{"-3G", 0, false},
	} {
		got, ok := parseTopSize(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseTopSize(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseTopRow(t *testing.T) {
	const hdr = "Processes: 600 total\n\nPID    MEM    CMPRS\n"
	for _, tc := range []struct {
		name    string
		in      string
		mem     int64
		cmprs   int64
		cmprsOK bool
		ok      bool
	}{
		{"both columns", hdr + "21401  40G    10G\n", 39 << 30, 11 << 30, true, true},
		{"plus suffix", hdr + "21401  39G+   9840M-\n", 38 << 30, 9841 << 20, true, true},
		{"megabytes", hdr + "21401  9840M   512K\n", 9839 << 20, 513 << 10, true, true},
		{"nothing compressed", hdr + "21401  40G    0B\n", 39 << 30, 0, true, true},
		{"cmprs not available", hdr + "21401  40G    N/A\n", 39 << 30, 0, false, true},
		{"cmprs garbage", hdr + "21401  40G    ??\n", 39 << 30, 0, false, true},
		{"cmprs column missing in header and row", "PID    MEM\n21401  40G\n", 39 << 30, 0, false, true},
		{"cmprs missing, no header", "21401  40G\n", 39 << 30, 0, false, true},
		{"no header, positional", "21401  40G  10G\n", 39 << 30, 11 << 30, true, true},
		{"columns swapped by header", "PID CMPRS MEM\n21401 10G 40G\n", 39 << 30, 11 << 30, true, true},
		{"other pid only", hdr + "99  40G  10G\n", 0, 0, false, false},
		{"mem garbage", hdr + "21401  lots  10G\n", 0, 0, false, false},
		{"empty", "", 0, 0, false, false},
		{"garbage", "top: command not found\n", 0, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := parseTopRow(tc.in, 21401)
			if ok != tc.ok || (ok && (row.mem != tc.mem || row.cmprs != tc.cmprs || row.cmprsOK != tc.cmprsOK)) {
				t.Fatalf("parseTopRow = %+v, %v; want mem %d cmprs %d ok %v / %v", row, ok, tc.mem, tc.cmprs, tc.cmprsOK, tc.ok)
			}
		})
	}
}

// A top without a readable cmprs column gives no credit at all, and says so
// once per change of reason, not per tick.
func TestVMResidentNoCmprsGivesNoCredit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(home, ".colima", "default", "docker.sock"))
	pidFile := filepath.Join(home, ".colima", "_lima", "colima", "ha.pid")
	var logs []string
	fr := &fakeRunner{out: map[string]string{
		"info --format":                 "24 41971261440\n",
		"stats --no-stream --f":         fixtureDockerStats,
		"top -l 1 -pid 21401 -stats pi": "PID    MEM\n21401  40G\n",
	}}
	p := newProberWith(Options{Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}, fr.run)
	now := time.Date(2026, 11, 11, 8, 30, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	d := p.docker
	d.vm = true
	d.lookup = func() string { return "/opt/homebrew/bin/docker" }
	d.readFile = func(path string) ([]byte, error) {
		if path == pidFile {
			return []byte("21401\n"), nil
		}
		return nil, os.ErrNotExist
	}
	for i := 0; i < 4; i++ {
		d.refresh(context.Background())
		rm, _ := d.runtimeMetrics(proto.NodeMetrics{Time: now})
		if rm.HostResident != 0 {
			t.Fatalf("tick %d: host_resident %d, want none", i, rm.HostResident)
		}
		now = now.Add(vmResidentEvery)
	}
	if fr.count("top -l 1") != 4 {
		t.Fatalf("top ran %d times", fr.count("top -l 1"))
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "cmprs") || !strings.Contains(logs[0], "no credit") {
		t.Fatalf("logs %q: want one line naming cmprs", logs)
	}
	// top shows it again: credit returns; losing it again logs anew.
	fr.mu.Lock()
	fr.out["top -l 1 -pid 21401 -stats pi"] = fixtureTopPid
	fr.mu.Unlock()
	d.refresh(context.Background())
	if rm, _ := d.runtimeMetrics(proto.NodeMetrics{Time: now}); rm.HostResident != 28<<30 {
		t.Fatalf("host_resident %d", rm.HostResident)
	}
}

func TestColimaLimaDir(t *testing.T) {
	for in, want := range map[string]string{
		"unix:///Users/dev/.colima/default/docker.sock": "/Users/dev/.colima/_lima/colima",
		"unix:///Users/dev/.colima/docker.sock":         "/Users/dev/.colima/_lima/colima",
		"unix:///Users/dev/.colima/work/docker.sock":    "/Users/dev/.colima/_lima/colima-work",
		"unix:///var/run/docker.sock":                   "",
		"unix:///Users/dev/.docker/run/docker.sock":     "",
		"tcp://127.0.0.1:2375":                          "",
		"/Users/dev/.colima/default/docker.sock":        "",
	} {
		if got := colimaLimaDir(in); got != want {
			t.Errorf("colimaLimaDir(%q) = %q, want %q", in, got, want)
		}
	}
}

const fixtureTopPid = `Processes: 612 total, 3 running, 609 sleeping, 3021 threads
2026/11/11 08:30:00
Load Avg: 6.61, 6.20, 6.02
PhysMem: 93G used (17G wired, 2938M compressor), 3402M unused.

PID    MEM    CMPRS
21401  40G    10G
`

func TestVMResidentFromColimaHostAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(home, ".colima", "default", "docker.sock"))
	pidFile := filepath.Join(home, ".colima", "_lima", "colima", "ha.pid")
	var logs []string
	fr := &fakeRunner{out: map[string]string{
		"info --format":                 "24 41971261440\n",
		"stats --no-stream --f":         fixtureDockerStats,
		"top -l 1 -pid 21401 -stats pi": fixtureTopPid,
	}}
	p := newProberWith(Options{Logf: func(f string, a ...any) { logs = append(logs, f) }}, fr.run)
	now := time.Date(2026, 11, 11, 8, 30, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	d := p.docker
	d.vm = true
	d.lookup = func() string { return "/opt/homebrew/bin/docker" }
	d.readFile = func(path string) ([]byte, error) {
		if path == pidFile {
			return []byte("21401\n"), nil
		}
		return nil, os.ErrNotExist
	}
	resident := func() int64 {
		rm, ok := d.runtimeMetrics(proto.NodeMetrics{Time: now})
		if !ok {
			t.Fatal("no runtime metrics")
		}
		return rm.HostResident
	}

	d.refresh(context.Background())
	// top's 40G rounds down to 39 GiB, its 10G compressed up to 11 GiB.
	if got := resident(); got != 28<<30 {
		t.Fatalf("host_resident %d, want 28 GiB (40G less 10G compressed, rounded against credit)", got)
	}
	// Not measured again within the interval.
	now = now.Add(10 * time.Second)
	d.refresh(context.Background())
	if n := fr.count("top -l 1"); n != 1 {
		t.Fatalf("top ran %d times within %s", n, vmResidentEvery)
	}
	// Overloaded: nothing forked, the last reading stands in until stale.
	p.stressed.Store(true)
	now = now.Add(time.Minute)
	d.refresh(context.Background())
	if n := fr.count("top -l 1"); n != 1 || resident() != 28<<30 {
		t.Fatalf("stressed: top ran %d times, resident %d", n, resident())
	}
	now = now.Add(2 * time.Minute)
	if got := resident(); got != 0 {
		t.Fatalf("a stale reading must not count: %d", got)
	}
	p.stressed.Store(false)
	// A failed reading publishes none, never the last one.
	d.refresh(context.Background())
	if resident() != 28<<30 {
		t.Fatal("fresh reading expected")
	}
	fr.mu.Lock()
	fr.err = map[string]error{"top -l 1": errors.New("top: exit status 1")}
	fr.mu.Unlock()
	now = now.Add(vmResidentEvery)
	d.refresh(context.Background())
	if got := resident(); got != 0 {
		t.Fatalf("failed reading: host_resident %d, want 0", got)
	}
	if len(logs) != 1 {
		t.Fatalf("logs %v", logs)
	}
}

func TestVMResidentOnlyForColima(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	fr := &fakeRunner{out: map[string]string{
		"info --format":         "8 8589934592\n",
		"stats --no-stream --f": fixtureDockerStats,
	}}
	p := newProberWith(Options{}, fr.run)
	now := time.Date(2026, 11, 11, 8, 30, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	d := p.docker
	d.vm = true
	d.lookup = func() string { return "/usr/local/bin/docker" }
	d.refresh(context.Background())
	rm, ok := d.runtimeMetrics(proto.NodeMetrics{Time: now})
	if !ok || rm.HostResident != 0 || fr.count("top") != 0 {
		t.Fatalf("rm %+v, top calls %d", rm, fr.count("top"))
	}
	// Linux: no VM, never measured.
	p2 := newProberWith(Options{}, fr.run)
	p2.docker.vm = false
	p2.docker.lookup = d.lookup
	p2.docker.refresh(context.Background())
	if rm, _ := p2.docker.runtimeMetrics(proto.NodeMetrics{Time: now, MemAvailable: 1 << 30}); rm.HostResident != 0 {
		t.Fatalf("linux rm %+v", rm)
	}
}
