package metrics

import (
	"math"
	"reflect"
	"testing"
)

// Fixtures are verbatim output recorded on gx10 (NVIDIA GB10, Ubuntu 24.04)
// and devmac (Mac Studio M3 Ultra, macOS 26).

const fixtureProcStat = `cpu  276674685 129168251 164802311 1027018937 64083586 0 3025173 0 0 0
cpu0 11726714 4872202 8195355 53790264 3383977 0 184325 0 0 0
intr 1 2 3
`

const fixtureMeminfo = `MemTotal:       127535148 kB
MemFree:        33469288 kB
MemAvailable:   56540060 kB
Buffers:          936332 kB
Cached:         23266492 kB
SwapTotal:      16777212 kB
SwapFree:        1048576 kB
HugePages_Total:       0
`

const fixturePSI = `some avg10=6.08 avg60=6.98 avg300=8.31 total=54171487160
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
`

const fixtureNetDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 4824256410509 1320220242    0    0    0     0          0         0 4824256410509 1320220242    0    0    0     0       0          0
enP7s7:       0       0    0    0    0     0          0         0        0       0    0   11    0     0       0          0
wlP9s9: 210663339841 270297112    0    0    0     0          0         0 445014877961 365916559    0    0    0     0       0          0
docker0: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0
vethab12: 1000 10 0 0 0 0 0 0 2000 20 0 0 0 0 0 0
`

const fixtureNetstatIB = `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                      49419928     0 158682758400 49419928     0 158682758400     0
lo0        16384 127           localhost       49419928     - 158682758400 49419928     - 158682758400     -
gif0*      1280  <Link#2>                             0     0          0        0     0          0     0
en0        1500  <Link#11>   36:a8:5d:12:34:56 812345678     0 912345678901 523456789     0 123456789012     0
en0        1500  fe80::1     fe80:b::1        812345678     - 912345678901 523456789     - 123456789012     -
en1        1500  <Link#12>   36:a8:5d:12:34:57      100     0       5000      200     0       7000     0
utun3      1380  <Link#20>                          10     0       999         10     0       999     0
`

const fixtureVMStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                             2162167.
Pages active:                           1353180.
Pages inactive:                          882239.
Pages speculative:                       470681.
Pages throttled:                              0.
Pages wired down:                       1146051.
Pages purgeable:                          20352.
"Translation faults":                3067997321.
Pages copy-on-write:                  647817199.
`

const fixtureNvidiaGB10 = "NVIDIA GB10, [N/A], [N/A], 6\n"

const fixtureNvidiaDiscrete = `NVIDIA GeForce RTX 4090, 24564, 1203, 37
NVIDIA GeForce RTX 4090, 24564, 0, 0
`

const fixtureDockerStats = "acme-qdrant\t0.20%\t71.45MiB / 4GiB\n" +
	"acme-postgres\t31.24%\t1.825GiB / 8GiB\n" +
	"kran-job-abc\t250.00%\t512KiB / 16GiB\n" +
	"stopped\t--\t0B / 0B\n"

const fixtureCPUInfoARM = `processor	: 0
BogoMIPS	: 2000.00
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x0
CPU part	: 0xd85
CPU revision	: 1

processor	: 1
CPU implementer	: 0x41
CPU part	: 0xd87

processor	: 2
CPU implementer	: 0x41
CPU part	: 0xd85

processor	: 3
CPU implementer	: 0x41
CPU part	: 0xd87
`

const fixtureCPUInfoX86 = `processor	: 0
vendor_id	: AuthenticAMD
model name	: AMD Ryzen 9  7950X 16-Core Processor
`

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestParseProcStat(t *testing.T) {
	ct, err := parseProcStat(fixtureProcStat)
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := uint64(276674685 + 129168251 + 164802311 + 1027018937 + 64083586 + 0 + 3025173 + 0)
	if ct.total != wantTotal || ct.idle != 1027018937+64083586 {
		t.Fatalf("got %+v, want total %d", ct, wantTotal)
	}
	next := cpuTimes{total: ct.total + 1000, idle: ct.idle + 250}
	busy, ok := busyBetween(ct, next)
	if !ok || !near(busy, 0.75) {
		t.Fatalf("busy = %v %v, want 0.75", busy, ok)
	}
	if _, ok := busyBetween(next, next); ok {
		t.Fatal("no elapsed time must not produce a reading")
	}
	if _, err := parseProcStat("intr 1 2\n"); err == nil {
		t.Fatal("want error without cpu line")
	}
}

