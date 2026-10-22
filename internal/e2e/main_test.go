package e2e

import (
	"os"
	"testing"

	"github.com/hafencity-dev/kran/internal/execenv"
)

// TestMain makes the test binary act as `kran internal supervise`, which
// execenv launches for every remote job of the in-process tests. Builds
// with the e2e, chaos or smoke tag also build the real kran binary once
// (beforeAll).
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "internal" && os.Args[2] == "supervise" {
		os.Exit(execenv.Supervise(os.Args[3:]))
	}
	cleanup := beforeAll()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
