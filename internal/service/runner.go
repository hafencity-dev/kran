package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// RunError is a failed command with its stderr.
type RunError struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *RunError) Error() string {
	if s := strings.TrimSpace(e.Stderr); s != "" {
		return fmt.Sprintf("%s: %v: %s", e.Cmd, e.Err, s)
	}
	return fmt.Sprintf("%s: %v", e.Cmd, e.Err)
}
func (e *RunError) Unwrap() error { return e.Err }

type execRunner struct {
	uid    int
	getenv func(string) string
}

func (r *execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = r.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), &RunError{Cmd: name + " " + strings.Join(args, " "), Stderr: errb.String(), Err: err}
	}
	return out.String(), nil
}

// env makes `systemctl --user` work from ssh sessions that lack a user bus
// environment, when the bus exists.
func (r *execRunner) env() []string {
	env := os.Environ()
	xdg := r.getenv("XDG_RUNTIME_DIR")
	if xdg == "" && r.uid > 0 {
		dir := "/run/user/" + strconv.Itoa(r.uid)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			xdg = dir
			env = append(env, "XDG_RUNTIME_DIR="+dir)
		}
	}
	if xdg != "" && r.getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		if _, err := os.Stat(xdg + "/bus"); err == nil {
			env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+xdg+"/bus")
		}
	}
	return env
}
