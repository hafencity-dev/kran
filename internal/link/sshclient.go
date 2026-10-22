package link

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// sshClient drives the system ssh with one multiplexed connection, so the
// user's config, agent, jump hosts and known_hosts all apply and a password
// is typed at most once.
type sshClient struct {
	o    *Options
	r    Runner
	dest string
	ctl  string
	dir  string // private directory holding ctl ("" = no multiplexing)
}

// newSSH puts the ControlPath in a fresh 0700 directory: ssh connects to
// whatever socket already sits at that path, so in a shared /tmp another
// account could otherwise answer as our master. Without one, ssh runs
// without multiplexing.
func newSSH(o *Options) *sshClient {
	base := os.TempDir()
	if len(base) > 40 { // unix socket paths max out around 104 bytes
		base = "/tmp"
	}
	c := &sshClient{o: o, r: o.runner(), dest: o.Host, ctl: "none"}
	if dir, err := os.MkdirTemp(base, "kran-ssh-"); err == nil {
		c.dir, c.ctl = dir, dir+"/%C"
	}
	return c
}

func (c *sshClient) args(batch bool, extra ...string) []string {
	a := []string{"-o", "ConnectTimeout=10", "-o", "ControlPath=" + c.ctl}
	if c.dir != "" {
		a = append(a, "-o", "ControlMaster=auto", "-o", "ControlPersist=60")
	}
	if batch {
		a = append(a, "-o", "BatchMode=yes")
	}
	a = append(a, c.o.SSHArgs...)
	a = append(a, c.dest)
	return append(a, extra...)
}

// connect opens the shared connection. With a terminal, a failed
// non-interactive attempt is retried interactively so the user can accept a
// new host key or type a password once.
func (c *sshClient) connect(ctx context.Context) error {
	_, err := c.r.Run(ctx, Cmd{Name: "ssh", Args: c.args(true, "true")})
	if err == nil {
		return nil
	}
	if isNoSSH(err) {
		return errors.New("ssh was not found on this machine; install OpenSSH and try again")
	}
	se := errStderr(err)
	needsUser := strings.Contains(se, "Permission denied") || strings.Contains(se, "Host key verification failed") ||
		strings.Contains(se, "passphrase") || strings.Contains(se, "password")
	if needsUser && c.o.Interactive {
		_, err2 := c.r.Run(ctx, Cmd{Name: "ssh", Args: c.args(false, "true"), Inherit: true})
		if err2 == nil {
			return nil
		}
		err = err2
		se = errStderr(err)
	}
	return c.explain(err, se)
}

func isNoSSH(err error) bool {
	return err != nil && (errors.Is(err, errNoExec) || strings.Contains(err.Error(), "executable file not found"))
}

var errNoExec = errors.New("executable file not found")

func (c *sshClient) explain(err error, se string) error {
	h := c.dest
	switch {
	case strings.Contains(se, "Host key verification failed"):
		return fmt.Errorf("ssh does not know %s yet. Run `ssh %s` once to accept its host key, then run kran link again", h, h)
	case strings.Contains(se, "Permission denied"):
		return fmt.Errorf("ssh to %s was refused (%s). Make sure `ssh %s` works without a password prompt (ssh-copy-id or ssh-agent), then run kran link again", h, lastLines(se, 1), h)
	case strings.Contains(se, "Could not resolve hostname"), strings.Contains(se, "Connection refused"),
		strings.Contains(se, "timed out"), strings.Contains(se, "No route to host"):
		return fmt.Errorf("cannot reach %s over ssh (%s). Check that the machine is on, SSH (Remote Login) is enabled and the name is right", h, lastLines(se, 1))
	}
	return fmt.Errorf("ssh to %s failed: %w", h, err)
}

// run executes script on the remote with `sh -c`, so the user's login shell
// (fish, csh, zsh) never interprets it.
func (c *sshClient) run(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	out, err := c.r.Run(ctx, Cmd{Name: "ssh", Args: c.args(true, "sh -c "+shq(script)), Stdin: stdin})
	if err != nil {
		if se := errStderr(err); se != "" {
			return out.Stdout, fmt.Errorf("on %s: %s", c.dest, lastLines(se, 3))
		}
		return out.Stdout, fmt.Errorf("on %s: %w", c.dest, err)
	}
	return out.Stdout, nil
}

func envPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s; ", k, remoteWord(env[k]))
	}
	return b.String()
}

// kran runs `<kranPath> args…` on the remote with the dev env overrides.
func (c *sshClient) kran(ctx context.Context, kranPath string, stdin io.Reader, args ...string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(envPrefix(c.o.RemoteEnv))
	b.WriteString("exec " + remoteWord(kranPath))
	for _, a := range args {
		b.WriteString(" " + shq(a))
	}
	return c.run(ctx, b.String(), stdin)
}

// exitMaster closes the multiplexed connection.
func (c *sshClient) exitMaster() {
	if c.dir == "" {
		return
	}
	defer func() { os.RemoveAll(c.dir); c.dir = "" }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := []string{"-o", "ControlPath=" + c.ctl, "-O", "exit"}
	a = append(a, c.o.SSHArgs...)
	a = append(a, c.dest)
	_, _ = c.r.Run(ctx, Cmd{Name: "ssh", Args: a})
}

// resolve runs `ssh -G` to learn what the user's config says.
func (c *sshClient) resolve(ctx context.Context) (sshResolved, error) {
	a := append([]string{"-G"}, c.o.SSHArgs...)
	a = append(a, c.dest)
	out, err := c.r.Run(ctx, Cmd{Name: "ssh", Args: a})
	if err != nil {
		return sshResolved{}, fmt.Errorf("ssh -G %s: %w", c.dest, err)
	}
	r := parseSSHG(string(out.Stdout))
	if r.Hostname == "" {
		return r, fmt.Errorf("ssh could not resolve %q; check your ssh config", c.dest)
	}
	return r, nil
}

// rtt measures round trips on the open connection (median of n).
func (c *sshClient) rtt(ctx context.Context, n int) time.Duration {
	var ds []time.Duration
	for i := 0; i < n; i++ {
		t := time.Now()
		if _, err := c.r.Run(ctx, Cmd{Name: "ssh", Args: c.args(true, "true")}); err == nil {
			ds = append(ds, time.Since(t))
		}
	}
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// bandwidth pushes size bytes through the connection and returns bytes/s.
func (c *sshClient) bandwidth(ctx context.Context, size int) float64 {
	t := time.Now()
	_, err := c.run(ctx, "cat >/dev/null", io.LimitReader(zeroReader{}, int64(size)))
	el := time.Since(t).Seconds()
	if err != nil || el <= 0 {
		return 0
	}
	return float64(size) / el
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
