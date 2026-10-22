package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/transport"
)

// A second daemon on the same dirs fails before it touches anything (no
// restore, no peer dialing, no log lines) when started by hand.
func TestInstanceLockRefusesSecondDaemon(t *testing.T) {
	dir := shortDir(t)
	release, err := acquireInstance(context.Background(), dir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = acquireInstance(context.Background(), dir, false, nil)
	if !errors.Is(err, transport.ErrAlreadyRunning) || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("second instance: %v", err)
	}
	release()
	release2, err := acquireInstance(context.Background(), dir, false, nil)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	release2()
}

// Under a service manager the second daemon waits for the first to stop
// instead of exiting, so launchd/systemd do not respawn it every few
// seconds; it says so once.
func TestInstanceLockStandbyUnderSupervisor(t *testing.T) {
	dir := shortDir(t)
	release, err := acquireInstance(context.Background(), dir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	got := make(chan func(), 1)
	go func() {
		r, err := acquireInstance(context.Background(), dir, true, func(f string, a ...any) { notes = append(notes, f) })
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("standby daemon did not wait")
	case <-time.After(700 * time.Millisecond):
	}
	release()
	select {
	case r := <-got:
		if r != nil {
			r()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("standby daemon did not take over")
	}
	if len(notes) != 1 {
		t.Fatalf("notes %v", notes)
	}
	// Canceled while waiting: it gives up.
	release, _ = acquireInstance(context.Background(), dir, false, nil)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := acquireInstance(ctx, dir, true, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled standby: %v", err)
	}
}

// launchd and the on-demand start send the daemon's stdout/stderr to a
// file that only crash output and fatal errors should reach; it is kept
// small, keeping its tail.
func TestCapCrashLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.out.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := strings.Repeat("old line\n", 200000) // ~1.8 MB
	f.WriteString(old + "last words before the crash\n")
	capCrashLog(f, path, 1<<20, 4<<10)
	b, _ := os.ReadFile(path)
	if len(b) > 8<<10 || !strings.HasSuffix(string(b), "last words before the crash\n") {
		t.Fatalf("after cap: %d bytes, tail %q", len(b), b[max(0, len(b)-60):])
	}
	f.WriteString("next\n") // O_APPEND keeps working
	b, _ = os.ReadFile(path)
	if !strings.HasSuffix(string(b), "crash\nnext\n") {
		t.Fatalf("append after cap: %q", b[max(0, len(b)-60):])
	}
	// Small files and non-files are left alone.
	capCrashLog(f, path, 1<<20, 4<<10)
	capCrashLog(os.Stdin, path, 1, 1) // not that file
	if b2, _ := os.ReadFile(path); len(b2) != len(b) {
		t.Fatal("capped a file it was not writing to")
	}
}
