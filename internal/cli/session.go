package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// timing holds the intervals of the run session; tests shorten them.
type timing struct {
	NoticeDelay time.Duration // a wait must exceed this before kran says anything
	Heartbeat   time.Duration // we send a heartbeat this often
	DeadAfter   time.Duration // no frame for this long = connection lost
	Reconnect   time.Duration // how long to retry the daemon after a drop (a restarted daemon waits as long, CLIReattachWait)
	DoubleInt   time.Duration // second Ctrl-C within this escalates to KILL
	RetryEvery  time.Duration
	ExitConfirm time.Duration // how long a local job's exit waits for the daemon to record it
}

func defaultTiming() timing {
	return timing{NoticeDelay: 2 * time.Second, Heartbeat: 2 * time.Second, DeadAfter: 10 * time.Second,
		Reconnect: 20 * time.Second, DoubleInt: 2 * time.Second, RetryEvery: 300 * time.Millisecond,
		ExitConfirm: 3 * time.Second}
}

// runner drives one `kran run` (SPEC §6.1).
type runner struct {
	spec    proto.JobSpec
	flags   RunFlags
	env     []string          // full local environment, for local execution
	envSet  map[string]string // .kran.toml env.set, applied on top of env
	dial    func(ctx context.Context) (client.Client, error)
	stdin   *os.File
	stdout  io.Writer
	stderr  *lineWriter
	stdio   localStdio
	signals <-chan os.Signal
	winch   <-chan os.Signal
	size    func() *proto.TTY
	tm      timing
	start   func(localJob) (*localProc, *localError)
	// watcher builds `kran internal watch-parent <pgid>` (nil: the real one).
	watcher func(pgid int) *exec.Cmd
	probe   func(int64) bool
	color   style
	// unknownNode explains a --node pin naming no linked machine, from
	// peers.toml (used when no daemon answers); nil skips the check.
	unknownNode func(node string) string

	// session state
	cl       client.Client
	sess     client.RunSession
	gen      int
	frames   chan frameMsg
	jobID    string
	queued   bool // jobID is known from a queued notice; no decision yet
	decision *proto.Decision
	remote   bool // a remote placement is active
	started  bool // the user's command has started (remote running phase, or local start)
	offset   int64
	lastRecv time.Time
	local    *localProc
	localCh  <-chan proto.Exit
	// localNode is the daemon's node name for a local placement.
	localNode string
	// pendingExit: the local job ended while reconnecting to the daemon.
	pendingExit *proto.Exit
	// sentExit: the local job's exit went to the daemon, which has not
	// confirmed it yet (its exit frame back); exitConfirm bounds the wait.
	sentExit    *proto.Exit
	exitConfirm <-chan time.Time
	tee         *historyTee // local output → daemon (job history)
	fallback    bool
	lastInt     time.Time

	// notices
	pending   string
	lastSaid  string
	noteTimer *time.Timer
	noteShown bool
	noteC     <-chan time.Time

	stdinCh     chan []byte
	in          inputState
	reconnectCh chan reconnectResult
	reconnectOn bool
	quit        chan struct{}
}

type frameMsg struct {
	gen     int
	t       proto.FrameType
	payload []byte
	err     error
}

type reconnectResult struct {
	cl   client.Client
	sess client.RunSession
	err  error
}

// ---------------------------------------------------------------------------
// output helpers

func (r *runner) say(format string, a ...any) {
	if r.flags.Quiet {
		return
	}
	r.sayAlways(format, a...)
}

func (r *runner) sayAlways(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if r.flags.JSONEvents {
		r.event("notice", map[string]any{"message": msg})
		return
	}
	r.stderr.line("kran: " + msg)
}

// event writes a machine-readable progress line: {"kran":"<name>",…}.
func (r *runner) event(name string, fields map[string]any) {
	if !r.flags.JSONEvents {
		return
	}
	head := fmt.Sprintf(`{"kran":%q`, name)
	if len(fields) == 0 {
		r.stderr.line(head + "}")
		return
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(head)
	for _, k := range keys {
		v, err := json.Marshal(fields[k])
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, ",%q:%s", k, v)
	}
	b.WriteString("}")
	r.stderr.line(b.String())
}

