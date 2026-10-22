package execenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Supervise is the entry point of `kran internal supervise <job dir>`
// (SPEC §6.2). The manager starts it detached (own session; on Linux in its
// own systemd scope) so it survives the daemon. It owns the job's process
// tree, writes the output spool and the final status, enforces the lease
// and the memory cap, and exits when the job is over.
func Supervise(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: kran internal supervise <job dir>")
		return 2
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "kran supervise:", err)
		return 2
	}
	s, err := newSupervisor(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kran supervise:", err)
		return 1
	}
	return s.run()
}

type supervisor struct {
	dir   string
	spec  superSpec
	logf  func(string, ...any)
	lockF *os.File
	spool *spoolWriter
	evMu  sync.Mutex
	evF   *os.File
	ln    net.Listener
	cg    *jobCgroup

	mu         sync.Mutex
	cmd        *exec.Cmd     // running step, nil between steps
	exited     chan struct{} // closed when cmd has been waited for
	pgid       int
	ptmx       *os.File // job terminal (TTY jobs), created before the job starts
	ptySlave   *os.File
	stdinR     *os.File // job stdin pipe (Stdin jobs), created before the job starts
	stdinW     *os.File
	stdinEOF   bool
	container  string // container of the running step (docker)
	killReason string // "lost" | "canceled" | "oom"; first wins
	killing    bool

	statMu   sync.Mutex
	peakMem  int64
	treeCPU  float64 // last sampled CPU seconds of the tree (pgid/docker)
	lastCPU  float64
	lastTime time.Time
	api      *dockerAPI // container jobs: stats probe

	sampleStop chan struct{} // nil until the sampler runs
	sampleDone chan struct{}
	stopOnce   sync.Once
	closeOnce  sync.Once
}

const (
	killLost     = "lost"
	killCanceled = "canceled"
	killOOM      = "oom"
)

func newSupervisor(dir string) (*supervisor, error) {
	s := &supervisor{dir: dir}
	if err := readJSON(filepath.Join(dir, fileSpec), &s.spec); err != nil {
		return nil, fmt.Errorf("read job spec: %w", err)
	}
	if s.spec.LeaseTTL <= 0 {
		s.spec.LeaseTTL = LeaseTTL
	}
	if s.spec.KillGrace <= 0 {
		s.spec.KillGrace = 5 * time.Second
	}
	if s.spec.SampleEvery <= 0 {
		s.spec.SampleEvery = time.Second
	}
	lg := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	s.logf = func(f string, a ...any) { lg.Printf("job %s: "+f, append([]any{s.spec.JobID}, a...)...) }
	return s, nil
}

