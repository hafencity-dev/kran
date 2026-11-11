package metrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

PID    MEM
21401  40G
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
	if got := resident(); got != 39<<30 {
		t.Fatalf("host_resident %d, want 39 GiB (top's 40G rounded down)", got)
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
	if n := fr.count("top -l 1"); n != 1 || resident() != 39<<30 {
		t.Fatalf("stressed: top ran %d times, resident %d", n, resident())
	}
	now = now.Add(2 * time.Minute)
	if got := resident(); got != 0 {
		t.Fatalf("a stale reading must not count: %d", got)
	}
	p.stressed.Store(false)
	// A failed reading publishes none, never the last one.
	d.refresh(context.Background())
	if resident() != 39<<30 {
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