// pendNote holds a waiting/progress message until it has lasted NoticeDelay,
// then prints it; changes afterwards print immediately.
func (r *runner) pendNote(text string) {
	if text == "" || sameNote(text, r.lastSaid) || r.flags.JSONEvents {
		return // JSON mode already gets every notice and phase as an event
	}
	if r.noteShown {
		r.printNote(text)
		return
	}
	r.pending = text
	if r.noteTimer == nil {
		r.noteTimer = time.NewTimer(r.tm.NoticeDelay)
		r.noteC = r.noteTimer.C
	}
}

// sameNote compares two waiting messages ignoring numbers: "has 6.8 GiB free"
// becoming "has 6.6 GiB free" is the same reason, not news worth a new line.
func sameNote(a, b string) bool {
	return digitsRe.ReplaceAllString(a, "#") == digitsRe.ReplaceAllString(b, "#")
}

var digitsRe = regexp.MustCompile(`[0-9]+(\.[0-9]+)?`)

func (r *runner) printNote(text string) {
	r.lastSaid = text
	r.say("%s", text)
	r.noteShown = true
}

func (r *runner) clearNote() {
	if r.noteTimer != nil {
		r.noteTimer.Stop()
		r.noteTimer = nil
		r.noteC = nil
	}
	r.pending = ""
	r.noteShown = false
}

// ---------------------------------------------------------------------------
// entry points

// Run executes the job and returns kran run's exit code.
func (r *runner) Run(ctx context.Context) int {
	r.quit = make(chan struct{})
	defer close(r.quit)
	defer func() {
		if r.tee != nil {
			r.tee.stop()
		}
	}()
	r.frames = make(chan frameMsg, 64)
	r.reconnectCh = make(chan reconnectResult, 1)

	cl, err := r.dial(ctx)
	if err != nil {
		return r.noSession(ctx, "daemon not running — running locally (start it: kran service start)", err)
	}
	defer func() {
		if r.cl != nil {
			_ = r.cl.Close()
		}
	}()
	r.cl = cl
	vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	v, verr := cl.Version(vctx)
	cancel()
	if verr != nil {
		return r.noSession(ctx, "daemon not responding — running locally (restart it: kran service restart --force)", verr)
	}
	if v.API != proto.APIVersion {
		return r.noSession(ctx, fmt.Sprintf("daemon %s speaks API %s, this kran speaks %s — running locally (update: kran upgrade)", v.Version, v.API, proto.APIVersion), nil)
	}
	usage, notice := pinnedNode(ctx, cl, r.spec.Req.Node)
	if usage != "" {
		r.sayAlways("%s", usage)
		r.event("exit", map[string]any{"code": proto.ExitUsage, "state": proto.JobFailed})
		return proto.ExitUsage
	}
	if notice != "" {
		r.say("%s", notice)
	}
	sess, err := cl.Run(ctx)
	if err != nil {
		return r.noSession(ctx, "could not open a run session — running locally", err)
	}
	r.attach(sess)
	if err := sess.SendJSON(proto.FrameSubmit, r.spec); err != nil {
		r.closeSession()
		return r.noSession(ctx, "could not submit the job — running locally", err)
	}
	return r.loop(ctx)
}

