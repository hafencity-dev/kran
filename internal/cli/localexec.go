package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// localStdio are the descriptors the child inherits. Inheriting (instead of
// piping) keeps colors, progress bars and interactive tools working.
type localStdio struct{ In, Out, Err *os.File }

func stdStdio() localStdio { return localStdio{os.Stdin, os.Stdout, os.Stderr} }

// localJob describes a command the CLI executes itself (SPEC §6.1 step 5).
type localJob struct {
	JobID    string
	Node     string // this node's name, for messages ("" = here)
	Argv     []string
	Dir      string
	Env      []string // full child environment
	Envelope *proto.LocalEnvelope
	Stdio    localStdio
	// ScopeProbe reports whether a transient systemd scope with this memory cap
	// can be created. nil uses the real probe.
	ScopeProbe func(memMax int64) bool
	// Tee receives a copy of the output for the job history; nil lets the
	// child inherit the descriptors directly.
	Tee func(proto.FrameType, []byte)
	// Watcher, when set, guards a job no daemon looks after: the job dies
	// with kran run, even by SIGKILL (lifeline.go; on Linux the leader
	// also gets a parent-death signal).
	Watcher func(pgid int) *exec.Cmd
}

// localProc is a running local job.
type localProc struct {
	cmd      *exec.Cmd
	Started  proto.Started
	start    time.Time
	scope    string
	memMax   int64    // the scope's MemoryMax
	node     string   // for the memory cap message
	fgTTY    *os.File // terminal we handed to the child; reclaimed after Wait
	capture  *outputCapture
	life     *lifeline
	resultCh chan proto.Exit
}

// captureGrace is how long output may still arrive after the child exited
// (a background grandchild holding the pipe must not keep kran run alive).
const captureGrace = 500 * time.Millisecond

// localError carries the exit code a shell would use for a launch failure.
type localError struct {
	Code int
	Msg  string
}

func (e *localError) Error() string { return e.Msg }

// lookPath resolves name against the PATH in env like a shell, relative to dir.
func lookPath(name string, env []string, dir string) (string, *localError) {
	check := func(p string) *localError {
		st, err := os.Stat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return &localError{127, name + ": command not found"}
		case err != nil:
			return &localError{126, fmt.Sprintf("%s: %v", name, err)}
		case st.IsDir():
			return &localError{126, name + ": is a directory"}
		case st.Mode().Perm()&0o111 == 0:
			return &localError{126, name + ": permission denied (not executable)"}
		}
		return nil
	}
	if strings.ContainsRune(name, '/') {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if e := check(p); e != nil {
			return "", e
		}
		return p, nil
	}
	pathVar := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVar = v
		}
	}
	var denied bool
	for _, d := range filepath.SplitList(pathVar) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, name)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if st.Mode().Perm()&0o111 == 0 {
			denied = true
			continue
		}
		return p, nil
	}
	if denied {
		return "", &localError{126, name + ": permission denied (not executable)"}
	}
	return "", &localError{127, name + ": command not found"}
}

// startLocal launches the job in its own process group (inside a transient
// systemd scope on Linux when the envelope asks for a memory cap).
func startLocal(j localJob) (*localProc, *localError) {
	if len(j.Argv) == 0 {
		return nil, &localError{126, "empty command"}
	}
	env := append([]string(nil), j.Env...)
	if j.Envelope != nil {
		for k, v := range j.Envelope.Env {
			env = setEnv(env, k, v)
		}
	}
	if st, err := os.Stat(j.Dir); err != nil || !st.IsDir() {
		return nil, &localError{126, "working directory " + j.Dir + " does not exist"}
	}
	prog, lerr := lookPath(j.Argv[0], env, j.Dir)
	if lerr != nil {
		return nil, lerr
	}
	argv := append([]string{prog}, j.Argv[1:]...)
	scope := ""
	if j.Envelope != nil && j.Envelope.MemMax > 0 {
		probe := j.ScopeProbe
		if probe == nil {
			probe = scopeUsable
		}
		if probe(j.Envelope.MemMax) {
			id := j.JobID
			if id == "" {
				id = strconv.Itoa(os.Getpid())
			}
			slice := j.Envelope.Slice
			if slice == "" {
				slice = "kran.slice"
			}
			scope = "kran-local-" + id + ".scope"
			// No --collect: a scope the kernel OOM-killed must stay
			// loaded (failed, Result=oom-kill) until wait has read why.
			argv = append([]string{"systemd-run", "--user", "--scope", "--quiet",
				"--slice=" + slice, "--unit=kran-local-" + id,
				// No swap beyond the cap: a job over it is killed and told so
				// (SPEC §6.8, as on remote targets), instead of swapping the
				// machine to a crawl.
				"-p", "MemoryMax=" + strconv.FormatInt(j.Envelope.MemMax, 10), "-p", "MemorySwapMax=0",
				// A runaway fork loop stops at the cap instead of taking
				// the machine with it.
				"-p", "TasksMax=" + strconv.Itoa(proto.JobTasksMax), "--"}, argv...)
			if p, e := exec.LookPath("systemd-run"); e == nil {
				argv[0] = p
			}
		}
	}
	cmd := &exec.Cmd{
		Path: argv[0], Args: argv, Dir: j.Dir, Env: env,
		Stdin: j.Stdio.In, Stdout: j.Stdio.Out, Stderr: j.Stdio.Err,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	var fg *os.File
	var capture *outputCapture
	if j.Tee != nil {
		// Without a capture (no pty available …) the job still runs; only
		// its history stays empty.
		capture, _ = newCapture(j.Stdio, j.Tee)
	}
	if capture != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = capture.stdin, capture.stdout, capture.stderr
		cmd.SysProcAttr = capture.sysProcAttr()
		if capture.foreground {
			fg = j.Stdio.In
		}
	} else if j.Stdio.In != nil && isTerminal(j.Stdio.In) {
		// Make the child the foreground group so it can read the terminal and
		// receives Ctrl-C directly, exactly like a shell would.
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = int(j.Stdio.In.Fd())
		fg = j.Stdio.In
	}
	start := time.Now()
	startCmd := cmd.Start
	if j.Watcher != nil {
		startCmd = func() error { return startWithDeathSignal(cmd) }
	}
	if err := startCmd(); err != nil {
		if capture != nil {
			capture.abort()
		}
		code := 126
		if errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		return nil, &localError{code, fmt.Sprintf("%s: %v", j.Argv[0], err)}
	}
	if j.Envelope != nil && j.Envelope.Nice != 0 {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, j.Envelope.Nice)
	}
	if capture != nil {
		capture.started()
	}
	p := &localProc{
		cmd: cmd, start: start, scope: scope, fgTTY: fg, node: j.Node, capture: capture, life: startLifeline(cmd.Process.Pid, j.Watcher),
		Started:  proto.Started{JobID: j.JobID, PID: cmd.Process.Pid, PGID: cmd.Process.Pid, Scope: scope},
		resultCh: make(chan proto.Exit, 1),
	}
	if scope != "" {
		p.memMax = j.Envelope.MemMax
	}
	go p.wait()
	return p, nil
}

