package client

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestStoppedSessionHelper is the child of TestStoppedProcessKeepsSession:
// a run session that reads until the exit frame.
func TestStoppedSessionHelper(t *testing.T) {
	sock := os.Getenv("KRAN_TEST_STALL_SOCK")
	if sock == "" {
		t.Skip("helper process only")
	}
	RecvTimeout = 300 * time.Millisecond
	conn, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	sr := &stallReader{conn: conn}
	s := newRunSession(conn, bufio.NewReader(sr))
	s.sr = sr
	for {
		ft, _, err := s.Recv()
		if err != nil {
			fmt.Println(err)
			os.Exit(3)
		}
		if ft == proto.FrameExit {
			os.Exit(0)
		}
	}
}

// A kran run that was stopped (Ctrl-Z, a suspended laptop) for longer than
// RecvTimeout finds the daemon's heartbeats waiting when it continues: that
// is not a dead daemon (a closed session cancels the job).
func TestStoppedProcessKeepsSession(t *testing.T) {
	sock := filepath.Join(shortDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoppedSessionHelper$")
	cmd.Env = append(os.Environ(), "KRAN_TEST_STALL_SOCK="+sock)
	out := &lockedBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		for i := 0; i < 80; i++ { // 4 s of daemon heartbeats
			if proto.WriteFrame(conn, proto.FrameHeartbeat, nil) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		proto.WriteFrame(conn, proto.FrameExit, []byte(`{}`))
	}()
	// Whether the expired deadline or the waiting data wins on resume is up
	// to the runtime: stop several times.
	for i := 0; i < 4; i++ {
		time.Sleep(150 * time.Millisecond)
		cmd.Process.Signal(syscall.SIGSTOP)
		time.Sleep(600 * time.Millisecond)
		cmd.Process.Signal(syscall.SIGCONT)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("session ended after the stop: %v\n%s", err, out.String())
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A frame write that fails half way (the daemon stopped reading for longer
// than the write deadline) must end the session: another frame written
// after the torn one would be parsed out of step by the daemon.
func TestTornWriteEndsSession(t *testing.T) {
	sock := filepath.Join(shortDir(t), "t.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close() // reads only after the torn write
	s := newRunSession(conn, bufio.NewReader(conn))
	s.recvTimeout = 200 * time.Millisecond
	defer s.Close()
	big := make([]byte, proto.MaxFramePayload)
	if err := s.Send(proto.FrameStdin, big); err == nil {
		t.Skip("the socket buffer took a whole frame")
	}
	go io.Copy(io.Discard, peer) // the daemon reads again
	if err := s.Send(proto.FrameSignal, []byte(`{"name":"INT"}`)); err == nil {
		t.Fatal("a frame was written after a torn one")
	}
}