// noSession is the path for every failure before a run session exists: run
// locally unless the user forbade it.
func (r *runner) noSession(ctx context.Context, why string, cause error) int {
	if r.unknownNode != nil && r.jobID == "" {
		// No daemon to ask: peers.toml says whether the pin is a typo.
		if msg := r.unknownNode(r.spec.Req.Node); msg != "" && r.spec.Req.Node != "" {
			r.sayAlways("%s", msg)
			return proto.ExitUsage
		}
	}
	if r.spec.NoFallback {
		msg := strings.SplitN(why, " — ", 2)[0]
		if cause != nil && !errors.Is(cause, client.ErrNoDaemon) {
			msg += " (" + cause.Error() + ")"
		}
		flag := "--no-fallback"
		if r.spec.Req.Prefer == proto.PreferRemote {
			flag = "--remote"
		}
		hint := "check: kran doctor"
		if i := strings.LastIndex(why, "("); i >= 0 && strings.HasSuffix(why, ")") {
			hint = why[i+1 : len(why)-1] // the "start it: …" advice of the original message
		}
		r.sayAlways("%s; %s forbids running here (%s)", msg, flag, hint)
		r.event("exit", map[string]any{"code": proto.ExitInfra, "state": proto.JobInfraErr})
		return proto.ExitInfra
	}
	r.event("fallback", map[string]any{"reason": why})
	if !r.flags.JSONEvents { // the fallback event already carries the reason
		r.say("%s", why)
	}
	return r.standalone(ctx)
}

// localEnvelope is the memory guard used without a daemon decision: cap the
// job at 75% of physical memory so a runaway build is killed instead of
// freezing the machine.
func defaultEnvelope() *proto.LocalEnvelope {
	env := &proto.LocalEnvelope{Slice: "kran.slice"}
	if m := totalMemory(); m > 0 {
		env.MemMax = m / 4 * 3
	}
	return env
}

func (r *runner) localJob(env *proto.LocalEnvelope) localJob {
	j := localJob{JobID: r.jobID, Node: r.localNode, Argv: r.spec.Argv, Dir: r.spec.Cwd, Env: withEnvSet(r.env, r.envSet), Envelope: env,
		Stdio: r.stdio, ScopeProbe: r.probe}
	if r.tee != nil {
		j.Tee = r.tee.push
	}
	return j
}

// standalone runs the command locally with no daemon involvement.
func (r *runner) standalone(ctx context.Context) int {
	job := r.localJob(defaultEnvelope())
	// No daemon will kill the job if kran run dies: it must die with us.
	job.Watcher = r.watcher
	if job.Watcher == nil {
		job.Watcher = defaultWatcher
	}
	p, lerr := r.start(job)
	if lerr != nil {
		return r.launchFailed(lerr)
	}
	r.local, r.localCh = p, p.Done()
	for {
		select {
		case ex := <-r.localCh:
			if ex.Reason != "" {
				r.sayAlways("%s", ex.Reason)
			}
			r.event("exit", map[string]any{"code": ex.Code, "signal": ex.Signal, "state": ex.State, "fallback": true, "reason": ex.Reason})
			return ex.Code
		case sig := <-r.signals:
			r.forwardLocal(sig)
		case <-ctx.Done():
			_ = r.local.Signal("TERM")
			r.signals = nil
			r.localCh = r.local.Done()
		}
	}
}

func (r *runner) launchFailed(lerr *localError) int {
	r.sayAlways("%s", lerr.Msg)
	r.event("exit", map[string]any{"code": lerr.Code, "state": proto.JobFailed})
	return lerr.Code
}

// ---------------------------------------------------------------------------
// session loop

