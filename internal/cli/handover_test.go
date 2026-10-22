package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/service"
	"github.com/hafencity-dev/kran/internal/version"
)

// fakeAPIDaemon is a daemon process stand-in serving /api/v1 on sock.
type fakeAPIDaemon struct {
	pid      int
	srv      *http.Server
	mu       sync.Mutex
	stopWhen string
}

func serveFakeDaemon(t *testing.T, sock string, pid int) *fakeAPIDaemon {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAPIDaemon{pid: pid}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(version.Info())
	})
	mux.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(proto.State{})
	})
	mux.HandleFunc("GET /api/v1/daemon", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(proto.DaemonInfo{PID: pid})
	})
	mux.HandleFunc("POST /api/v1/daemon/stop", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ When string }
		json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.stopWhen = b.When
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(proto.DaemonInfo{PID: pid})
		go func() {
			time.Sleep(200 * time.Millisecond)
			f.srv.Close() // removes the socket
		}()
	})
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

// fakeManager is a service manager whose Install starts the managed daemon.
type fakeManager struct {
	t       *testing.T
	sock    string
	mu      sync.Mutex
	calls   []string
	running *fakeAPIDaemon
}

func (m *fakeManager) Install(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "install")
	if c, err := net.DialTimeout("unix", m.sock, time.Second); err == nil {
		c.Close()
		m.calls = append(m.calls, "socket busy: another kran daemon is already running")
		return true, nil
	}
	m.running = serveFakeDaemon(m.t, m.sock, 777)
	return true, nil
}
func (m *fakeManager) Uninstall(context.Context) error { return nil }
func (m *fakeManager) Start(ctx context.Context) error { _, err := m.Install(ctx); return err }
func (m *fakeManager) Stop(context.Context) error      { return nil }
func (m *fakeManager) Restart(ctx context.Context, o service.RestartOptions) error {
	return m.Start(ctx)
}
func (m *fakeManager) Unit() string                          { return "" }
func (m *fakeManager) Refresh(context.Context) (bool, error) { return false, nil }
func (m *fakeManager) Status(context.Context) (service.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := service.Status{Manager: "launchd", Installed: true}
	if m.running != nil {
		st.Running, st.PID = true, m.running.pid
	}
	return st, nil
}

// An on-demand daemon (started by `kran serve --stdio`, not by the service
// manager) holds the socket: installing the service asks it to stop once
// idle, waits for the socket, then starts the managed daemon.
func TestServiceInstallTakesOverFromOnDemandDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "kh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("KRAN_RUNTIME_DIR", dir)
	t.Setenv("KRAN_STATE_DIR", dir)
	sock := filepath.Join(dir, "kran.sock")
	onDemand := serveFakeDaemon(t, sock, 4242)
	m := &fakeManager{t: t, sock: sock}
	var out, errb bytes.Buffer
	a := &App{In: os.Stdin, Out: &out, Err: &errb, Env: os.Environ,
		Dial:           func(ctx context.Context) (client.Client, error) { return client.DialSocket(ctx, sock) },
		ServiceManager: func(bool) (service.Manager, error) { return m, nil },
		Self:           func() (string, error) { return "/usr/local/bin/kran", nil }}
	if code := a.Execute(context.Background(), []string{"service", "start"}); code != 0 {
		t.Fatalf("code %d: %s %s", code, out.String(), errb.String())
	}
	onDemand.mu.Lock()
	when := onDemand.stopWhen
	onDemand.mu.Unlock()
	m.mu.Lock()
	calls := strings.Join(m.calls, ", ")
	m.mu.Unlock()
	if when != "idle" || calls != "install" {
		t.Fatalf("stop asked %q, manager calls: %s; stderr %s", when, calls, errb.String())
	}
	if !strings.Contains(errb.String(), "pid 4242") {
		t.Fatalf("no word about the handover: %q", errb.String())
	}
	_ = io.Discard
}

// After an upgrade without a service, the on-demand daemon (still the old
// binary) is asked to stop once idle; the next connection starts the new one.
func TestUpgradeStopsOnDemandDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "kh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "kran.sock")
	d := serveFakeDaemon(t, sock, 4243)
	a := &App{Dial: func(ctx context.Context) (client.Client, error) { return client.DialSocket(ctx, sock) }}
	if err := a.stopOnDemand(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopWhen != "idle" {
		t.Fatalf("stop %q", d.stopWhen)
	}
}