func (s *supervisor) run() int {
	f, err := os.OpenFile(filepath.Join(s.dir, fileLock), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		s.logf("lock: %v", err)
		return 1
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		s.logf("another supervisor owns this job: %v", err)
		return 1
	}
	s.lockF = f // held until the process exits
	// The env now lives in this process only; a reattach needs none of it.
	if err := writeJSONAtomic(filepath.Join(s.dir, fileSpec), s.spec.redacted()); err != nil {
		s.logf("redact spec: %v", err)
	}

	signal.Ignore(syscall.SIGHUP)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for range sigs {
			s.terminate(killCanceled, s.spec.KillGrace)
		}
	}()

	if s.spec.Nice > 0 {
		unix.Setpriority(unix.PRIO_PROCESS, 0, s.spec.Nice)
	}
	if s.spec.Cgroup && s.spec.Docker == nil {
		if cg, err := setupJobCgroup(s.spec.Scope, s.spec.MemMax); err != nil {
			s.logf("cgroup setup failed, using process groups: %v", err)
		} else {
			s.cg = cg
			defer cg.close()
		}
	}
	exit := proto.Exit{JobID: s.spec.JobID, Node: s.spec.Node}
	if err := s.open(); err != nil {
		s.logf("%v", err)
		exit.State, exit.Code, exit.BeforeStart = proto.JobInfraErr, proto.ExitInfra, true
		exit.Reason = fmt.Sprintf("kran could not start the job on %s: %v", s.spec.Node, err)
		s.writeStatus(exit)
		return 1
	}
	defer s.closeAll()
	if err := s.prepareStdio(); err != nil {
		exit.State, exit.Code, exit.BeforeStart = proto.JobInfraErr, proto.ExitInfra, true
		exit.Reason = fmt.Sprintf("kran could not start the job on %s: %v", s.spec.Node, err)
		s.writeStatus(exit)
		return 1
	}

	ev := event{Ev: "ready", PID: os.Getpid()}
	if s.cg != nil {
		ev.Cgroup = filepath.Dir(s.cg.dir)
	}
	s.event(ev)
	go s.watchLease()
	s.sampleStop, s.sampleDone = make(chan struct{}), make(chan struct{})
	go s.sample()

	var usage proto.Usage
	if st := s.spec.Setup; st != nil {
		s.event(event{Ev: "phase", State: proto.JobSetup, Message: "running setup: " + st.Run})
		t0 := time.Now()
		res := s.runStep(true)
		usage.SetupSeconds = time.Since(t0).Seconds()
		if res.code != 0 || res.err != nil || s.reason() != "" {
			exit = s.setupExit(res)
			exit.Usage = usage
			s.writeStatus(exit)
			return 0
		}
		if st.KeyFile != "" {
			if err := writeFileAtomic(st.KeyFile, []byte(st.Key+"\n"), 0o600); err != nil {
				s.logf("record setup key: %v", err)
			}
		}
		s.resetPeak()
		// The origin may have given up on the job while the setup ran (its
		// daemon or ours died, the link was cut) and may run it elsewhere
		// now. Start the command only after a heartbeat newer than the
		// setup; without one the lease expires and the job ends before
		// its command ran, so it never runs twice.
		if !s.awaitRenewal(time.Now()) {
			exit = s.setupExit(stepResult{code: proto.ExitSignalBase + int(syscall.SIGTERM)})
			exit.Usage = usage
			s.writeStatus(exit)
			return 0
		}
	}
	t0 := time.Now()
	res := s.runStep(false)
	usage.WallSeconds = time.Since(t0).Seconds()
	exit = s.mainExit(res)
	usage.CPUSeconds, usage.PeakMem = res.cpu, res.peak
	exit.Usage = usage
	s.writeStatus(exit)
	return 0
}

func (s *supervisor) open() error {
	var err error
	if s.spool, err = openSpool(filepath.Join(s.dir, fileOut)); err != nil {
		return fmt.Errorf("output spool: %w", err)
	}
	if s.evF, err = os.OpenFile(filepath.Join(s.dir, fileEvents), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err != nil {
		return fmt.Errorf("events: %w", err)
	}
	if s.spec.CtlPath != "" {
		if err := privateDir(filepath.Dir(s.spec.CtlPath)); err != nil {
			return fmt.Errorf("control socket dir: %w", err)
		}
		os.Remove(s.spec.CtlPath)
		if s.ln, err = net.Listen("unix", s.spec.CtlPath); err != nil {
			return fmt.Errorf("control socket: %w", err)
		}
		os.Chmod(s.spec.CtlPath, 0o600)
		go s.serveCtl()
	}
	return nil
}

// prepareStdio creates the job's terminal or stdin pipe before reporting
// ready, so input and resizes sent right after Start are never lost: they
// wait in the pipe or the terminal until the command reads them.
func (s *supervisor) prepareStdio() error {
	switch {
	case s.spec.TTY != nil:
		m, t, err := pty.Open()
		if err != nil {
			return fmt.Errorf("pty: %w", err)
		}
		pty.Setsize(m, &pty.Winsize{Rows: s.spec.TTY.Rows, Cols: s.spec.TTY.Cols})
		s.ptmx, s.ptySlave = m, t
	case s.spec.Stdin:
		r, w, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("stdin pipe: %w", err)
		}
		s.stdinR, s.stdinW = r, w
	}
	return nil
}

// stopSampling ends the sampler and waits until it wrote its last file.
func (s *supervisor) stopSampling() {
	s.stopOnce.Do(func() {
		if s.sampleStop != nil {
			close(s.sampleStop)
			<-s.sampleDone
		}
	})
}

func (s *supervisor) closeAll() { s.closeOnce.Do(s.closeFiles) }

