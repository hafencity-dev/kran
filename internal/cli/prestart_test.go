package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/version"
)

// quietDaemon serves the local API with one run session: after the submit
// it sends nothing but its heartbeats for quiet (a long pre-start phase such
// as a first container image build), then runs the job remotely.
func quietDaemon(t *testing.T, quiet time.Duration) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(version.Info())
	})
	mux.HandleFunc("POST /api/v1/run", func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: kran-run/1\r\n\r\n")
		brw.Flush()
		if _, _, err := proto.ReadFrame(brw.Reader, nil); err != nil { // submit
			return
		}
		go func() { // the CLI's frames
			br := bufio.NewReader(brw.Reader)
			for {
				if _, _, err := proto.ReadFrame(br, nil); err != nil {
					conn.Close()
					return
				}
			}
		}()
		write := func(ft proto.FrameType, v any) error {
			b, raw := v.([]byte)
			if !raw && v != nil {
				b, _ = json.Marshal(v)
			}
			return proto.WriteFrame(conn, ft, b)
		}
		write(proto.FrameDecision, proto.Decision{JobID: "jq", Mode: "remote", Node: "devmac"})
		write(proto.FramePhase, proto.Phase{State: proto.JobSetup, Message: "preparing the Linux runtime on devmac (first use, ~30 s)"})
		for end := time.Now().Add(quiet); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			if write(proto.FrameHeartbeat, nil) != nil {
				return
			}
		}
		write(proto.FramePhase, proto.Phase{State: proto.JobRunning})
		write(proto.FrameStdout, []byte("done\n"))
		write(proto.FrameExit, proto.Exit{JobID: "jq", Code: 0, State: proto.JobSucceeded, Node: "devmac"})
		time.Sleep(200 * time.Millisecond)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// A pre-start phase longer than DeadAfter, with only heartbeats from the
// daemon, on a busy machine: the daemon is alive and the job must run there.
func TestLongQuietPrestartIsNotADeadDaemon(t *testing.T) {
	sock := quietDaemon(t, 3*time.Second) // DeadAfter is 1 s in these tests
	var stop atomic.Bool
	defer stop.Store(true)
	for i := 0; i < max(2, runtime.NumCPU()/4); i++ { // a busy box
		go func() {
			for x := 0; !stop.Load(); x++ {
				_ = x * x
			}
		}()
	}
	h := newHarness(t, nil, "x")
	h.r.spec.NoFallback = true // --remote: a false "dead daemon" is exit 125
	h.r.dial = func(ctx context.Context) (client.Client, error) { return client.DialSocket(ctx, sock) }
	if code := h.run(t); code != 0 {
		t.Fatalf("code %d: %s", code, h.stderr.String())
	}
	if h.stdout.String() != "done\n" || strings.Contains(h.stderr.String(), "lost the connection") {
		t.Fatalf("stdout %q stderr %q", h.stdout.String(), h.stderr.String())
	}
}
