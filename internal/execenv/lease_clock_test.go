package execenv

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// The lease must measure time since the last renewal, not the wall-clock age
// of the lease file: a clock stepped forward (NTP after a wake-up) made
// every renewed job look expired and killed it, and a clock stepped back
// kept an abandoned job alive until the clock caught up.
func TestLeaseSurvivesWallClockStep(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	p := te.start(ref, sh("sleep 4; echo done"))
	lease := filepath.Join(te.m.opts.StateDir, "jobs", p.JobID(), fileLease)
	if _, err := os.Stat(lease); err != nil {
		t.Fatalf("lease file: %v", err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// Renewals as seen through a clock one hour off.
		for i := 0; ; i++ {
			at := time.Now().Add(-time.Hour)
			os.Chtimes(lease, at, at)
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
	}()
	exit := waitExit(t, p, 20*time.Second)
	if exit.State != proto.JobSucceeded {
		t.Fatalf("a renewed job was killed: %+v", exit)
	}
}