func (s *supervisor) closeFiles() {
	if s.ln != nil {
		s.ln.Close()
		os.Remove(s.spec.CtlPath)
	}
	if s.spool != nil {
		s.spool.Close()
	}
	if s.evF != nil {
		s.evF.Close()
	}
	os.RemoveAll(filepath.Join(s.dir, dirTmp))
}

func (s *supervisor) event(e event) {
	e.Time = time.Now().UTC()
	b, _ := json.Marshal(e)
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if s.evF != nil {
		s.evF.Write(append(b, '\n'))
	}
}

// writeStatus is the supervisor's last write to the job dir: once the
// status exists, readers may treat the job dir as final (and tests may
// delete it), so sampling stops, files close and the per-job tmp goes
// first.
func (s *supervisor) writeStatus(exit proto.Exit) {
	s.stopSampling()
	s.event(event{Ev: "exit", State: exit.State, Message: exit.Reason})
	s.logf("ended: state=%s code=%d %s", exit.State, exit.Code, exit.Reason)
	s.closeAll()
	if err := writeJSONAtomic(filepath.Join(s.dir, fileStatus), exit); err != nil {
		s.logf("write status: %v", err)
	}
}

func (s *supervisor) reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killReason
}

// stepResult is how one step (setup or main command) ended.
type stepResult struct {
	code       int
	signal     string
	err        error // the step could not be started
	oom        bool
	cpu        float64
	peak       int64
	notStarted bool   // container never started (docker reports why)
	detail     string // runtime's own explanation
}

// runStep runs the setup (setup=true) or the main command to completion.
func (s *supervisor) runStep(setup bool) stepResult {
	if s.reason() != "" {
		return stepResult{code: proto.ExitInfra, err: errors.New("job was stopped before it started")}
	}
	cmd, name, err := s.buildCmd(setup)
	if err != nil {
		return startFailure(err)
	}
	tty := !setup && s.ptySlave != nil
	// Output reaches the spool only after the running event is written,
	// so whoever reads the spool learns that the command started before
	// seeing its first byte (the origin decides fallback by that).
	gate := make(chan struct{})
	openGate := sync.OnceFunc(func() { close(gate) })
	if setup {
		openGate()
	}
	defer openGate()
	var (
		readers  sync.WaitGroup
		closers  []*os.File
		cgBefore cgUsage
	)
	attr := &syscall.SysProcAttr{Setsid: true}
	if tty {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = s.ptySlave, s.ptySlave, s.ptySlave
		attr.Setctty = true
		attr.Ctty = 0
		closers = append(closers, s.ptySlave)
	} else {
		outR, outW, err := os.Pipe()
		if err != nil {
			return startFailure(err)
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			return startFailure(err)
		}
		cmd.Stdout, cmd.Stderr = outW, errW
		closers = append(closers, outW, errW)
		readers.Add(2)
		go s.pump(&readers, outR, proto.FrameStdout, true, gate)
		go s.pump(&readers, errR, proto.FrameStderr, true, gate)
		if !setup && s.stdinR != nil {
			cmd.Stdin = s.stdinR
			closers = append(closers, s.stdinR)
		}
	}
	cmd.SysProcAttr = attr
	if s.cg != nil {
		cgBefore = s.cg.usage()
	}
	err = s.startCmd(cmd)
	for _, c := range closers {
		c.Close()
	}
	if !setup {
		s.mu.Lock()
		s.ptySlave, s.stdinR = nil, nil
		s.mu.Unlock()
	}
	if err != nil {
		openGate()
		readers.Wait()
		return startFailure(err)
	}
	if tty {
		readers.Add(1)
		go s.pump(&readers, s.ptmx, proto.FrameStdout, false, gate)
	}
	exited := make(chan struct{})
	s.mu.Lock()
	s.cmd, s.exited, s.pgid, s.container = cmd, exited, cmd.Process.Pid, name
	killing := s.killReason != ""
	s.mu.Unlock()
	if killing {
		go s.killTree(0)
	}
	if !setup {
		s.event(event{Ev: "phase", State: proto.JobRunning, PID: cmd.Process.Pid, Container: name})
	}
	openGate()

	werr := cmd.Wait()
	close(exited)
	res := stepResult{}
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
			switch {
			case ws.Exited():
				res.code = ws.ExitStatus()
			case ws.Signaled():
				res.code = proto.ExitSignalBase + int(ws.Signal())
				res.signal = sigName(ws.Signal())
			}
		}
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			res.cpu = tvSeconds(ru.Utime) + tvSeconds(ru.Stime)
			res.peak = maxRSS(ru)
		}
	} else if werr != nil {
		res.err = werr
		res.code = proto.ExitInfra
	}

	// The step is over: whatever it left running (background daemons,
	// setsid escapees) goes too, as a systemd scope stop would do.
	s.killLeftovers(cmd.Process.Pid)
	if name != "" {
		s.dockerFinish(name, &res)
	}
	if !waitTimeout(&readers, 3*time.Second) {
		s.logf("output still open after the job ended; closing")
	}
	s.mu.Lock()
	s.cmd, s.container = nil, ""
	var ptmx *os.File
	if !setup {
		if s.stdinW != nil {
			s.stdinW.Close()
			s.stdinW = nil
		}
		ptmx, s.ptmx = s.ptmx, nil
	}
	s.mu.Unlock()
	if ptmx != nil {
		ptmx.Close()
	}

	switch {
	case s.cg != nil:
		u := s.cg.usage()
		res.cpu = u.cpu - cgBefore.cpu
		res.peak = max(u.peak, s.sampledPeak())
		if u.oomKills > cgBefore.oomKills {
			res.oom = true
		}
	case name != "":
		res.peak = max(res.peak, s.sampledPeak())
		res.cpu = max(res.cpu, s.sampledCPU())
	default:
		res.peak = max(res.peak, s.sampledPeak())
		res.cpu = max(res.cpu, s.sampledCPU())
	}
	if s.reason() == killOOM {
		res.oom = true
	}
	return res
}

