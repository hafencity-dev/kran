package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/version"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "kc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// fakeDaemon is a minimal stand-in for the daemon's local API.
type fakeDaemon struct {
	sock    string
	ln      net.Listener
	srv     *http.Server
	ver     proto.VersionInfo
	streams atomic.Int32
	// events served per connection: the first connection gets a gap after
	// two events to force a resync.
	mu       sync.Mutex
	logCalls int
}

func startFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := &fakeDaemon{sock: filepath.Join(shortDir(t), "k.sock"), ver: version.Info()}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(f.ver)
	})
	mux.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(proto.State{Self: "gx10"})
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(proto.APIError{Error: "no job " + r.PathValue("id"), Hint: "list jobs: kran jobs"})
	})
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		n := f.streams.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		send := func(ev proto.Event) {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b)
			fl.Flush()
		}
		base := uint64(n * 100)
		send(proto.Event{Type: proto.EventState, Seq: base, Data: proto.State{Self: "gx10"}})
		send(proto.Event{Type: proto.EventJob, Seq: base + 1, Data: proto.Job{Spec: proto.JobSpec{ID: "a"}}})
		if n == 1 {
			// Gap: the client must resync.
			send(proto.Event{Type: proto.EventJob, Seq: base + 5, Data: proto.Job{Spec: proto.JobSpec{ID: "gap"}}})
		} else {
			send(proto.Event{Type: proto.EventQueue, Seq: base + 2, Data: proto.QueueEvent{Queue: []string{"x"}}})
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("follow") == "" {
			w.Write([]byte("all output"))
			return
		}
		f.mu.Lock()
		f.logCalls++
		call := f.logCalls
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(off int64, s string) {
			b, _ := json.Marshal(proto.OutputChunk{Stream: "stdout", Offset: off + int64(len(s)), Data: s, B64: base64.StdEncoding.EncodeToString([]byte(s))})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if call == 1 {
			chunk(0, "hello ")
			w.(http.Flusher).Flush()
			// Connection drops; the client must resume at offset 6.
			panic(http.ErrAbortHandler)
		}
		if r.URL.Query().Get("offset") != "6" {
			http.Error(w, "bad offset "+r.URL.Query().Get("offset"), 400)
			return
		}
		chunk(3, "lo world") // overlapping resend is trimmed
		fmt.Fprintf(w, "event: end\ndata: {\"offset\":11}\n\n")
	})
	mux.HandleFunc("POST /api/v1/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != UpgradeProtocol {
			w.WriteHeader(400)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: kran-run/1\r\n\r\n")
		brw.Flush()
		// Echo control frames back, interleaved with heartbeats.
		proto.WriteFrame(conn, proto.FrameHeartbeat, nil)
		for {
			ty, p, err := proto.ReadFrame(brw.Reader, nil)
			if err != nil {
				return
			}
			if ty == proto.FrameHeartbeat {
				continue
			}
			if ty == proto.FrameSignal {
				// Go silent to exercise the receive timeout.
				time.Sleep(time.Second)
				return
			}
			proto.WriteFrame(conn, proto.FrameHeartbeat, nil)
			proto.WriteFrame(conn, ty, p)
		}
	})
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func TestDialNoDaemon(t *testing.T) {
	dir := shortDir(t)
	_, err := DialSocket(context.Background(), filepath.Join(dir, "none.sock"))
	if !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("err = %v", err)
	}
	// Stale socket file left by a crashed daemon.
	stale := filepath.Join(dir, "stale.sock")
	l, _ := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	l.SetUnlinkOnClose(false)
	l.Close()
	if _, err := DialSocket(context.Background(), stale); !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("stale: err = %v", err)
	}
}

func TestDialPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	f := startFake(t)
	os.Chmod(f.sock, 0)
	defer os.Chmod(f.sock, 0o600)
	_, err := DialSocket(context.Background(), f.sock)
	if !errors.Is(err, ErrPermission) {
		t.Fatalf("err = %v", err)
	}
}