func TestParseMeminfo(t *testing.T) {
	m := parseMeminfo(fixtureMeminfo)
	if m["MemTotal"] != 127535148*1024 || m["MemAvailable"] != 56540060*1024 || m["SwapFree"] != 1048576*1024 {
		t.Fatalf("unexpected %v", m)
	}
	if m["HugePages_Total"] != 0 {
		t.Fatal("unitless field must parse as plain number")
	}
}

func TestParsePSIAndLoad(t *testing.T) {
	v, err := parsePSI(fixturePSI)
	if err != nil || !near(v, 6.08) {
		t.Fatalf("psi = %v, %v", v, err)
	}
	l, err := parseLoadavg("24.20 23.72 30.65 7/8430 2716699\n")
	if err != nil || !near(l, 24.20) {
		t.Fatalf("load = %v, %v", l, err)
	}
}

func TestParseNet(t *testing.T) {
	c := parseNetDev(fixtureNetDev)
	if c.rx != 210663339841 || c.tx != 445014877961 {
		t.Fatalf("netdev = %+v (lo/docker/veth must be skipped)", c)
	}
	m := parseNetstatIB(fixtureNetstatIB)
	if m.rx != 912345678901+5000 || m.tx != 123456789012+7000 {
		t.Fatalf("netstat = %+v (only <Link#> rows of up, physical interfaces)", m)
	}
	if counterRate(100, 50, 1) != 0 || counterRate(100, 300, 2) != 100 {
		t.Fatal("counterRate")
	}
}