func startFailure(err error) stepResult {
	code := proto.ExitInfra
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOENT):
		code = proto.ExitNotFound
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.ENOEXEC):
		code = proto.ExitNotExec
	}
	return stepResult{code: code, err: err}
}

func (s *supervisor) buildCmd(setup bool) (*exec.Cmd, string, error) {
	if d := s.spec.Docker; d != nil {
		args, name := d.RunArgs, d.Name
		if setup {
			args, name = d.SetupArgs, d.SetupName
		}
		cmd := &exec.Cmd{Path: d.CLI, Args: append([]string{d.CLI}, args...), Env: d.Env, Dir: "/"}
		return cmd, name, nil
	}
	argv, dir := s.spec.Argv, s.spec.Dir
	if setup {
		argv, dir = s.spec.Setup.Argv, s.spec.Setup.Dir
	}
	if len(argv) == 0 {
		return nil, "", errors.New("empty command")
	}
	path, err := lookPathEnv(argv[0], envGet(s.spec.Env, "PATH"), dir)
	if err != nil {
		return nil, "", err
	}
	return &exec.Cmd{Path: path, Args: argv, Env: s.spec.Env, Dir: dir}, "", nil
}

// pump copies one output stream into the spool until EOF (EIO on a pty
// whose last slave closed). Pipes are closed here; the pty master is
// closed by runStep because stdin and resize use it too.
func (s *supervisor) pump(wg *sync.WaitGroup, r *os.File, t proto.FrameType, closeAtEOF bool, gate <-chan struct{}) {
	defer wg.Done()
	buf := make([]byte, spoolChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			<-gate
			if werr := s.spool.write(t, buf[:n]); werr != nil {
				s.logf("spool write: %v", werr)
			}
		}
		if err != nil {
			if closeAtEOF {
				r.Close()
			}
			return
		}
	}
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// terminate stops the job tree: SIGTERM, then SIGKILL after grace.
func (s *supervisor) terminate(reason string, grace time.Duration) {
	s.mu.Lock()
	if s.killReason == "" {
		s.killReason = reason
	}
	already := s.killing
	s.killing = true
	running := s.cmd != nil
	s.mu.Unlock()
	if running && (!already || grace == 0) {
		go s.killTree(grace)
	}
}