func TestDialIncompatible(t *testing.T) {
	f := startFake(t)
	f.ver.Protocol = "9.0"
	_, err := DialSocket(context.Background(), f.sock)
	if !errors.Is(err, ErrIncompatible) || errors.Is(err, ErrNoDaemon) {
		t.Fatalf("err = %v", err)
	}
	f.ver = version.Info()
	f.ver.API = "v2"
	if _, err := DialSocket(context.Background(), f.sock); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("api: err = %v", err)
	}
}

func TestDialWedgedDaemon(t *testing.T) {
	sock := filepath.Join(shortDir(t), "w.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept but never answer
		}
	}()
	old := DialTimeout
	DialTimeout = 300 * time.Millisecond
	defer func() { DialTimeout = old }()
	start := time.Now()
	_, err = DialSocket(context.Background(), sock)
	if !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("dial was not bounded")
	}
}

func TestBasicCalls(t *testing.T) {
	f := startFake(t)
	c, err := DialSocket(context.Background(), f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st, err := c.State(context.Background())
	if err != nil || st.Self != "gx10" {
		t.Fatalf("state %v %v", st, err)
	}
	_, err = c.Job(context.Background(), "nope")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Hint == "" || !IsNotFound(err) {
		t.Fatalf("err = %#v", err)
	}
	if err := c.Cancel(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := c.Log(context.Background(), "x", false, &buf); err != nil || buf.String() != "all output" {
		t.Fatalf("log %q %v", buf.String(), err)
	}
}

func TestEventsResyncOnGap(t *testing.T) {
	f := startFake(t)
	c, err := DialSocket(context.Background(), f.sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for ev := range ch {
		types = append(types, string(ev.Type))
		if ev.Type == proto.EventJob {
			if j, ok := ev.Data.(proto.Job); !ok || j.Spec.ID == "gap" {
				t.Fatalf("bad job event %#v", ev.Data)
			}
		}
		if ev.Type == proto.EventQueue {
			if q, ok := ev.Data.(proto.QueueEvent); !ok || len(q.Queue) != 1 {
				t.Fatalf("bad queue event %#v", ev.Data)
			}
			cancel()
		}
	}
	want := "state,job,state,job,queue"
	if got := strings.Join(types, ","); got != want {
		t.Fatalf("events %s, want %s", got, want)
	}
}

func TestLogFollowResumes(t *testing.T) {
	f := startFake(t)
	c, err := DialSocket(context.Background(), f.sock)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Log(ctx, "j", true, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello world" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestRunSession(t *testing.T) {
	f := startFake(t)
	c, err := DialSocket(context.Background(), f.sock)
	if err != nil {
		t.Fatal(err)
	}
	old := RecvTimeout
	RecvTimeout = 400 * time.Millisecond
	defer func() { RecvTimeout = old }()
	rs, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if err := rs.SendJSON(proto.FrameSubmit, proto.JobSpec{ID: "abc"}); err != nil {
		t.Fatal(err)
	}
	// Heartbeats come through: they show the daemon is alive.
	ty, p, err := rs.Recv()
	for err == nil && ty == proto.FrameHeartbeat {
		ty, p, err = rs.Recv()
	}
	if err != nil || ty != proto.FrameSubmit {
		t.Fatalf("recv %v %v", ty, err)
	}
	var spec proto.JobSpec
	if json.Unmarshal(p, &spec); spec.ID != "abc" {
		t.Fatalf("payload %s", p)
	}
	rs.SendJSON(proto.FrameSignal, proto.Signal{Name: "INT"})
	for {
		ty, _, err := rs.Recv()
		if err == nil && ty == proto.FrameHeartbeat {
			continue
		}
		if !errors.Is(err, ErrSessionTimeout) {
			t.Fatalf("err = %v, want ErrSessionTimeout", err)
		}
		break
	}
}

func TestReadSSE(t *testing.T) {
	in := ": comment\nevent: a\ndata: x\ndata: y\nid: 3\n\ndata: z\n\n"
	var got []sseEvent
	err := readSSE(bufio.NewReader(strings.NewReader(in)), func(e sseEvent) error {
		got = append(got, e)
		return nil
	})
	if err == nil {
		t.Fatal("expected unexpected EOF at end")
	}
	if len(got) != 2 || got[0].Event != "a" || got[0].Data != "x\ny" || got[0].ID != "3" || got[1].Data != "z" {
		t.Fatalf("%+v", got)
	}
}