func (r *runner) attach(s client.RunSession) {
	r.sess = s
	r.gen++
	r.lastRecv = time.Now()
	gen := r.gen
	go func() {
		for {
			t, p, err := s.Recv()
			m := frameMsg{gen: gen, t: t, err: err}
			if err == nil {
				m.payload = append([]byte(nil), p...)
			}
			select {
			case r.frames <- m:
			case <-r.quit:
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

func (r *runner) closeSession() {
	if r.tee != nil {
		r.tee.setSession(nil)
	}
	if r.sess != nil {
		_ = r.sess.Close()
		r.sess = nil
	}
}

func (r *runner) send(t proto.FrameType, payload []byte) {
	if r.sess != nil {
		_ = r.sess.Send(t, payload)
	}
}

func (r *runner) sendJSON(t proto.FrameType, v any) {
	if r.sess != nil {
		_ = r.sess.SendJSON(t, v)
	}
}

func (r *runner) loop(ctx context.Context) int {
	hb := time.NewTicker(r.tm.Heartbeat)
	defer hb.Stop()
	defer r.closeSession()
	defer r.clearNote()
	lastTick := time.Now()
	for {
		select {
		case m := <-r.frames:
			if m.gen != r.gen {
				continue
			}
			if code, done := r.onFrame(ctx, m); done {
				return code
			}
		case <-hb.C:
			r.send(proto.FrameHeartbeat, nil)
			now := time.Now()
			if now.Sub(lastTick) > r.tm.DeadAfter/2 {
				// This loop itself stood still (blocked writing output to a
				// slow reader such as a pager, or the process was stopped):
				// that silence is ours, not the daemon's. Frames may be
				// waiting; start counting again.
				r.lastRecv = now
			}
			lastTick = now
			if r.sess != nil && now.Sub(r.lastRecv) > r.tm.DeadAfter {
				r.closeSession()
				if code, done := r.onDisconnect(ctx, errors.New("no heartbeat from the daemon")); done {
					return code
				}
			}
		case sig := <-r.signals:
			if code, done := r.onSignal(sig); done {
				return code
			}
		case <-r.winch:
			if r.local != nil {
				r.local.resize()
			}
			if r.remote && r.size != nil {
				if t := r.size(); t != nil {
					r.sendJSON(proto.FrameResize, t)
				}
			}
		case b := <-r.stdinCh:
			r.onInput(b)
		case ex := <-r.localCh:
			if r.sess == nil && r.reconnectOn {
				// The daemon is restarting: report the exit once we are
				// back (or the reconnect gives up), so its record is right.
				r.localCh, r.pendingExit = nil, &ex
				continue
			}
			if r.sess == nil {
				return r.onLocalDone(ex)
			}
			// Wait for the daemon to record the exit: one that dies before
			// it reads the frame would otherwise lose the result.
			x := r.reportLocalExit(ex)
			r.sentExit, r.exitConfirm = &x, time.After(r.tm.ExitConfirm)
		case <-r.exitConfirm:
			return r.localResult(*r.sentExit)
		case <-r.noteC:
			r.noteTimer, r.noteC = nil, nil
			if r.pending != "" {
				r.printNote(r.pending)
				r.pending = ""
			}
		case rr := <-r.reconnectCh:
			if code, done := r.onReconnect(ctx, rr); done {
				return code
			}
		case <-ctx.Done():
			// Context cancelled (SIGTERM at the process level): treat as TERM.
			if code, done := r.onSignal(syscall.SIGTERM); done {
				return code
			}
		}
	}
}

func (r *runner) onFrame(ctx context.Context, m frameMsg) (int, bool) {
	if m.err != nil {
		return r.onDisconnect(ctx, m.err)
	}
	if r.sentExit != nil && (m.t == proto.FrameExit || m.t == proto.FrameError) {
		return r.localResult(*r.sentExit), true // recorded (or refused): the code is the command's
	}
	r.lastRecv = time.Now()
	switch m.t {
	case proto.FrameHeartbeat:
	// Output alone does not mean the command started: a remote job's setup
	// prints too. Only the running phase (sent before the command's first
	// byte) or a local start does.
	case proto.FrameStdout:
		r.offset += int64(len(m.payload))
		_, _ = r.stdout.Write(m.payload)
	case proto.FrameStderr:
		r.offset += int64(len(m.payload))
		_, _ = r.stderr.Write(m.payload)
	case proto.FrameNotice:
		var n proto.Notice
		if json.Unmarshal(m.payload, &n) == nil {
			r.onNotice(n)
		}
	case proto.FramePhase:
		var p proto.Phase
		if json.Unmarshal(m.payload, &p) == nil {
			r.onPhase(p)
		}
	case proto.FrameStarted:
		r.started = true
		r.clearNote()
	case proto.FrameDecision:
		var d proto.Decision
		if err := json.Unmarshal(m.payload, &d); err != nil {
			return r.fail("the daemon sent an unreadable decision (" + err.Error() + ")")
		}
		return r.onDecision(ctx, d)
	case proto.FrameExit:
		var ex proto.Exit
		if err := json.Unmarshal(m.payload, &ex); err != nil {
			return r.fail("the daemon sent an unreadable exit status")
		}
		return r.onExit(ex), true
	case proto.FrameError:
		var e proto.APIError
		_ = json.Unmarshal(m.payload, &e)
		return r.onErrorFrame(ctx, e)
	case proto.FrameStdinCredit:
		if c, err := decodeCredit(m.payload); err == nil {
			r.onCredit(c)
		}
	}
	return 0, false
}

func (r *runner) onNotice(n proto.Notice) {
	r.event("notice", map[string]any{"kind": n.Kind, "message": n.Message, "job_id": n.JobID})
	switch n.Kind {
	case "queued":
		if r.jobID == "" && n.JobID != "" {
			// A daemon restart keeps the queued job and waits for this
			// kran run to reattach (SPEC §6.1): it needs the ID for that.
			r.jobID, r.queued = n.JobID, true
		}
		r.pendNote(n.Message)
	case "placed":
		// The decision frame announces remote placement.
	default:
		r.clearNote()
		r.lastSaid = n.Message
		if !r.flags.JSONEvents {
			r.say("%s", n.Message)
		}
	}
}

func (r *runner) onPhase(p proto.Phase) {
	r.event("phase", map[string]any{"state": p.State, "message": p.Message, "progress": p.Progress})
	switch p.State {
	case proto.JobSyncing, proto.JobSetup:
		text := string(p.State)
		if p.Message != "" {
			text += " — " + p.Message
		}
		r.pendNote(text)
	case proto.JobRunning:
		r.started = true
		r.clearNote()
		r.pumpInput() // input is read once the command runs
	}
}

func (r *runner) onDecision(ctx context.Context, d proto.Decision) (int, bool) {
	r.clearNote()
	r.jobID, r.queued = d.JobID, false
	r.event("decision", map[string]any{"job_id": d.JobID, "mode": d.Mode, "node": d.Node, "runtime": d.Runtime,
		"reason": d.Reason, "estimate": d.Estimate})
	if d.Mode == "remote" {
		r.remote = true
		r.decision = &d
		line := "running on " + d.Node
		if d.Runtime == proto.RuntimeContainer {
			line += " (container)"
		}
		if d.Reason != "" {
			line += " — " + d.Reason
		}
		if !r.flags.JSONEvents {
			r.say("%s", line)
		}
		r.startInput(d)
		return 0, false
	}
	// Local placement (or a fallback after a failed remote attempt).
	if r.remote {
		r.fallback = true
	}
	r.remote = false
	r.localNode = d.Node
	if r.tee == nil && r.sess != nil {
		// The output also goes to the daemon, so kran logs and the UI
		// show local jobs like remote ones (SPEC §8.2).
		r.tee = newHistoryTee(teeQueueMax) // queues until started is sent
	}
	p, lerr := r.start(r.localJob(d.Envelope))
	if lerr != nil {
		r.sendJSON(proto.FrameExit, proto.Exit{JobID: d.JobID, Code: lerr.Code, State: proto.JobFailed, Reason: lerr.Msg})
		return r.launchFailed(lerr), true
	}
	r.local, r.localCh, r.started = p, p.Done(), true
	r.sendJSON(proto.FrameStarted, p.Started)
	if r.tee != nil {
		r.tee.setSession(r.sess) // the history takes output only after started
	}
	r.event("started", map[string]any{"job_id": d.JobID, "pid": p.Started.PID, "scope": p.Started.Scope})
	return 0, false
}

func (r *runner) onLocalDone(ex proto.Exit) int {
	return r.localResult(r.reportLocalExit(ex))
}

// reportLocalExit sends a local job's exit (after its output) to the daemon.
func (r *runner) reportLocalExit(ex proto.Exit) proto.Exit {
	ex.JobID = r.jobID
	ex.Fallback = r.fallback
	r.local, r.localCh = nil, nil
	if r.tee != nil {
		// The history gets the output before the exit closes it; a daemon
		// that cannot keep up delays the exit by at most teeFlushWait.
		r.tee.flush(teeFlushWait)
		if r.tee.stop() {
			r.send(proto.FrameStdout, []byte("\n"+historyTruncated+"\n"))
		}
		r.tee = nil
	}
	r.sendJSON(proto.FrameExit, ex)
	return ex
}

// localResult ends kran run with a local job's own result.
func (r *runner) localResult(ex proto.Exit) int {
	if ex.Reason != "" {
		r.sayAlways("%s", ex.Reason)
	}
	r.event("exit", map[string]any{"code": ex.Code, "signal": ex.Signal, "state": ex.State, "fallback": r.fallback, "reason": ex.Reason})
	return ex.Code
}

func (r *runner) onExit(ex proto.Exit) int {
	r.event("exit", map[string]any{"code": ex.Code, "signal": ex.Signal, "state": ex.State, "reason": ex.Reason,
		"node": ex.Node, "usage": ex.Usage, "fallback": ex.Fallback, "disk_short": ex.DiskShort})
	code := ex.Code
	if (ex.State == proto.JobInfraErr || ex.State == proto.JobLost) && code == 0 {
		code = proto.ExitInfra
	}
	if ex.Reason != "" {
		r.sayAlways("%s", ex.Reason)
	}
	return code
}

// onErrorFrame: before a remote decision, fall back to local. After one,
// the job may run on its target (SPEC §6.1): only an error saying nothing
// ran anywhere (RunLocally) lets kran run run it here; otherwise exit 125.
func (r *runner) onErrorFrame(ctx context.Context, e proto.APIError) (int, bool) {
	msg := e.Error
	if msg == "" {
		msg = "the daemon reported an error"
	}
	if e.Hint != "" {
		msg += " (" + e.Hint + ")"
	}
	if r.local == nil && !r.started && (!r.remote || e.RunLocally) {
		r.closeSession()
		return r.noSession(ctx, msg+" — running locally", nil), true
	}
	if r.remote && !r.started {
		msg += "; " + r.mayStillStart()
	}
	return r.fail(msg)
}

// mayStillStart says why kran run does not run a remote job here after
// losing track of it before it started.
func (r *runner) mayStillStart() string {
	node := "its node"
	if r.decision != nil {
		node = r.decision.Node
	}
	return fmt.Sprintf("job %s may still start on %s, so kran does not run it here too (check: kran jobs; kran logs %s)", r.jobID, node, r.jobID)
}

func (r *runner) fail(msg string) (int, bool) {
	r.sayAlways("%s", msg)
	r.event("exit", map[string]any{"code": proto.ExitInfra, "state": proto.JobInfraErr, "reason": msg})
	return proto.ExitInfra, true
}

// onDisconnect handles a dropped daemon connection. Before any decision
// nothing can have started, so the job runs here. After one, kran run
// reattaches: a remote job may start on its target while the daemon
// restarts or shuts down, and only the daemon can tell (SPEC §6.1).
func (r *runner) onDisconnect(ctx context.Context, cause error) (int, bool) {
	r.closeSession()
	switch {
	case r.jobID == "":
		return r.noSession(ctx, "lost the connection to the daemon before the job started — running locally", cause), true
	case r.reconnectOn:
		return 0, false
	}
	if r.sentExit != nil {
		// The daemon went away before confirming the exit: report it again
		// once it is back.
		r.pendingExit, r.sentExit, r.exitConfirm = r.sentExit, nil, nil
	}
	r.reconnectOn = true
	go r.reconnect()
	return 0, false
}

func (r *runner) reconnect() {
	deadline := time.Now().Add(r.tm.Reconnect)
	var last error
	for time.Now().Before(deadline) {
		select {
		case <-r.quit:
			return
		case <-time.After(r.tm.RetryEvery):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cl, err := r.dial(ctx)
		if err == nil {
			var s client.RunSession
			if s, err = cl.Run(ctx); err == nil {
				cancel()
				r.reconnectCh <- reconnectResult{cl: cl, sess: s}
				return
			}
			_ = cl.Close()
		}
		cancel()
		last = err
	}
	r.reconnectCh <- reconnectResult{err: last}
}

func (r *runner) onReconnect(ctx context.Context, rr reconnectResult) (int, bool) {
	r.reconnectOn = false
	if rr.err != nil || rr.sess == nil {
		if r.pendingExit != nil {
			return r.onLocalDone(*r.pendingExit), true
		}
		if r.local != nil {
			return 0, false // keep running locally; the exit code is still ours
		}
		if r.queued {
			// Never placed: a daemon that comes back cancels the job unless
			// this kran run reattaches, so running it here runs it once.
			return r.noSession(ctx, "lost the connection to the daemon while the job was queued — running locally", rr.err), true
		}
		if r.remote && !r.started {
			return r.fail("lost the connection to the kran daemon; " + r.mayStillStart())
		}
		node := "its node"
		if r.decision != nil {
			node = r.decision.Node
		}
		return r.fail(fmt.Sprintf("lost the connection to the kran daemon; job %s may still be running on %s (check: kran jobs)", r.jobID, node))
	}
	if r.cl != nil {
		_ = r.cl.Close()
	}
	r.cl = rr.cl
	r.attach(rr.sess)
	r.sendJSON(proto.FrameReattach, proto.Reattach{JobID: r.jobID, Offset: r.offset})
	if r.local != nil {
		// A restarted daemon lost the child's process group: without it, it
		// could not kill the child if this kran run dies later.
		r.sendJSON(proto.FrameStarted, r.local.Started)
	}
	if r.tee != nil {
		r.tee.setSession(r.sess) // output only after reattach and started
	}
	if r.pendingExit != nil {
		return r.onLocalDone(*r.pendingExit), true
	}
	return 0, false
}

// onSignal forwards a signal; a second Ctrl-C within DoubleInt escalates.
func (r *runner) onSignal(sig os.Signal) (int, bool) {
	name := sigName(sig)
	if name == "" {
		return 0, false
	}
	if ex := r.sentExit; ex != nil || r.pendingExit != nil {
		// The command has ended; stop waiting to report its result.
		if ex == nil {
			ex = r.pendingExit
		}
		return r.localResult(*ex), true
	}
	if r.local != nil {
		r.forwardLocal(sig)
		return 0, false
	}
	if r.remote && r.jobID != "" {
		if name == "INT" {
			if !r.lastInt.IsZero() && time.Since(r.lastInt) < r.tm.DoubleInt {
				name = "KILL"
			}
			r.lastInt = time.Now()
		}
		r.sendJSON(proto.FrameSignal, proto.Signal{Name: name})
		return 0, false
	}
	// Still waiting in the queue: cancel by leaving.
	r.sendJSON(proto.FrameSignal, proto.Signal{Name: name})
	r.event("exit", map[string]any{"code": 128 + sigNum(name), "state": proto.JobCanceled})
	return 128 + sigNum(name), true
}

func (r *runner) forwardLocal(sig os.Signal) {
	name := sigName(sig)
	if name == "" || r.local == nil {
		return
	}
	if name == "INT" {
		if !r.lastInt.IsZero() && time.Since(r.lastInt) < r.tm.DoubleInt {
			name = "KILL"
		}
		r.lastInt = time.Now()
	}
	_ = r.local.Signal(name)
}

func sigName(s os.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGQUIT:
		return "QUIT"
	}
	return ""
}

func sigNum(name string) int { return int(signalNames[name]) }

// withEnvSet applies .kran.toml env.set to a local environment, as the
// target does for remote runs.
func withEnvSet(environ []string, set map[string]string) []string {
	if len(set) == 0 {
		return environ
	}
	out := make([]string, 0, len(environ)+len(set))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; !ok {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out
}
