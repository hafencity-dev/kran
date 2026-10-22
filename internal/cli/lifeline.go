package cli

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Without a daemon nobody else would stop a local job whose kran run was
// killed (an agent's timeout, the OOM killer): it runs in its own process
// group, so even a group kill aimed at kran run misses it. A small watcher
// (`kran internal watch-parent <pgid>`, in its own session) holds the read
// end of a pipe only kran run can write: a byte means "the job ended, go
// away"; end of file means kran run died, and the watcher stops the job's
// group. The pipe closes on any death of kran run, SIGKILL included, and
// unlike watching a pid it cannot be fooled by pid reuse.

// lifeline is kran run's end of the pipe.
type lifeline struct {
	w      *os.File
	cmd    *exec.Cmd
	exited chan struct{} // closed once the watcher was reaped
}

// defaultWatcher runs this kran binary as the watcher.
func defaultWatcher(pgid int) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	return exec.Command(self, "internal", "watch-parent", strconv.Itoa(pgid))
}

// startLifeline starts the watcher for pgid. Failing to start it never
// fails the job: it only loses the cleanup after a SIGKILL.
func startLifeline(pgid int, mk func(int) *exec.Cmd) *lifeline {
	if mk == nil || pgid <= 1 {
		return nil
	}
	cmd := mk(pgid)
	if cmd == nil {
		return nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil
	}
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	r.Close()
	if err != nil {
		w.Close()
		return nil
	}
	return &lifeline{w: w, cmd: cmd, exited: make(chan struct{})}
}

// release tells the watcher the job ended normally.
func (l *lifeline) release() {
	if l == nil {
		return
	}
	l.w.Write([]byte{'x'})
	l.w.Close()
	go func() {
		l.cmd.Wait()
		close(l.exited)
	}()
}

// watchParent is `kran internal watch-parent <pgid>`; in is the lifeline.
func watchParent(in io.Reader, pgid int, grace time.Duration) int {
	// It runs in its own session without a terminal; nothing but the
	// lifeline should end it.
	signal.Ignore(syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGPIPE)
	var b [1]byte
	for {
		n, err := in.Read(b[:])
		if n > 0 {
			return 0 // the job ended normally
		}
		if err != nil {
			break // kran run is gone
		}
	}
	if pgid <= 1 || syscall.Kill(-pgid, syscall.SIGTERM) != nil {
		return 0
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		if syscall.Kill(-pgid, 0) != nil {
			return 0
		}
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
	return 0
}
