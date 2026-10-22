package cli

import (
	"fmt"
	"os"
	"testing"
)

// TestMain refuses to run the suite when this test binary was started the
// way kran starts itself (`<self> internal ...`, `<self> daemon`): a test
// binary ignores those arguments and would run every test again, and the
// standalone runs among them would start more copies (2026-10-22: 1,826
// cli.test processes, a global OOM). The production paths refuse a test
// binary already (lifeline.go, transport/autostart_unix.go); this is the
// second line for any self-start added later.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "internal", "daemon":
			fmt.Fprintf(os.Stderr, "cli.test: started as kran (%q); refusing to run the suite again\n", os.Args[1:])
			os.Exit(70)
		}
	}
	os.Exit(m.Run())
}
