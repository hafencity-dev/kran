package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func (c *checker) checkRuntimes(ctx context.Context) {
	c.connect(ctx)
	if c.state != nil {
		for _, n := range c.state.Nodes {
			if c.o.Node != "" && n.Info.Name != c.o.Node || (c.o.Node == "" && !n.Self) {
				continue
			}
			for _, rt := range n.Info.Runtimes {
				if rt.Available {
					c.add(Result{ID: "runtime." + string(rt.Kind), Title: n.Info.Name + " " + string(rt.Kind) + " runtime", Level: OK, Node: n.Info.Name,
						Detail: fmt.Sprintf("%s/%s, %d cpus, %s", rt.OS, rt.Arch, rt.CPUs, gib(rt.Mem))})
					if rt.Caches != nil {
						c.add(cacheResult(n.Info.Name, rt.Caches))
					}
					continue
				}
				lvl := Info
				if n.Info.OS == "darwin" && rt.Kind == "container" {
					lvl = Warn // without it a Mac cannot run Linux jobs
				}
				fix := ""
				if rt.Kind == "container" {
					fix = c.dockerHint()
				}
				c.add(Result{ID: "runtime." + string(rt.Kind), Title: n.Info.Name + " " + string(rt.Kind) + " runtime", Level: lvl, Node: n.Info.Name, Detail: "unavailable: " + rt.Reason, Fix: fix})
			}
		}
		return
	}
	// No daemon: probe docker directly.
	if c.o.Node != "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := c.o.LookPath("docker"); err != nil {
		c.add(Result{ID: "runtime.docker", Title: "docker", Level: Info, Detail: "not installed; only native jobs can run here", Fix: c.dockerHint()})
		return
	}
	if out, err := c.o.Exec(cctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		c.add(Result{ID: "runtime.docker", Title: "docker", Level: Warn, Detail: "installed but the daemon is not reachable: " + firstLine(out), Fix: c.dockerHint()})
	} else {
		c.add(Result{ID: "runtime.docker", Title: "docker", Level: OK, Detail: "server " + strings.TrimSpace(out)})
	}
}

// cacheResult reports the container runtime's shared caches as their
// last trim (hourly, by the node's daemon) left them.
func cacheResult(node string, cs *proto.CacheStatus) Result {
	r := Result{ID: "runtime.container.caches", Title: node + " container caches", Level: OK, Node: node,
		Detail: fmt.Sprintf("%s of %s budget; Docker disk %s free of %s", gib(cs.Used), gib(cs.Budget), gib(cs.DiskFree), gib(cs.DiskTotal))}
	if cs.Reserve > 0 {
		r.Detail += fmt.Sprintf("; kran keeps %s free (accept.disk_reserve)", gib(cs.Reserve))
	}
	if cs.Reclaimed > 0 {
		r.Detail += fmt.Sprintf("; the last trim reclaimed %s", gib(cs.Reclaimed))
	}
	if floor := max(int64(10<<30), cs.DiskTotal/20); cs.DiskFree < floor {
		r.Level = Warn
		r.Detail += fmt.Sprintf("; below the floor of %s", gib(floor))
		r.Fix = "free space in the Docker VM (`docker system prune`), give it a bigger disk, or lower runtime.container.cache_budget on " + node + "; kran already removes idle slots there"
	}
	return r
}

func (c *checker) dockerHint() string {
	if c.o.GOOS == "darwin" {
		return "start your container runtime, e.g. `colima start` or open Docker Desktop"
	}
	return "start docker: `sudo systemctl start docker` (your user must be in the docker group)"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (c *checker) checkToolchains(ctx context.Context) {
	if c.o.Node != "" {
		return
	}
	var found, missing []string
	for _, t := range []string{"git", "go", "node", "pnpm", "python3", "cargo", "mise", "docker"} {
		if _, err := c.o.LookPath(t); err == nil {
			found = append(found, t)
		} else {
			missing = append(missing, t)
		}
	}
	lvl := Info
	fix := ""
	if contains(missing, "git") {
		lvl, fix = Warn, "install git: kran needs it to detect repositories and build the file list"
	}
	d := "found " + strings.Join(found, ", ")
	if len(missing) > 0 {
		d += "; not found: " + strings.Join(missing, ", ") + " (only matters if your jobs use them; `mise` lets kran fetch pinned versions)"
	}
	c.add(Result{ID: "toolchains", Title: "toolchains on PATH", Level: lvl, Detail: d, Fix: fix})
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
