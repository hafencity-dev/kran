package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// shortDir returns a temp dir with a short path (unix socket paths are
// limited to ~104 bytes, and macOS temp dirs are long).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "kt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func fastKeepalive() SessionConfig {
	return SessionConfig{Keepalive: KeepaliveConfig{Interval: 40 * time.Millisecond, Timeout: 40 * time.Millisecond, Misses: 3}}
}

func echoServer(s Session) {
	for {
		c, err := s.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			io.Copy(c, c)
		}()
	}
}

func roundTrip(t *testing.T, s Session, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := s.Open(ctx)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(line) != msg {
		t.Fatalf("got %q want %q", line, msg)
	}
}

func waitDone(t *testing.T, s Session, within time.Duration) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(within):
		t.Fatalf("session still alive after %s", within)
	}
}

func TestPipeRoundTrip(t *testing.T) {
	c, s, link, err := Pipe(SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Cut()
	go echoServer(s)
	roundTrip(t, c, "hello")
	// Streams work in both directions.
	go echoServer(c)
	roundTrip(t, s, "back")
	c.Close()
	waitDone(t, c, time.Second)
	waitDone(t, s, time.Second)
	if c.Err() == nil {
		t.Fatal("Err() nil after close")
	}
}

func TestKeepaliveDetectsHalfOpen(t *testing.T) {
	c, s, link, err := Pipe(fastKeepalive())
	if err != nil {
		t.Fatal(err)
	}
	defer link.Cut()
	go echoServer(s)
	roundTrip(t, c, "x")
	link.Stall(true)
	waitDone(t, c, 2*time.Second)
	if !errors.Is(c.Err(), ErrKeepalive) {
		t.Fatalf("err = %v, want ErrKeepalive", c.Err())
	}
}

func TestKeepaliveSurvivesShortStall(t *testing.T) {
	cfg := SessionConfig{Keepalive: KeepaliveConfig{Interval: 100 * time.Millisecond, Timeout: 100 * time.Millisecond, Misses: 3}}
	c, s, link, err := Pipe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Cut()
	go echoServer(s)
	link.Stall(true)
	time.Sleep(150 * time.Millisecond)
	link.Stall(false)
	roundTrip(t, c, "still here")
	select {
	case <-c.Done():
		t.Fatalf("session died after a short stall: %v", c.Err())
	case <-time.After(300 * time.Millisecond):
	}
	if c.RTT() <= 0 {
		t.Fatal("no RTT measured")
	}
}

func TestCutEndsBothSides(t *testing.T) {
	c, s, link, err := Pipe(SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	link.Cut()
	waitDone(t, c, time.Second)
	waitDone(t, s, time.Second)
	if _, err := c.Open(context.Background()); err == nil {
		t.Fatal("open on dead session succeeded")
	}
}

func TestOpenRespectsContext(t *testing.T) {
	c, _, link, err := Pipe(SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Cut()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Open(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestMemNetwork(t *testing.T) {
	n := NewMemNetwork(fastKeepalive())
	var mu sync.Mutex
	var servers []Session
	n.Listen("b", func(c net.Conn) {
		s, err := Server(c, fastKeepalive(), "mem")
		if err != nil {
			return
		}
		mu.Lock()
		servers = append(servers, s)
		mu.Unlock()
		echoServer(s)
	})
	ctx := context.Background()
	if _, err := n.Dial(ctx, "a", "c"); !errors.Is(err, ErrDaemonDown) {
		t.Fatalf("dial to unknown node: %v", err)
	}
	s1, err := n.Dial(ctx, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s1, "one")

	n.Cut("b")
	waitDone(t, s1, time.Second)
	s2, err := n.Dial(ctx, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s2, "two")

	n.SetDown("a", true)
	if _, err := n.Dial(ctx, "a", "b"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("dial while down: %v", err)
	}
	waitDone(t, s2, 2*time.Second) // stalled link dies by keepalive
	n.SetDown("a", false)
	s3, err := n.Dial(ctx, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s3, "three")
	s3.Close()
	mu.Lock()
	for _, s := range servers {
		s.Close()
	}
	mu.Unlock()
}

// peerSocketServer serves echo sessions on a unix socket, like a daemon's
// peer listener.
func peerSocketServer(t *testing.T, path string) net.Listener {
	t.Helper()
	l, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				s, err := Server(c, SessionConfig{}, "unix")
				if err != nil {
					return
				}
				echoServer(s)
			}()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return l
}

type sshFixture struct {
	srv    *SSHServer
	signer ssh.Signer
	sock   string
}

func newSSHFixture(t *testing.T, withDaemon bool) *sshFixture {
	t.Helper()
	dir := shortDir(t)
	sock := filepath.Join(dir, "peer.sock")
	if withDaemon {
		peerSocketServer(t, sock)
	}
	signer, err := GenerateSigner()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewSSHServer(SSHServerConfig{AuthorizedKeys: []ssh.PublicKey{signer.PublicKey()}, Socket: sock, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return &sshFixture{srv: srv, signer: signer, sock: sock}
}

func (f *sshFixture) peer() Peer {
	return Peer{Name: "node", Addresses: []string{f.srv.Addr()}, User: "kran", HostKey: f.srv.HostKey()}
}

func TestSSHDial(t *testing.T) {
	f := newSSHFixture(t, true)
	d := &Dialer{Signer: f.signer}
	s, err := d.Dial(context.Background(), f.peer())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	roundTrip(t, s, "over ssh")
	if s.RTT() <= 0 {
		t.Fatal("no RTT")
	}
	if d.Preferred("node") != f.srv.Addr() {
		t.Fatalf("winner not remembered: %q", d.Preferred("node"))
	}
	// The server dropping the connection ends the session.
	f.srv.DropConnections()
	waitDone(t, s, 3*time.Second)
}

func TestSSHHostKeyMismatch(t *testing.T) {
	f := newSSHFixture(t, true)
	other, _ := GenerateSigner()
	p := f.peer()
	p.HostKey = string(ssh.MarshalAuthorizedKey(other.PublicKey()))
	d := &Dialer{Signer: f.signer}
	_, err := d.Dial(context.Background(), p)
	if !errors.Is(err, ErrHostKey) {
		t.Fatalf("err = %v, want ErrHostKey", err)
	}
	p.HostKey = ""
	if _, err := d.Dial(context.Background(), p); !errors.Is(err, ErrHostKey) {
		t.Fatalf("empty host key: err = %v", err)
	}
}

func TestSSHKeyRefused(t *testing.T) {
	f := newSSHFixture(t, true)
	other, _ := GenerateSigner()
	d := &Dialer{Signer: other}
	_, err := d.Dial(context.Background(), f.peer())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestSSHDaemonDown(t *testing.T) {
	f := newSSHFixture(t, false)
	d := &Dialer{Signer: f.signer}
	_, err := d.Dial(context.Background(), f.peer())
	if !errors.Is(err, ErrDaemonDown) {
		t.Fatalf("err = %v, want ErrDaemonDown", err)
	}
	if !strings.Contains(err.Error(), "kran daemon on node is not running") {
		t.Fatalf("message: %v", err)
	}
}

func TestSSHAddressRace(t *testing.T) {
	f := newSSHFixture(t, true)
	const blackhole = "10.255.255.1:22"
	var mu sync.Mutex
	var tried []string
	d := &Dialer{Signer: f.signer, RaceDelay: 50 * time.Millisecond, HandshakeTimeout: 3 * time.Second,
		NetDial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			tried = append(tried, addr)
			mu.Unlock()
			if addr == blackhole {
				<-ctx.Done() // never answers, like a LAN address away from home
				return nil, ctx.Err()
			}
			var nd net.Dialer
			return nd.DialContext(ctx, network, addr)
		}}
	p := f.peer()
	p.Addresses = []string{blackhole, "127.0.0.1:1", f.srv.Addr()}
	start := time.Now()
	s, err := d.Dial(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if time.Since(start) > 2*time.Second {
		t.Fatalf("race took %s; the blackhole address held it up", time.Since(start))
	}
	if d.Preferred("node") != f.srv.Addr() {
		t.Fatalf("winner %q", d.Preferred("node"))
	}
	mu.Lock()
	tried = nil
	mu.Unlock()
	s, err = d.Dial(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(tried) == 0 || tried[0] != f.srv.Addr() {
		t.Fatalf("remembered winner not tried first: %v", tried)
	}
}

func TestSSHAllAddressesFail(t *testing.T) {
	signer, _ := GenerateSigner()
	hk, _ := GenerateSigner()
	d := &Dialer{Signer: signer, RaceDelay: 10 * time.Millisecond}
	p := Peer{Name: "x", Addresses: []string{"127.0.0.1:1", "127.0.0.1:2"}, User: "u",
		HostKey: string(ssh.MarshalAuthorizedKey(hk.PublicKey()))}
	_, err := d.Dial(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:2") {
		t.Fatalf("err = %v", err)
	}
}

func TestBridgeNoDaemon(t *testing.T) {
	dir := shortDir(t)
	var errOut bytes.Buffer
	code := Bridge(context.Background(), strings.NewReader(""), io.Discard, &errOut, filepath.Join(dir, "none.sock"))
	if code != ExitDaemonDown {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errOut.String(), "not running") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestBridgeEndsWhenDaemonCloses(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "p.sock")
	l, err := ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("bye"))
		c.Close()
	}()
	inR, inW := io.Pipe()
	defer inW.Close()
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Bridge(context.Background(), inR, &out, io.Discard, sock) }()
	select {
	case code := <-done:
		if code != 0 || out.String() != "bye" {
			t.Fatalf("code=%d out=%q", code, out.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit after the daemon closed")
	}
}

func TestBridgeEndsWhenClientCloses(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "p.sock")
	l, err := ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		b, _ := io.ReadAll(c)
		got <- string(b)
		c.Close()
	}()
	done := make(chan int, 1)
	go func() { done <- Bridge(context.Background(), strings.NewReader("hi"), io.Discard, io.Discard, sock) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not exit after stdin EOF")
	}
	if s := <-got; s != "hi" {
		t.Fatalf("daemon got %q", s)
	}
}

func TestListenUnix(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "sub", "d.sock")
	l, err := ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Dir(sock))
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dst.Mode().Perm())
	}
	if _, err := ListenUnix(sock); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second listen: %v", err)
	}
	// A crashed daemon leaves a stale socket file behind.
	l.SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}
	l2, err := ListenUnix(sock)
	if err != nil {
		t.Fatalf("listen over stale socket: %v", err)
	}
	l2.Close()
}
