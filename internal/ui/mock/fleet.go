package mock

import (
	"fmt"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

const gib = int64(1) << 30

// simNode carries what the simulation needs beyond the published NodeState.
type simNode struct {
	st proto.NodeState
	// outside is CPU cores busy with non-kran work (the owner's IDE,
	// browsers, a local build); it random-walks between lo and hi.
	outside, outsideLo, outsideHi float64
	// baseMem is memory in use by non-kran processes.
	baseMem int64
	// flaky nodes drop off the mesh now and then.
	flaky        bool
	offlineUntil time.Time
	nextFlap     time.Time
	// vm bounds what Linux jobs may use on a Mac (the Docker VM).
	vm proto.Resources
}

func (n *simNode) name() string { return n.st.Info.Name }

func (n *simNode) linuxJobsVia() proto.RuntimeKind {
	if n.st.Info.OS == "linux" {
		return proto.RuntimeNative
	}
	return proto.RuntimeContainer
}

func runtimeNative(os, arch string, cpus int, mem int64, tc map[string]string) proto.Runtime {
	return proto.Runtime{Kind: proto.RuntimeNative, OS: os, Arch: arch, CPUs: cpus, Mem: mem, Available: true, Toolchains: tc, Docker: true}
}

func fleet(now time.Time, extra int) []*simNode {
	started := now.Add(-26 * time.Hour).UTC()
	gx10 := &simNode{
		st: proto.NodeState{
			Info: proto.NodeInfo{
				ID: "4c1f0a9e2b7d4e18a0c3", Name: "gx10", Hostname: "gx10", OS: "linux", Arch: "arm64",
				OSVersion: "Ubuntu 24.04.3 LTS", Version: "0.4.0", CPUModel: "Cortex-X925 + Cortex-A725",
				CPUs: 20, PerfCPUs: 10, MemTotal: 128 * gib, DiskTotal: 3725 * gib,
				GPUs:            []proto.GPU{{Vendor: "nvidia", Model: "NVIDIA GB10", Unified: true}},
				Runtimes:        []proto.Runtime{runtimeNative("linux", "arm64", 20, 128*gib, map[string]string{"go": "1.26.0", "node": "24.8.0", "python": "3.12.3", "mise": "2026.10.1"}), {Kind: proto.RuntimeContainer, OS: "linux", Arch: "arm64", CPUs: 20, Mem: 128 * gib, Available: true, Docker: true}},
				SingleCoreScore: 1.0, StartedAt: started,
			},
			Status: proto.NodeOnline, Self: true, Accepts: true,
			Capacity: proto.Resources{CPUs: 18, Mem: 120 * gib},
			Reserve:  proto.Resources{CPUs: 2, Mem: 8 * gib},
		},
		outside: 1.6, outsideLo: 0.4, outsideHi: 5, baseMem: 22 * gib,
		vm: proto.Resources{CPUs: 20, Mem: 128 * gib},
	}
	devmac := &simNode{
		st: proto.NodeState{
			Info: proto.NodeInfo{
				ID: "9b02e6d1c4a84f7f81d2", Name: "devmac", Hostname: "devmac.local", OS: "darwin", Arch: "arm64",
				OSVersion: "macOS 26.0.1", Version: "0.4.0", CPUModel: "Apple M3 Ultra",
				CPUs: 28, PerfCPUs: 20, MemTotal: 96 * gib, DiskTotal: 1858 * gib,
				GPUs: []proto.GPU{{Vendor: "apple", Model: "Apple M3 Ultra (60-core GPU)", Unified: true}},
				Runtimes: []proto.Runtime{
					runtimeNative("darwin", "arm64", 28, 96*gib, map[string]string{"go": "1.26.0", "node": "24.8.0", "mise": "2026.10.1"}),
					{Kind: proto.RuntimeContainer, OS: "linux", Arch: "arm64", CPUs: 16, Mem: 32 * gib, Available: true, Docker: true, Toolchains: map[string]string{"mise": "2026.10.1"}},
				},
				SingleCoreScore: 1.31, StartedAt: started.Add(3 * time.Hour),
			},
			Status: proto.NodeOnline, Accepts: true,
			Capacity: proto.Resources{CPUs: 26, Mem: 88 * gib},
			Reserve:  proto.Resources{CPUs: 2, Mem: 8 * gib},
			Link:     proto.LinkInfo{Address: "192.168.1.20:22", Direction: "both", RTTMillis: 1.7, BandwidthBps: 108e6, Connected: true},
		},
		outside: 3, outsideLo: 1, outsideHi: 9, baseMem: 41 * gib,
		vm: proto.Resources{CPUs: 16, Mem: 32 * gib},
	}
	e2e := &simNode{
		st: proto.NodeState{
			Info: proto.NodeInfo{
				ID: "1d7e3b90aa5c4c02b6f4", Name: "e2e", Hostname: "e2e-mini.local", OS: "darwin", Arch: "arm64",
				OSVersion: "macOS 26.0", Version: "0.4.0", CPUModel: "Apple M4 Pro",
				CPUs: 14, PerfCPUs: 10, MemTotal: 48 * gib, DiskTotal: 926 * gib,
				GPUs: []proto.GPU{{Vendor: "apple", Model: "Apple M4 Pro (20-core GPU)", Unified: true}},
				Runtimes: []proto.Runtime{
					runtimeNative("darwin", "arm64", 14, 48*gib, map[string]string{"node": "24.8.0"}),
					{Kind: proto.RuntimeContainer, OS: "linux", Arch: "arm64", CPUs: 8, Mem: 16 * gib, Available: true, Docker: true},
				},
				SingleCoreScore: 1.38, StartedAt: started.Add(20 * time.Hour),
			},
			Status: proto.NodeOnline, Accepts: true,
			Capacity: proto.Resources{CPUs: 12, Mem: 40 * gib},
			Reserve:  proto.Resources{CPUs: 2, Mem: 8 * gib},
			Link:     proto.LinkInfo{Address: "100.97.12.4:22", Direction: "outbound", RTTMillis: 23.5, BandwidthBps: 38e6, Connected: true},
		},
		outside: 0.6, outsideLo: 0.2, outsideHi: 3, baseMem: 14 * gib, flaky: true,
		nextFlap: now.Add(45 * time.Second),
		vm:       proto.Resources{CPUs: 8, Mem: 16 * gib},
	}
	nodes := []*simNode{gx10, devmac, e2e}
	for i := 0; i < extra; i++ {
		name := fmt.Sprintf("build-%02d", i+4)
		cpus := 32
		if i%3 == 1 {
			cpus = 64
		}
		mem := int64(cpus) * 4 * gib
		nodes = append(nodes, &simNode{
			st: proto.NodeState{
				Info: proto.NodeInfo{
					ID: fmt.Sprintf("%020x", i+100), Name: name, Hostname: name, OS: "linux", Arch: "amd64",
					OSVersion: "Debian 13", Version: "0.4.0", CPUModel: "AMD EPYC 9354P", CPUs: cpus, PerfCPUs: cpus,
					MemTotal: mem, DiskTotal: 1800 * gib,
					Runtimes:        []proto.Runtime{runtimeNative("linux", "amd64", cpus, mem, map[string]string{"go": "1.26.0", "node": "24.8.0"})},
					SingleCoreScore: 0.92, StartedAt: started,
				},
				Status: proto.NodeOnline, Accepts: true,
				Capacity: proto.Resources{CPUs: float64(cpus - 2), Mem: mem - 8*gib},
				Reserve:  proto.Resources{CPUs: 2, Mem: 8 * gib},
				Link:     proto.LinkInfo{Address: fmt.Sprintf("10.0.4.%d:22", 10+i), Direction: "both", RTTMillis: 0.8 + float64(i)*0.3, BandwidthBps: 450e6, Connected: true},
			},
			outside: 0.5, outsideLo: 0.1, outsideHi: 2, baseMem: 6 * gib,
			vm: proto.Resources{CPUs: float64(cpus), Mem: mem},
		})
	}
	for _, n := range nodes {
		n.st.Info.DiskTotal = n.st.Info.DiskTotal / gib * gib
		n.st.Metrics.DiskFree = n.st.Info.DiskTotal * 55 / 100
		n.st.LastSeen = now
	}
	return nodes
}

// template is one kind of job the simulated agents and humans submit.
type template struct {
	argv     []string
	ws       string
	branch   string
	family   string
	cpus     float64
	mem      int64
	dur      float64 // seconds on gx10
	gpu      bool
	setup    bool
	local    bool // must stay on origin (e.g. talks to localhost)
	weight   int
	exitFail float64 // probability of a non-zero exit
	log      logKind
}

type logKind int

const (
	logGoTest logKind = iota
	logVitest
	logNext
	logDocker
	logBench
	logTsc
	logGeneric
)

var templates = []template{
	{argv: []string{"go", "test", "./internal/indexplane/..."}, ws: "acme", branch: "feat/index-shards", family: "family:go-test", cpus: 8, mem: 4 * gib, dur: 48, weight: 9, exitFail: 0.12, log: logGoTest},
	{argv: []string{"go", "test", "-race", "./..."}, ws: "acme", branch: "main", family: "learned", cpus: 12, mem: 7 * gib, dur: 150, weight: 4, exitFail: 0.08, log: logGoTest},
	{argv: []string{"go", "vet", "./..."}, ws: "kran", branch: "agent/c-sched", family: "family:go-vet", cpus: 4, mem: 2 * gib, dur: 18, weight: 4, log: logGeneric},
	{argv: []string{"pnpm", "vitest", "run"}, ws: "acme-web", branch: "fix/upload-retry", family: "learned", cpus: 4, mem: 4 * gib, dur: 64, setup: true, weight: 7, exitFail: 0.1, log: logVitest},
	{argv: []string{"pnpm", "tsc", "--noEmit"}, ws: "acme-web", branch: "fix/upload-retry", family: "family:tsc", cpus: 2, mem: 3 * gib, dur: 26, setup: true, weight: 5, exitFail: 0.15, log: logTsc},
	{argv: []string{"next", "build"}, ws: "acme-web", branch: "main", family: "family:next-build", cpus: 4, mem: 6 * gib, dur: 112, setup: true, weight: 4, log: logNext},
	{argv: []string{"docker", "build", "-t", "acme/api:dev", "."}, ws: "acme", branch: "main", family: "family:docker-build", cpus: 4, mem: 4 * gib, dur: 160, weight: 3, log: logDocker},
	{argv: []string{"python", "bench.py", "--gpu"}, ws: "embed-bench", branch: "exp/fp8", family: "flag", cpus: 6, mem: 24 * gib, dur: 210, gpu: true, weight: 2, log: logBench},
	{argv: []string{"pytest", "-x", "tests/"}, ws: "embed-bench", branch: "exp/fp8", family: "family:pytest", cpus: 2, mem: 2 * gib, dur: 34, weight: 3, exitFail: 0.2, log: logGeneric},
	{argv: []string{"go", "test", "./internal/e2e/..."}, ws: "kran", branch: "agent/a-core", family: "learned", cpus: 6, mem: 3 * gib, dur: 75, local: true, weight: 2, exitFail: 0.25, log: logGoTest},
}

var owners = []struct {
	o      proto.Owner
	weight int
	prio   proto.Priority
}{
	{proto.Owner{Kind: proto.OwnerClaude, ID: "sess-7f3a", Name: "claude · acme-a1"}, 6, proto.PriorityNormal},
	{proto.Owner{Kind: proto.OwnerClaude, ID: "sess-91c2", Name: "claude · acme-b2"}, 4, proto.PriorityNormal},
	{proto.Owner{Kind: proto.OwnerCodex, ID: "codex-48121", Name: "codex"}, 4, proto.PriorityNormal},
	{proto.Owner{Kind: proto.OwnerHuman, ID: "pts/3", Name: "alex@pts/3"}, 3, proto.PriorityHigh},
	{proto.Owner{Kind: proto.OwnerAgent, ID: "cursor-2231", Name: "cursor · kran"}, 1, proto.PriorityLow},
	{proto.Owner{Kind: proto.OwnerCI, ID: "ci", Name: "ci · nightly"}, 1, proto.PriorityLow},
}