func setEnv(env []string, k, v string) []string {
	prefix := k + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + v
			return env
		}
	}
	return append(env, prefix+v)
}

func (p *localProc) wait() {
	err := p.cmd.Wait()
	ex := proto.Exit{JobID: p.Started.JobID}
	ps := p.cmd.ProcessState
	if ps != nil {
		ex.Code, ex.Signal = exitFromState(ps)
		ex.Usage = usageFromState(ps, time.Since(p.start))
	} else if err != nil {
		ex.Code = proto.ExitInfra
		ex.Reason = err.Error()
	}
	if ex.Code == 0 {
		ex.State = proto.JobSucceeded
	} else {
		ex.State = proto.JobFailed
	}
	if p.capture != nil {
		p.capture.finish(captureGrace)
	}
	p.life.release()
	if p.scope != "" && ex.Signal == "KILL" && scopeOOMKilled(p.scope) {
		where := "here"
		if p.node != "" {
			where = "on " + p.node
		}
		ex.Reason = fmt.Sprintf("job hit its %s memory cap %s — rerun with --mem %dG", humanBytes(p.memMax), where, max(2, (2*p.memMax+(1<<30)-1)>>30))
	}
	if p.fgTTY != nil {
		reclaimTerminal(p.fgTTY)
	}
	p.resultCh <- ex
}

// resize passes a terminal size change to the job's pty, if it has one.
func (p *localProc) resize() {
	if p.capture != nil {
		p.capture.resize()
	}
}

// Done delivers the exit once.
func (p *localProc) Done() <-chan proto.Exit { return p.resultCh }

var signalNames = map[string]syscall.Signal{
	"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL,
	"HUP": syscall.SIGHUP, "QUIT": syscall.SIGQUIT,
}

// Signal sends the named signal to the job's whole process group.
func (p *localProc) Signal(name string) error {
	sig, ok := signalNames[name]
	if !ok {
		return fmt.Errorf("unknown signal %q", name)
	}
	if err := syscall.Kill(-p.Started.PGID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if sig == syscall.SIGKILL && p.scope != "" {
		// Descendants that left the process group are still in the scope.
		go func() {
			_ = exec.Command("systemctl", "--user", "kill", "--kill-whom=all", "--signal=SIGKILL", p.scope).Run()
		}()
	}
	return nil
}

// exitFromState maps a wait status the way a shell reports it.
func exitFromState(ps *os.ProcessState) (int, string) {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return proto.ExitSignalBase + int(ws.Signal()), signalName(ws.Signal())
	}
	return ps.ExitCode(), ""
}

func signalName(s syscall.Signal) string {
	for n, v := range signalNames {
		if v == s {
			return n
		}
	}
	return strings.TrimPrefix(strings.ToUpper(s.String()), "SIG")
}

func usageFromState(ps *os.ProcessState, wall time.Duration) proto.Usage {
	u := proto.Usage{WallSeconds: wall.Seconds(), CPUSeconds: (ps.UserTime() + ps.SystemTime()).Seconds()}
	if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
		u.PeakMem = maxRSSBytes(ru.Maxrss)
	}
	return u
}

// reclaimTerminal takes the foreground back after a job that owned it.
func reclaimTerminal(tty *os.File) {
	// Background processes calling tcsetpgrp get SIGTTOU; ignore it briefly.
	ignoreTTOU()
	_ = unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSPGRP, syscall.Getpgrp())
}
