package doctor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

// checkPipeLimits reports fs.pipe-user-pages-soft of every Linux kernel
// kran's jobs run under: a Linux node's own, a Mac's Docker VM's. Every job
// of a node runs as one user; once that user's pipes hold the soft limit's
// pages (the kernel default: 16384, 64 MiB), every new pipe gets a single
// page, and a program that writes more than 4 KiB into a pipe nobody reads
// yet blocks until its time limit (a Go test helper hung 45 minutes that
// way). kran never changes a kernel setting itself: it says where to.
func (c *checker) checkPipeLimits(ctx context.Context) {
	c.connect(ctx)
	if c.state == nil {
		// No daemon: this machine only.
		if c.o.Node == "" && c.o.GOOS == "linux" {
			c.add(pipeResult("this machine", "", false, c.o.PipePages()))
		}
		return
	}
	for _, n := range c.state.Nodes {
		if c.o.Node != "" && n.Info.Name != c.o.Node {
			continue
		}
		vm := n.Info.OS != "linux"
		var v int64
		have := false
		for _, rt := range n.Info.Runtimes {
			if rt.OS != "linux" || (vm && rt.Kind != proto.RuntimeContainer) {
				continue
			}
			have = true
			v = max(v, rt.PipeUserPagesSoft) // native and container share a Linux host's kernel
		}
		if n.Self && !vm && v == 0 {
			v, have = c.o.PipePages(), true // read here: the daemon reports no runtimes
		}
		if !have {
			continue // a Mac without a container runtime runs no Linux job
		}
		c.add(pipeResult(n.Info.Name, n.Info.Name, vm, v))
	}
}

func pipeResult(name, node string, vm bool, v int64) Result {
	kernel := "the host"
	if vm {
		kernel = "the Docker VM"
	}
	r := Result{ID: "pipes", Title: name + " pipe limit", Node: node, Level: OK,
		Detail: fmt.Sprintf("fs.pipe-user-pages-soft = %d (%s)", v, kernel)}
	fix := fmt.Sprintf("raise it on %s (kran never changes a host's kernel settings): "+
		"echo 'fs.pipe-user-pages-soft = %d' | sudo tee /etc/sysctl.d/60-pipe-user-pages.conf && sudo sysctl --system",
		name, proto.PipeUserPagesWanted)
	if vm {
		fix = fmt.Sprintf("raise it inside %s's Docker VM at every VM start (kran does not configure the VM): "+
			"with Colima, add to ~/.colima/<profile>/colima.yaml `provision: [{mode: system, script: \"sysctl -w fs.pipe-user-pages-soft=%d\"}]` and run `colima restart`",
			name, proto.PipeUserPagesWanted)
	}
	switch {
	case v <= 0:
		r.Level = Info
		r.Detail = fmt.Sprintf("fs.pipe-user-pages-soft of %s is unknown", kernel)
		if vm {
			r.Detail += " (its kran is older, or no container job has synced there since Docker started)"
		} else {
			r.Detail += " (its kran is older)"
		}
	case v <= proto.PipeUserPagesDefault:
		r.Level = Warn
		r.Detail += fmt.Sprintf(", the kernel default: once the jobs' user holds %d pages of pipes, every new pipe gets one page "+
			"and a program writing more than 4 KiB into a pipe nobody reads yet blocks", v)
		r.Fix = fix
	case v < proto.PipeUserPagesWanted:
		r.Level = Info
		r.Detail += fmt.Sprintf(", above the kernel default; kran suggests %d for many parallel jobs", proto.PipeUserPagesWanted)
		r.Fix = fix
	}
	return r
}

// readPipePages reads this machine's fs.pipe-user-pages-soft (0 = unknown,
// not Linux).
func readPipePages() int64 {
	b, err := os.ReadFile("/proc/sys/fs/pipe-user-pages-soft")
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}
