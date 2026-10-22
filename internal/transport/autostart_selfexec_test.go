//go:build unix

package transport

import "testing"

// Autostart never spawns `<this binary> daemon` from a test binary: it would
// run the package's whole suite again instead of a daemon, and recurse.
func TestSpawnDaemonRefusesATestBinary(t *testing.T) {
	stop, err := spawnDaemon()
	if err == nil {
		if stop != nil {
			_ = stop()
		}
		t.Fatal("spawnDaemon started this test binary as a daemon")
	}
}
