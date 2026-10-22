// Package selfexe locates the running kran binary for every place that
// starts kran again: the job watcher, daemon autostart, the supervisor and
// the sync helper.
//
// Inside `go test` the running binary is the test binary. It ignores kran's
// arguments and runs its whole suite again, and that suite starts more
// copies: on 2026-10-22 the recursion reached 1,826 test processes and 66 GB
// and took a machine down with a global OOM. Path therefore refuses a test
// binary unless the package's TestMain dispatches kran's arguments itself
// and says so with AllowTest.
package selfexe

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
)

var allowTest atomic.Bool

// AllowTest is called from a TestMain that handles kran's self-start
// arguments (`internal supervise`, `internal apply`, …) before m.Run.
func AllowTest() { allowTest.Store(true) }

// ErrTestBinary is returned by Path inside a test binary that did not
// call AllowTest.
var ErrTestBinary = errors.New("refusing to start a Go test binary as kran: its TestMain must handle kran's arguments and call selfexe.AllowTest")

// Path returns the absolute path of the running kran binary.
func Path() (string, error) {
	if testing.Testing() && !allowTest.Load() {
		return "", ErrTestBinary
	}
	return os.Executable()
}
