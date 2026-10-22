package execenv

import (
	"bytes"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// A job that does not read its stdin while input keeps coming must still
// get Ctrl-C at once: signals may never queue behind stdin.
func TestSignalNotBehindUnreadStdin(t *testing.T) {
	te := newTestEnv(t)
	ref := te.slot(testRepo, nil, proto.RuntimeNative)
	p := te.start(ref, sh("echo up; sleep 30"), withStdin())
	waitOutput(t, p, "up", 10*time.Second)
	stop := make(chan struct{})
	defer close(stop)
	go func() { // the origin's heartbeats
		for {
			p.Renew()
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
	}()
	go func() {
		w := p.Stdin()
		w.Write(bytes.Repeat([]byte("x"), 8<<20)) // far beyond every pipe and socket buffer
	}()
	time.Sleep(500 * time.Millisecond) // the writer is blocked now
	sent := make(chan error, 1)
	go func() { sent <- p.Signal("INT") }()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Signal blocked behind unread stdin")
	}
	exit := waitExit(t, p, 5*time.Second)
	if exit.Signal != "INT" {
		t.Fatalf("exit %+v", exit)
	}
}

// waitOutput waits until the job printed want.
func waitOutput(t *testing.T, p Process, want string, d time.Duration) {
	t.Helper()
	r := p.Output(0)
	defer r.Close()
	got := make(chan bool, 1)
	go func() {
		var out bytes.Buffer
		buf := make([]byte, 64<<10)
		for {
			_, payload, err := proto.ReadFrame(r, buf)
			if err != nil {
				got <- false
				return
			}
			out.Write(payload)
			if bytes.Contains(out.Bytes(), []byte(want)) {
				got <- true
				return
			}
		}
	}()
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("output ended before %q", want)
		}
	case <-time.After(d):
		t.Fatalf("no %q within %s", want, d)
	}
}