func (s *supervisor) killTree(grace time.Duration) {
	s.mu.Lock()
	cmd, exited, pgid, container := s.cmd, s.exited, s.pgid, s.container
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	done := func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}
	switch {
	case container != "":
		if grace > 0 {
			s.docker("kill", "--signal", "TERM", container)
			waitUntil(done, grace)
		}
		s.docker("kill", container)
		waitUntil(done, 5*time.Second)
		syscall.Kill(-pgid, syscall.SIGKILL) // the docker CLI itself
	case s.cg != nil:
		if grace > 0 {
			s.cg.signal(syscall.SIGTERM)
			waitUntil(func() bool { return !s.cg.populated() }, grace)
		}
		s.cg.kill(5 * time.Second)
	default:
		if grace > 0 {
			syscall.Kill(-pgid, syscall.SIGTERM)
			ps := jobProcs(pgid, pgid, s.spec.JobID, true)
			signalProcs(ps, syscall.SIGTERM)
			waitUntil(func() bool { return !anyAlive(ps) }, grace)
		}
		s.sweepKill(pgid)
	}
}

// anyAlive reports whether any of ps still runs (cheap: one kill(0) each,
// unlike a fresh process-table sweep).
func anyAlive(ps []procInfo) bool {
	for _, p := range ps {
		if alive(p.pid) {
			return true
		}
	}
	return false
}

// sweepKill SIGKILLs the job tree repeatedly until nothing is left (a
// forking process can spawn children between our listing and the kill).
func (s *supervisor) sweepKill(pgid int) {
	for i := 0; i < 20; i++ {
		syscall.Kill(-pgid, syscall.SIGKILL)
		ps := jobProcs(pgid, pgid, s.spec.JobID, true)
		if len(ps) == 0 {
			return
		}
		signalProcs(ps, syscall.SIGKILL)
		time.Sleep(50 * time.Millisecond)
	}
}

// killLeftovers runs after a step's main process exited.
func (s *supervisor) killLeftovers(pgid int) {
	switch {
	case s.cg != nil:
		if s.cg.populated() {
			s.cg.signal(syscall.SIGTERM)
			waitUntil(func() bool { return !s.cg.populated() }, time.Second)
			s.cg.kill(5 * time.Second)
		}
	default:
		ps := jobProcs(pgid, pgid, s.spec.JobID, true)
		if len(ps) == 0 {
			return
		}
		syscall.Kill(-pgid, syscall.SIGTERM)
		signalProcs(ps, syscall.SIGTERM)
		waitUntil(func() bool { return !anyAlive(ps) }, time.Second)
		s.sweepKill(pgid)
	}
}

