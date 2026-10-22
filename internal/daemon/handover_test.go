package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func unixHTTP(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

// A daemon a service manager does not own (started on demand by
// `kran serve --stdio`) hands over to the managed one: it says who it is
// and stops when asked, once idle unless told "now".
func TestDaemonStopsWhenAskedOnceIdle(t *testing.T) {
	c := newCluster(t, "a")
	a := c.nodes["a"]
	a.start()
	hc := unixHTTP(a.sock())

	resp, err := hc.Get("http://kran/api/v1/daemon")
	if err != nil {
		t.Fatal(err)
	}
	var info proto.DaemonInfo
	json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.PID != os.Getpid() {
		t.Fatalf("info %+v", info)
	}

	// A job is active: "idle" waits for it.
	ws := workspaceDir(t, map[string]string{"f": "x"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := make(chan *cliResult, 1)
	go func() {
		r, _ := runCLI(ctx, a.sock(), shSpec(ws, "sleep 1; echo done"), cliOpts{})
		res <- r
	}()
	waitFor(t, 5*time.Second, "job active", func() bool { return len(a.daemon().jobs.list()) == 1 })
	resp, err = hc.Post("http://kran/api/v1/daemon/stop", "application/json", bytes.NewReader([]byte(`{"when":"idle"}`)))
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: %v %v", err, resp)
	}
	resp.Body.Close()
	select {
	case err := <-a.done:
		t.Fatalf("stopped with a job active (%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if r := <-res; r == nil || r.exit == nil || r.exit.Code != 0 || !strings.Contains(r.out(), "done") {
		t.Fatalf("job %+v", r)
	}
	select {
	case err := <-a.done:
		if err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		a.cancel = nil // already stopped
		a.mu.Unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop once idle")
	}
	if _, err := os.Stat(a.sock()); err == nil {
		t.Fatal("socket still there")
	}
}
