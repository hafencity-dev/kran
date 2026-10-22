package cli

import (
	"os"
	"testing"
)

// A test binary must never start itself as the watcher: os.Executable is
// the test binary, which ignores `internal watch-parent <pgid>` and runs the
// whole suite again, whose standalone runs start watchers in turn. On
// 2026-10-22 that recursion reached 1,826 cli.test processes and 66 GB and
// took the shared build host down with a global OOM.
func TestDefaultWatcherNeverStartsATestBinary(t *testing.T) {
	if cmd := defaultWatcher(os.Getpid()); cmd != nil {
		t.Fatalf("defaultWatcher in a test binary = %v, want nil", cmd.Args)
	}
}

// A watcher never starts a watcher: the sentinel it carries bounds any
// self-start to one level, whatever binary it is.
func TestDefaultWatcherNeverStartsFromAWatcher(t *testing.T) {
	t.Setenv(watcherEnv, "1")
	if cmd := newWatcherCmd("/usr/local/bin/kran", 42); cmd != nil {
		t.Fatalf("a watcher started a watcher: %v", cmd.Args)
	}
}

// The real watcher carries the sentinel and the documented arguments.
func TestWatcherCommandCarriesTheSentinel(t *testing.T) {
	t.Setenv(watcherEnv, "")
	os.Unsetenv(watcherEnv)
	cmd := newWatcherCmd("/usr/local/bin/kran", 42)
	if cmd == nil {
		t.Fatal("no watcher command for a real kran binary")
	}
	if got := cmd.Args; len(got) != 4 || got[1] != "internal" || got[2] != "watch-parent" || got[3] != "42" {
		t.Fatalf("args = %v", got)
	}
	found := false
	for _, e := range cmd.Env {
		if e == watcherEnv+"=1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the watcher's environment lacks %s=1", watcherEnv)
	}
}