func waitUntil(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// awaitRenewal waits until the lease is renewed after `after`; false when
// the job was stopped first (lease expired or canceled).
func (s *supervisor) awaitRenewal(after time.Time) bool {
	path := filepath.Join(s.dir, fileLease)
	for s.reason() == "" {
		if fi, err := os.Stat(path); err == nil && fi.ModTime().After(after) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// watchLease ends the job when the daemon stopped renewing the lease
// (origin gone, network cut, daemon dead and not restarted).
func (s *supervisor) watchLease() {
	every := min(time.Second, s.spec.LeaseTTL/5)
	path := filepath.Join(s.dir, fileLease)
	// A renewal is a change of the mtime, timed with this process's
	// monotonic clock: the wall-clock age of the file means nothing once
	// the clock is stepped (forward: every job looks expired; back: an
	// abandoned job lives until the clock catches up).
	var seen time.Time
	renewed := time.Now()
	for {
		time.Sleep(every)
		fi, err := os.Stat(path)
		if err == nil && !fi.ModTime().Equal(seen) {
			seen, renewed = fi.ModTime(), time.Now()
		}
		if err != nil || time.Since(renewed) > s.spec.LeaseTTL {
			s.logf("lease expired (no heartbeat for %s); stopping the job", s.spec.LeaseTTL)
			s.terminate(killLost, s.spec.KillGrace)
			return
		}
	}
}

// capSampleEvery is how often a native job without a cgroup is measured
// against its memory cap.
const capSampleEvery = 200 * time.Millisecond

// sample records live usage once per SampleEvery, tracks the peak and, for
// native jobs without a cgroup, enforces the memory cap.
func (s *supervisor) sample() {
	defer close(s.sampleDone)
	every := s.spec.SampleEvery
	if s.cg == nil && s.spec.Docker == nil && s.spec.MemMax > 0 && every > capSampleEvery {
		// Without a cgroup or a container the cap is enforced only here
		// (native jobs on macOS): sample often enough to stop a job before
		// it takes the machine's memory.
		every = capSampleEvery
	}
	for {
		select {
		case <-s.sampleStop:
			return
		case <-time.After(every):
		}
		s.mu.Lock()
		cmd, pgid, container := s.cmd, s.pgid, s.container
		s.mu.Unlock()
		if cmd == nil {
			continue
		}
		var mem int64
		var cpu float64
		switch {
		case container != "":
			st, ok := s.dockerStats(container)
			if !ok {
				continue
			}
			mem, cpu = st.mem, st.cpu
		case s.cg != nil:
			u := s.cg.usage()
			mem, cpu = u.mem, u.cpu
		default:
			mem, cpu = treeUsage(jobProcs(pgid, pgid, s.spec.JobID, false))
			if s.spec.MemMax > 0 && mem > s.spec.MemMax {
				s.logf("memory %d exceeds cap %d; killing the job", mem, s.spec.MemMax)
				s.terminate(killOOM, 0)
			}
		}
		now := time.Now()
		s.statMu.Lock()
		s.peakMem = max(s.peakMem, mem)
		s.treeCPU = max(s.treeCPU, cpu)
		rate := 0.0
		if !s.lastTime.IsZero() && cpu >= s.lastCPU {
			rate = (cpu - s.lastCPU) / now.Sub(s.lastTime).Seconds()
		}
		s.lastCPU, s.lastTime = cpu, now
		s.statMu.Unlock()
		writeJSONAtomic(filepath.Join(s.dir, fileLive), liveSample{Time: now.UTC(), CPUs: rate, Mem: mem})
	}
}

func (s *supervisor) sampledPeak() int64 {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	return s.peakMem
}

func (s *supervisor) sampledCPU() float64 {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	return s.treeCPU
}

// resetPeak forgets the setup's usage so the command's own peak is learned.
func (s *supervisor) resetPeak() {
	s.statMu.Lock()
	s.peakMem, s.treeCPU, s.lastCPU, s.lastTime = 0, 0, 0, time.Time{}
	s.statMu.Unlock()
	if s.cg != nil {
		// Linux 6.12+ resets memory.peak on write; older kernels refuse.
		os.WriteFile(filepath.Join(s.cg.dir, "memory.peak"), []byte("reset"), 0)
	}
}

func (s *supervisor) setupExit(res stepResult) proto.Exit {
	exit := proto.Exit{JobID: s.spec.JobID, Node: s.spec.Node, BeforeStart: true, Code: proto.ExitInfra, State: proto.JobInfraErr}
	switch r := s.reason(); {
	case r == killLost:
		exit.State = proto.JobLost
		exit.Reason = s.lostReason()
	case r == killCanceled:
		exit.State = proto.JobCanceled
		exit.Code = res.code
		exit.Reason = "canceled during setup"
	case res.oom:
		exit.Reason = fmt.Sprintf("setup `%s` hit the job's %s memory cap on %s", s.spec.Setup.Run, fmtGiB(s.spec.MemMax), s.spec.Node)
	case res.err != nil:
		exit.Reason = fmt.Sprintf("setup `%s` could not start on %s: %v", s.spec.Setup.Run, s.spec.Node, res.err)
	default:
		exit.Reason = fmt.Sprintf("setup `%s` failed with exit code %d on %s", s.spec.Setup.Run, res.code, s.spec.Node)
		if res.detail != "" {
			exit.Reason += ": " + res.detail
		}
	}
	return exit
}

func (s *supervisor) mainExit(res stepResult) proto.Exit {
	exit := proto.Exit{JobID: s.spec.JobID, Node: s.spec.Node, Code: res.code, Signal: res.signal}
	switch r := s.reason(); {
	case r == killLost:
		exit.State, exit.Code = proto.JobLost, proto.ExitInfra
		exit.Reason = s.lostReason()
	case res.oom && s.spec.MemMax > 0:
		exit.State = proto.JobFailed
		if exit.Code == 0 || exit.Code == proto.ExitInfra {
			exit.Code = proto.ExitSignalBase + int(syscall.SIGKILL)
		}
		exit.Signal = "KILL"
		exit.Reason = fmt.Sprintf("job hit its %s memory cap on %s — rerun with --mem %s", fmtGiB(s.spec.MemMax), s.spec.Node, suggestMem(s.spec.MemMax))
	case r == killCanceled:
		exit.State = proto.JobCanceled
		exit.Reason = "canceled"
	case res.err != nil:
		exit.State, exit.BeforeStart = proto.JobFailed, true
		switch res.code {
		case proto.ExitNotFound:
			exit.Reason = fmt.Sprintf("command not found on %s: %s", s.spec.Node, s.commandName())
		case proto.ExitNotExec:
			exit.Reason = fmt.Sprintf("cannot execute %s on %s: %v", s.commandName(), s.spec.Node, res.err)
		default:
			exit.State = proto.JobInfraErr
			exit.Reason = fmt.Sprintf("kran could not start the job on %s: %v", s.spec.Node, res.err)
		}
	case res.notStarted:
		exit.State, exit.BeforeStart = proto.JobFailed, true
		if res.code == 0 || res.code == proto.ExitInfra {
			exit.State = proto.JobInfraErr
			exit.Code = proto.ExitInfra
		}
		exit.Reason = fmt.Sprintf("container did not start on %s: %s", s.spec.Node, res.detail)
	case res.code == 0:
		exit.State = proto.JobSucceeded
	default:
		exit.State = proto.JobFailed
	}
	return exit
}

func (s *supervisor) lostReason() string {
	return fmt.Sprintf("lost contact with the origin; %s stopped the job after %s without a heartbeat", s.spec.Node, s.spec.LeaseTTL.Round(time.Second))
}

func (s *supervisor) commandName() string {
	if len(s.spec.Argv) == 0 {
		return ""
	}
	return s.spec.Argv[0]
}

// docker runs a docker CLI command for the job (kill, inspect, rm).
func (s *supervisor) docker(args ...string) ([]byte, error) {
	d := s.spec.Docker
	if d == nil {
		return nil, errors.New("not a container job")
	}
	cmd := exec.Command(d.CLI, args...)
	cmd.Env = d.Env
	return cmd.Output()
}

// dockerFinish inspects the finished container (exit code, OOM, whether
// it started at all) and removes it and any containers the job started.
func (s *supervisor) dockerFinish(name string, res *stepResult) {
	out, err := s.docker("inspect", "--format", "{{json .State}}", name)
	if err == nil {
		var st struct {
			ExitCode  int    `json:"ExitCode"`
			OOMKilled bool   `json:"OOMKilled"`
			Error     string `json:"Error"`
			StartedAt string `json:"StartedAt"`
		}
		if json.Unmarshal(out, &st) == nil {
			res.code = st.ExitCode
			res.oom = res.oom || st.OOMKilled
			if strings.HasPrefix(st.StartedAt, "0001-") {
				res.notStarted = true
				res.detail = st.Error
			} else if st.Error != "" {
				res.detail = st.Error
			}
			if res.code > proto.ExitSignalBase && res.code < proto.ExitSignalBase+32 {
				res.signal = sigName(syscall.Signal(res.code - proto.ExitSignalBase))
			} else {
				res.signal = ""
			}
		}
	}
	s.docker("rm", "--force", "--volumes", name)
	if ids, err := s.docker("ps", "--all", "--quiet", "--filter", "label=kran.job="+s.spec.JobID); err == nil {
		if f := strings.Fields(string(ids)); len(f) > 0 {
			s.docker(append([]string{"rm", "--force", "--volumes"}, f...)...)
		}
	}
}

// ---------------------------------------------------------------------------
// Control socket: stdin, resize, signals from the manager.

func (s *supervisor) serveCtl() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleCtl(c)
	}
}

func (s *supervisor) handleCtl(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 64<<10)
	for {
		t, p, err := proto.ReadFrame(c, buf)
		if err != nil {
			return
		}
		switch t {
		case proto.FrameStdin:
			s.writeStdin(p)
		case proto.FrameStdinEOF:
			s.closeStdin()
		case proto.FrameResize:
			var tty proto.TTY
			if json.Unmarshal(p, &tty) == nil {
				s.resize(tty)
			}
		case proto.FrameSignal:
			var sig proto.Signal
			if json.Unmarshal(p, &sig) == nil {
				s.signal(sig.Name)
			}
		}
	}
}

