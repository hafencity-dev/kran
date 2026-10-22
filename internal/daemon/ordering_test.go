package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

// The origin decides "started or not" (fall back vs. exit 125) from the
// running phase. Output of the command must therefore never overtake that
// phase on the job stream; otherwise a cut right after the first output makes
// the origin run the command a second time locally.
func TestRunningPhasePrecedesOutput(t *testing.T) {
	_, a, _ := twoNodes(t)
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := testContext(60 * time.Second)
	defer cancel()
	for i := 0; i < 15; i++ {
		running := false
		res, err := runCLI(ctx, a.sock(), shSpec(ws, "echo out"), cliOpts{
			hook: func(rs client.RunSession, ft proto.FrameType, p []byte, r *cliResult) error {
				switch ft {
				case proto.FramePhase:
					var ph proto.Phase
					json.Unmarshal(p, &ph)
					if ph.State == proto.JobRunning {
						running = true
					}
				case proto.FrameStdout:
					if !running {
						t.Errorf("run %d: output arrived before the running phase", i)
					}
				}
				return nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		if res.exit == nil || res.exit.Code != 0 || res.out() != "out\n" {
			t.Fatalf("run %d: exit %+v out %q", i, res.exit, res.out())
		}
		if t.Failed() {
			return
		}
	}
}
