package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestPipeLimitsPerNode: kran doctor reports fs.pipe-user-pages-soft of
// every kernel kran's jobs run under (a Linux host's, a Mac's Docker VM's),
// warns at the kernel default with where to raise it, and never offers to
// change it itself (no --fix).
func TestPipeLimitsPerNode(t *testing.T) {
	o := baseOpts(t)
	o.PipePages = func() int64 { return 16384 }
	linuxRT := func(v int64) []proto.Runtime {
		return []proto.Runtime{{Kind: proto.RuntimeNative, OS: "linux", Available: true, PipeUserPagesSoft: v},
			{Kind: proto.RuntimeContainer, OS: "linux", Available: true, PipeUserPagesSoft: v}}
	}
	c := &checker{o: o, daemon: &proto.VersionInfo{}, state: &proto.State{Nodes: []proto.NodeState{
		{Self: true, Info: proto.NodeInfo{Name: "gx10", OS: "linux", Runtimes: linuxRT(262144)}},
		{Info: proto.NodeInfo{Name: "devmac", OS: "darwin", Runtimes: []proto.Runtime{
			{Kind: proto.RuntimeNative, OS: "darwin", Available: true},
			{Kind: proto.RuntimeContainer, OS: "linux", Available: true, PipeUserPagesSoft: 16384}}}},
		{Info: proto.NodeInfo{Name: "box", OS: "linux", Runtimes: linuxRT(65536)}},
		{Info: proto.NodeInfo{Name: "old", OS: "linux", Runtimes: linuxRT(0)}},
		{Info: proto.NodeInfo{Name: "macnodocker", OS: "darwin", Runtimes: []proto.Runtime{{Kind: proto.RuntimeNative, OS: "darwin", Available: true}}}},
	}}}
	c.checkPipeLimits(context.Background())
	by := map[string]Result{}
	for _, r := range c.results {
		if r.ID == "pipes" {
			by[r.Node] = r
		}
	}
	if r := by["gx10"]; r.Level != OK || r.Detail != "fs.pipe-user-pages-soft = 262144 (the host)" {
		t.Errorf("gx10: %+v", r)
	}
	if r := by["devmac"]; r.Level != Warn || !strings.Contains(r.Detail, "= 16384 (the Docker VM), the kernel default") ||
		!strings.Contains(r.Fix, "inside devmac's Docker VM") || !strings.Contains(r.Fix, "fs.pipe-user-pages-soft=262144") {
		t.Errorf("devmac: %+v", r)
	}
	if r := by["box"]; r.Level != Info || !strings.Contains(r.Fix, "sudo tee /etc/sysctl.d/") {
		t.Errorf("box: %+v", r)
	}
	if r := by["old"]; r.Level != Info || !strings.Contains(r.Detail, "unknown") {
		t.Errorf("old: %+v", r)
	}
	if _, ok := by["macnodocker"]; ok {
		t.Errorf("a Mac without a container runtime runs no Linux job: %+v", by["macnodocker"])
	}
	for _, r := range c.results {
		if r.fix != nil {
			t.Errorf("%s offers to change a kernel setting itself", r.Title)
		}
	}

	// Without a daemon: this machine's own value.
	c = &checker{o: o}
	c.checkPipeLimits(context.Background())
	if r := find(c.results, "pipes"); r == nil || r.Level != Warn || r.Title != "this machine pipe limit" || !strings.Contains(r.Fix, "kran never changes") {
		t.Errorf("no daemon: %+v", c.results)
	}
}