func (s *supervisor) writeStdin(p []byte) {
	s.mu.Lock()
	w, ptmx, eof := s.stdinW, s.ptmx, s.stdinEOF
	s.mu.Unlock()
	switch {
	case eof:
	case ptmx != nil:
		ptmx.Write(p)
	case w != nil:
		w.Write(p) // blocks while the job does not read: backpressure
	}
}

func (s *supervisor) closeStdin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdinEOF {
		return
	}
	s.stdinEOF = true
	switch {
	case s.ptmx != nil:
		s.ptmx.Write([]byte{4}) // ^D: EOF for a canonical-mode terminal
	case s.stdinW != nil:
		s.stdinW.Close()
		s.stdinW = nil
	}
}

func (s *supervisor) resize(t proto.TTY) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmx != nil && t.Rows > 0 && t.Cols > 0 {
		pty.Setsize(s.ptmx, &pty.Winsize{Rows: t.Rows, Cols: t.Cols})
	}
}

func (s *supervisor) signal(name string) {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG")) {
	case "KILL":
		s.terminate(killCanceled, 0)
		return
	case "TERM":
		s.terminate(killCanceled, s.spec.KillGrace)
		return
	}
	sig, ok := signalByName(name)
	if !ok {
		return
	}
	s.mu.Lock()
	cmd, pgid, ptmx, container := s.cmd, s.pgid, s.ptmx, s.container
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	switch {
	case container != "":
		go s.docker("kill", "--signal", strings.TrimPrefix(sigName(sig), "SIG"), container)
	case ptmx != nil:
		// The terminal's foreground process group is what Ctrl-C reaches
		// locally (a shell may have put a job in its own group).
		if fg, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP); err == nil && fg > 0 {
			syscall.Kill(-fg, sig)
			return
		}
		syscall.Kill(-pgid, sig)
	default:
		syscall.Kill(-pgid, sig)
	}
}

