// Package link implements `kran link` and `kran unlink`: bootstrapping a
// second machine over the system ssh, installing kran there, exchanging
// kran's own keys (forced-command authorized_keys entries) and pinning host
// keys. All remote mutation goes through the remote's own kran binary
// (`kran internal link-*`), so the editing code is identical on both ends.
package link

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Cmd is one external command invocation.
type Cmd struct {
	Name  string
	Args  []string
	Stdin io.Reader
	Env   []string // appended to the process environment
	// Inherit attaches the terminal (ssh password and host key prompts).
	Inherit bool
	Dir     string
}

// Output is what a command printed.
type Output struct {
	Stdout []byte
	Stderr []byte
}

// ExitError is returned by a Runner when the command ran and failed.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	s := strings.TrimSpace(e.Stderr)
	if s == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("exit status %d: %s", e.Code, lastLines(s, 3))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// Runner runs external commands (ssh, ssh-keyscan, tailscale, gh, go).
// It is injectable so tests never touch the network or the real ssh.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Output, error)
}

// ExecRunner runs real processes.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, c Cmd) (Output, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	var out, errb bytes.Buffer
	if c.Inherit {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	} else {
		cmd.Stdin = c.Stdin
		cmd.Stdout, cmd.Stderr = &out, &errb
	}
	err := cmd.Run()
	o := Output{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return o, &ExitError{Code: ee.ExitCode(), Stderr: errb.String()}
	}
	return o, err
}

func (o *Options) runner() Runner {
	if o.Runner != nil {
		return o.Runner
	}
	return ExecRunner{}
}

func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

func errStderr(err error) string {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Stderr
	}
	return ""
}

// shq single-quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// remoteWord renders a path for the remote shell, expanding a leading ~ to
// the remote $HOME (the local shell would have expanded it to ours).
func remoteWord(p string) string {
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		return `"$HOME"/` + shq(p[2:])
	}
	return shq(p)
}