func TestParseCPUList(t *testing.T) {
	got, err := parseCPUList("0-3,8,10-11\n")
	if err != nil || !reflect.DeepEqual(got, []int{0, 1, 2, 3, 8, 10, 11}) {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range []string{"", "a-b", "3-1"} {
		if _, err := parseCPUList(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestCPUModel(t *testing.T) {
	arm := parseCPUInfo(fixtureCPUInfoARM)
	if len(arm) != 4 || arm[1].part != 0xd87 || arm[0].implementer != 0x41 {
		t.Fatalf("cpuinfo = %+v", arm)
	}
	caps := map[int]int64{0: 1024, 1: 718, 2: 1017, 3: 731}
	if got := cpuModel(arm, caps); got != "Cortex-X925 ×2 + Cortex-A725 ×2" {
		t.Fatalf("model = %q", got)
	}
	if got := cpuModel(parseCPUInfo(fixtureCPUInfoX86), nil); got != "AMD Ryzen 9 7950X 16-Core Processor" {
		t.Fatalf("model = %q", got)
	}
	if got := armCoreName(0x41, 0xd4f); got != "Neoverse-V2" {
		t.Fatalf("got %q", got)
	}
	if got := perfSet(caps); !reflect.DeepEqual(got, []int{0, 2}) {
		t.Fatalf("perf = %v", got)
	}
	// GX10 cpufreq max (no cpu_capacity): 3.9 GHz X925 vs 2.8 GHz A725.
	if got := perfSet(map[int]int64{0: 3900000, 1: 2808000, 2: 3900000}); len(got) != 2 {
		t.Fatalf("perf by freq = %v", got)
	}
	if got := perfSet(map[int]int64{0: 1024, 1: 1024}); len(got) != 2 {
		t.Fatalf("homogeneous perf = %v", got)
	}
}

func TestParseOSRelease(t *testing.T) {
	s := "NAME=\"Ubuntu\"\nVERSION=\"24.04.4 LTS (Noble Numbat)\"\nPRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\n"
	if got := parseOSRelease(s); got != "Ubuntu 24.04.4 LTS" {
		t.Fatalf("got %q", got)
	}
	if got := parseOSRelease("NAME=Alpine\nVERSION=3.20\n"); got != "Alpine 3.20" {
		t.Fatalf("got %q", got)
	}
}

func TestParseNvidia(t *testing.T) {
	g, err := parseNvidiaCSV(fixtureNvidiaGB10)
	if err != nil {
		t.Fatal(err)
	}
	want := []gpuReading{{name: "NVIDIA GB10", util: 0.06, unified: true}}
	if len(g) != 1 || g[0].name != want[0].name || !g[0].unified || g[0].memTot != 0 || !near(g[0].util, 0.06) {
		t.Fatalf("GB10 = %+v", g)
	}
	g, err = parseNvidiaCSV(fixtureNvidiaDiscrete)
	if err != nil || len(g) != 2 || g[0].unified || g[0].memTot != 24564<<20 || g[0].memUsed != 1203<<20 {
		t.Fatalf("discrete = %+v %v", g, err)
	}
	if _, err := parseNvidiaCSV("\n"); err == nil {
		t.Fatal("empty output must be an error")
	}
	if _, err := parseNvidiaCSV("NVIDIA GB10, 1\n"); err == nil {
		t.Fatal("short line must be an error")
	}
}

func TestParseVMStat(t *testing.T) {
	ps, pages, err := parseVMStat(fixtureVMStat)
	if err != nil || ps != 16384 || pages["Pages wired down"] != 1146051 || pages["Translation faults"] != 3067997321 {
		t.Fatalf("ps=%d pages=%v err=%v", ps, pages, err)
	}
	want := int64(2162167+882239+470681+20352) * 16384
	if got := vmStatAvailable(ps, pages); got != want {
		t.Fatalf("available = %d, want %d", got, want)
	}
	if _, _, err := parseVMStat("Pages free: 1.\n"); err == nil {
		t.Fatal("missing header must be an error")
	}
}

func TestParseTopCPU(t *testing.T) {
	v, ok := parseTopCPU("CPU usage: 22.98% user, 14.90% sys, 62.11% idle ")
	if !ok || !near(v, 1-0.6211) {
		t.Fatalf("got %v %v", v, ok)
	}
	if _, ok := parseTopCPU("Load Avg: 1.0, 2.0, 3.0"); ok {
		t.Fatal("non-CPU line must not parse")
	}
}

func TestParseClockAndPS(t *testing.T) {
	cases := map[string]float64{
		"0:00.01":     0.01,
		"123:45.67":   123*60 + 45.67,
		"01:02:03":    3723,
		"2-01:00:00":  2*86400 + 3600,
		"05:07":       307,
		"1:02:03.500": 3723.5,
	}
	for in, want := range cases {
		got, err := parseClock(in)
		if err != nil || !near(got, want) {
			t.Errorf("parseClock(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x:01", "1:2:3:4"} {
		if _, err := parseClock(bad); err == nil {
			t.Errorf("parseClock(%q): want error", bad)
		}
	}
	ps := "  501  2512   0:01.50     01:40\n  501  1568   0:00.50     00:10\n  777  9999  10:00.00  1-00:00:00\n garbage\n"
	rss, cpu, age, n := psGroupUsage(ps, 501)
	if n != 2 || rss != (2512+1568)*1024 || !near(cpu, 2) || !near(age, 100) {
		t.Fatalf("rss=%d cpu=%v age=%v n=%d", rss, cpu, age, n)
	}
}

func TestParseSizeAndDocker(t *testing.T) {
	cases := map[string]int64{
		"71.45MiB": bytesOf(71.45 * (1 << 20)),
		"4GiB":     4 << 30,
		"0B":       0,
		"512kB":    512000,
		"1.2MB":    1200000,
		"512KiB":   512 << 10,
		"42":       42,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "GiB", "3XB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q): want error", bad)
		}
	}
	st := parseDockerStats(fixtureDockerStats)
	if len(st) != 4 {
		t.Fatalf("stats = %+v", st)
	}
	if !near(st[1].cpus, 0.3124) || st[1].mem != bytesOf(1.825*(1<<30)) || !near(st[2].cpus, 2.5) || st[3].cpus != 0 {
		t.Fatalf("stats = %+v", st)
	}
	n, mem, err := parseDockerInfo("16 33584394240\n")
	if err != nil || n != 16 || mem != 33584394240 {
		t.Fatalf("info = %d %d %v", n, mem, err)
	}
	if _, _, err := parseDockerInfo("Cannot connect to the Docker daemon"); err == nil {
		t.Fatal("want error")
	}
}

func TestParseProcPIDStat(t *testing.T) {
	// comm with spaces and a ')' must not shift fields.
	s := "4242 (my (weird) proc) S 1 4242 4242 0 -1 4194560 100 0 0 0 150 50 0 0 20 0 3 0 12345 1000000 2560 18446744073709551615"
	st, err := parseProcPIDStat(s)
	if err != nil {
		t.Fatal(err)
	}
	if st.pgrp != 4242 || st.ticks != 200 || st.startTick != 12345 || st.rssPages != 2560 {
		t.Fatalf("got %+v", st)
	}
	if _, err := parseProcPIDStat("1 (x) S 1"); err == nil {
		t.Fatal("short stat must be an error")
	}
}

func TestCgroupCPUUsec(t *testing.T) {
	v, err := cgroupCPUUsec("usage_usec 2500000\nuser_usec 2000000\nsystem_usec 500000\n")
	if err != nil || v != 2500000 {
		t.Fatalf("got %d %v", v, err)
	}
	if _, err := cgroupCPUUsec("user_usec 1\n"); err == nil {
		t.Fatal("want error")
	}
}

// bytesOf truncates like the parsers do (constant conversion would not compile).
func bytesOf(f float64) int64 { return int64(f) }
