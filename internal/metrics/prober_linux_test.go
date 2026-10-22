package metrics

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// fixtureProber points a prober at a fake /proc, /sys and /etc shaped like
// the GX10: 2 Cortex-X925 + 2 Cortex-A725 (cpu_capacity set).
func fixtureProber(t *testing.T) (*prober, string) {
	root := t.TempDir()
	files := map[string]string{
		"proc/stat":                     fixtureProcStat,
		"proc/meminfo":                  fixtureMeminfo,
		"proc/loadavg":                  "24.20 23.72 30.65 7/8430 2716699\n",
		"proc/uptime":                   "1000.00 5000.00\n",
		"proc/pressure/cpu":             fixturePSI,
		"proc/pressure/memory":          "some avg10=0.30 avg60=1.97 avg300=1.39 total=1\nfull avg10=0.18 avg60=1 avg300=1 total=1\n",
		"proc/pressure/io":              "some avg10=7.28 avg60=15.01 avg300=14.86 total=1\n",
		"proc/net/dev":                  fixtureNetDev,
		"proc/cpuinfo":                  fixtureCPUInfoARM,
		"sys/devices/system/cpu/online": "0-3\n",
		"sys/devices/system/cpu/cpu0/cpu_capacity": "1024\n",
		"sys/devices/system/cpu/cpu1/cpu_capacity": "718\n",
		"sys/devices/system/cpu/cpu2/cpu_capacity": "1017\n",
		"sys/devices/system/cpu/cpu3/cpu_capacity": "731\n",
		"etc/os-release": "PRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\n",
		// Process group 500: two members; 600: another group.
		"proc/500/stat": "500 (go) S 1 500 500 0 -1 0 0 0 0 0 300 100 0 0 20 0 1 0 50000 0 1000 0",
		"proc/501/stat": "501 (go test) R 500 500 500 0 -1 0 0 0 0 0 100 100 0 0 20 0 1 0 60000 0 3000 0",
		"proc/600/stat": "600 (bash) S 1 600 600 0 -1 0 0 0 0 0 999 999 0 0 20 0 1 0 1000 0 9999 0",
	}
	writeTree(t, root, files)
	p := newProberWith(Options{StateDir: root}, (&fakeRunner{}).run)
	p.procDir, p.sysDir, p.etcDir = filepath.Join(root, "proc"), filepath.Join(root, "sys"), filepath.Join(root, "etc")
	return p, root
}

func TestLinuxStaticInfoFixture(t *testing.T) {
	p, _ := fixtureProber(t)
	var info proto.NodeInfo
	p.plat.staticInfo(context.Background(), &info)
	if info.CPUs != 4 || info.PerfCPUs != 2 || info.MemTotal != 127535148*1024 {
		t.Fatalf("info = %+v", info)
	}
	if info.CPUModel != "Cortex-X925 ×2 + Cortex-A725 ×2" {
		t.Fatalf("model = %q", info.CPUModel)
	}
	if want := "Ubuntu 24.04.4 LTS (kernel "; len(info.OSVersion) < len(want) || info.OSVersion[:len(want)] != want {
		t.Fatalf("os = %q", info.OSVersion)
	}
	if got := p.plat.(*linuxPlatform).perfCPUs; fmt.Sprint(got) != "[0 2]" {
		t.Fatalf("perf cpus = %v", got)
	}
}

func TestLinuxSampleFixture(t *testing.T) {
	p, root := fixtureProber(t)
	t0 := time.Unix(2000, 0)
	var m proto.NodeMetrics
	p.plat.sample(context.Background(), &m, t0)
	if m.CPUBusy != 0 || m.NetRxBps != 0 {
		t.Fatal("first sample has no rates")
	}
	if m.Load1 != 24.20 || m.MemAvailable != 56540060*1024 || m.MemUsed != (127535148-56540060)*1024 {
		t.Fatalf("m = %+v", m)
	}
	if m.SwapTotal != 16777212*1024 || m.SwapUsed != (16777212-1048576)*1024 {
		t.Fatalf("swap = %d/%d", m.SwapUsed, m.SwapTotal)
	}
	if m.CPUPressure != 6.08 || m.MemPressure != 0.30 || m.IOPressure != 7.28 {
		t.Fatalf("psi = %v %v %v", m.CPUPressure, m.MemPressure, m.IOPressure)
	}
	// Advance counters: 1000 jiffies, 400 idle → 60% busy; 2 s of net.
	writeTree(t, root, map[string]string{
		"proc/stat":    "cpu  276675285 129168251 164802311 1027019337 64083586 0 3025173 0 0 0\n",
		"proc/net/dev": "wlP9s9: 210663341841 1 0 0 0 0 0 0 445014881961 1 0 0 0 0 0 0\n",
	})
	var m2 proto.NodeMetrics
	p.plat.sample(context.Background(), &m2, t0.Add(2*time.Second))
	if !near(m2.CPUBusy, 0.6) || m2.NetRxBps != 1000 || m2.NetTxBps != 2000 {
		t.Fatalf("rates = busy %v rx %v tx %v", m2.CPUBusy, m2.NetRxBps, m2.NetTxBps)
	}
}

func TestLinuxPGIDUsageFixture(t *testing.T) {
	p, _ := fixtureProber(t)
	now := time.Unix(5000, 0)
	r, err := p.plat.pgidUsage(context.Background(), 500, now)
	if err != nil {
		t.Fatal(err)
	}
	page := int64(os.Getpagesize())
	if r.Mem != 4000*page {
		t.Fatalf("mem = %d", r.Mem)
	}
	// 600 ticks = 6 cpu-seconds over the group's 500 s life (uptime 1000,
	// oldest start tick 50000 = 500 s).
	if !near(r.CPUs, 6.0/500) {
		t.Fatalf("cpus = %v", r.CPUs)
	}
	if _, err := p.plat.pgidUsage(context.Background(), 4242, now); err == nil {
		t.Fatal("unknown group: want error")
	}
}

// TestOwnCgroupUsage measures the test process's real cgroup v2 group.
func TestOwnCgroupUsage(t *testing.T) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip("no /proc/self/cgroup")
	}
	path, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "0::")
	if !ok || strings.Contains(path, "\n") {
		t.Skip("not a pure cgroup v2 host")
	}
	rel := strings.TrimPrefix(path, "/")
	if _, err := os.Stat(filepath.Join(CgroupRoot, rel, "memory.current")); err != nil {
		t.Skip("memory controller not delegated here")
	}
	p := newProberWith(Options{}, execRunner)
	r, err := p.JobUsage(UsageRef{CgroupPath: rel})
	if err != nil {
		t.Fatal(err)
	}
	u, err := CgroupStats(rel)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("own cgroup %s: %.2f cores, current %d MiB, peak %d MiB, %.1f cpu-s", rel, r.CPUs, u.MemCurrent>>20, u.MemPeak>>20, u.CPUSeconds)
	if r.Mem <= 0 || u.CPUSeconds <= 0 {
		t.Fatalf("implausible: %+v %+v", r, u)
	}
}
