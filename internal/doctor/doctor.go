// Package doctor implements `kran doctor [--fix] [node]`: checks that say
// what is wrong, what to do about it, and fix the safe ones on request.
package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/service"
	"github.com/hafencity-dev/kran/internal/version"
)

// Level is the severity of a result.
type Level string

const (
	OK   Level = "ok"
	Info Level = "info"
	Warn Level = "warn"
	Fail Level = "fail"
)

// Result is one finding. Fix is a sentence or command for the user; when the
// check can repair itself, --fix runs it and sets Fixed.
type Result struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Level  Level  `json:"level"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
	Fixed  bool   `json:"fixed,omitempty"`
	Node   string `json:"node,omitempty"`

	fix func(ctx context.Context) error
}

// Options configure Run; zero values use the real system.
type Options struct {
	Fix  bool
	Node string // restrict peer checks to this node ("" = all)

	Dial     func(ctx context.Context) (client.Client, error)
	Service  func() (service.Manager, error)
	Signer   func() (ssh.Signer, error)
	Exec     func(ctx context.Context, name string, args ...string) (string, error)
	LookPath func(string) (string, error)
	DiskFree func(path string) (free, total int64, err error)
	Now      func() time.Time
	GOOS     string
	// Paths (defaults from internal/paths and config).
	Peers func() (*config.Peers, error)
}

func (o *Options) defaults() {
	if o.Dial == nil {
		o.Dial = client.Dial
	}
	if o.Signer == nil {
		o.Signer = config.Signer
	}
	if o.Exec == nil {
		o.Exec = func(ctx context.Context, name string, args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		}
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.DiskFree == nil {
		o.DiskFree = diskFree
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Peers == nil {
		o.Peers = func() (*config.Peers, error) { return config.LoadPeers("") }
	}
}

type checker struct {
	o       Options
	state   *proto.State
	daemon  *proto.VersionInfo
	results []Result
}

func (c *checker) add(r Result) { c.results = append(c.results, r) }

// Run executes every check and, with o.Fix, repairs what it safely can.
func Run(ctx context.Context, o Options) []Result {
	o.defaults()
	c := &checker{o: o}
	c.checkIdentity(ctx)
	c.checkFiles(ctx)
	c.checkService(ctx)
	c.checkVersion(ctx)
	c.checkLocalResources(ctx)
	c.checkRuntimes(ctx)
	c.checkToolchains(ctx)
	c.checkPeers(ctx)
	if o.Fix {
		for i := range c.results {
			r := &c.results[i]
			if r.fix == nil || r.Level == OK {
				continue
			}
			if err := r.fix(ctx); err != nil {
				r.Detail = strings.TrimSpace(r.Detail + " (automatic fix failed: " + err.Error() + ")")
				continue
			}
			r.Fixed = true
			r.Level = OK
		}
	}
	return c.results
}

// Failed reports whether any unfixed result is a failure.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Level == Fail {
			return true
		}
	}
	return false
}

// connect opens the daemon client once and caches state/version.
func (c *checker) connect(ctx context.Context) {
	if c.daemon != nil {
		return
	}
	cl, err := c.o.Dial(ctx)
	if err != nil {
		return
	}
	defer cl.Close()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	v, err := cl.Version(cctx)
	if err != nil {
		return
	}
	c.daemon = &v
	if st, err := cl.State(cctx); err == nil {
		c.state = st
	}
}

func (c *checker) idle(ctx context.Context) (bool, string, error) {
	cl, err := c.o.Dial(ctx)
	if err != nil {
		return true, "", nil
	}
	defer cl.Close()
	st, err := cl.State(ctx)
	if err != nil {
		return true, "", nil
	}
	n := 0
	for _, j := range st.Jobs {
		if j.State.Starting() {
			n++
		}
	}
	if n > 0 {
		return false, fmt.Sprintf("%d jobs are starting", n), nil
	}
	return true, "", nil
}

func (c *checker) checkVersion(ctx context.Context) {
	c.connect(ctx)
	if c.daemon == nil {
		return
	}
	if c.daemon.Version != version.Version {
		r := Result{ID: "version.daemon", Title: "daemon version", Level: Warn,
			Detail: fmt.Sprintf("the daemon runs %s but this binary is %s", c.daemon.Version, version.Version),
			Fix:    "kran service restart (waits for running jobs)"}
		r.fix = func(ctx context.Context) error {
			m, err := c.o.Service()
			if err != nil {
				return err
			}
			return m.Restart(ctx, service.RestartOptions{Idle: c.idle, Wait: 30 * time.Second})
		}
		c.add(r)
	} else {
		c.add(Result{ID: "version.daemon", Title: "daemon version", Level: OK, Detail: version.Version})
	}
	if c.state == nil {
		return
	}
	for _, n := range c.state.Nodes {
		if n.Self || (c.o.Node != "" && n.Info.Name != c.o.Node) || n.Info.Version == "" {
			continue
		}
		if n.Info.Version != version.Version {
			c.add(Result{ID: "version.peer", Title: "version of " + n.Info.Name, Level: Warn, Node: n.Info.Name,
				Detail: fmt.Sprintf("%s runs kran %s, this machine runs %s", n.Info.Name, n.Info.Version, version.Version),
				Fix:    "kran upgrade"})
		}
	}
}