var signalNames = map[string]syscall.Signal{
	"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "HUP": syscall.SIGHUP,
	"QUIT": syscall.SIGQUIT, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "WINCH": syscall.SIGWINCH,
	"CONT": syscall.SIGCONT, "STOP": syscall.SIGSTOP, "TSTP": syscall.SIGTSTP,
}

func signalByName(name string) (syscall.Signal, bool) {
	s, ok := signalNames[strings.TrimPrefix(strings.ToUpper(name), "SIG")]
	return s, ok
}

func sigName(sig syscall.Signal) string {
	for n, s := range signalNames {
		if s == sig {
			return n
		}
	}
	return fmt.Sprintf("%d", int(sig))
}

func tvSeconds(tv syscall.Timeval) float64 { return float64(tv.Sec) + float64(tv.Usec)/1e6 }

// lookPathEnv resolves a command like a shell would with the job's PATH.
func lookPathEnv(file, path, dir string) (string, error) {
	if strings.Contains(file, "/") {
		p := file
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return p, checkExecutable(p)
	}
	var lastErr error = exec.ErrNotFound
	for _, d := range filepath.SplitList(path) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(d, file)
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		err := checkExecutable(p)
		if err == nil {
			return p, nil
		}
		if errors.Is(err, os.ErrPermission) {
			lastErr = err
		}
	}
	return "", fmt.Errorf("%s: %w", file, lastErr)
}

func checkExecutable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s: %w", p, os.ErrPermission)
	}
	return nil
}

func envGet(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

var _ io.Writer = streamWriter{}

// privateDir makes dir (and its parents) and insists that dir itself is a
// real directory of ours that nobody else can enter: the control socket may
// fall back to a shared /tmp, where another account could pre-create it.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a directory owned by this user; remove it", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
