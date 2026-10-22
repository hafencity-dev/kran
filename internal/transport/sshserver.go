package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"
)

// SSHServerConfig configures an in-process SSH server that behaves like a
// peer's sshd with kran's forced command: it accepts only the authorized
// keys and runs the bridge to Socket for every session, ignoring the
// requested command. Used by tests and the e2e harness (no system sshd).
type SSHServerConfig struct {
	HostKey        ssh.Signer      // nil = generate an ed25519 key
	AuthorizedKeys []ssh.PublicKey // keys allowed to log in
	// Socket is the peer socket the forced command bridges to.
	Socket string
	// Handle overrides the forced command (default: Bridge to Socket).
	Handle func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int
	Addr   string // default 127.0.0.1:0
	Logf   func(format string, args ...any)
}

// SSHServer is a running in-process SSH server.
type SSHServer struct {
	cfg     SSHServerConfig
	ln      net.Listener
	hostKey ssh.Signer
	ctx     context.Context
	cancel  context.CancelFunc

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// GenerateSigner returns a fresh ed25519 signer.
func GenerateSigner() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// NewSSHServer starts listening.
func NewSSHServer(cfg SSHServerConfig) (*SSHServer, error) {
	hk := cfg.HostKey
	if hk == nil {
		var err error
		if hk, err = GenerateSigner(); err != nil {
			return nil, err
		}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &SSHServer{cfg: cfg, ln: ln, hostKey: hk, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Addr is the host:port the server listens on.
func (s *SSHServer) Addr() string { return s.ln.Addr().String() }

// HostKey returns the server's public host key in authorized_keys format.
func (s *SSHServer) HostKey() string {
	return string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(s.hostKey.PublicKey())))
}

// Close stops the server and drops every connection.
func (s *SSHServer) Close() error {
	s.cancel()
	err := s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

// DropConnections closes all live connections but keeps listening
// (simulates a network cut that a reconnect can heal).
func (s *SSHServer) DropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.Close()
	}
}

func (s *SSHServer) serve() {
	defer s.wg.Done()
	sc := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, k := range s.cfg.AuthorizedKeys {
				if bytes.Equal(k.Marshal(), key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, errors.New("key not authorized")
		},
	}
	sc.AddHostKey(s.hostKey)
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				c.Close()
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
			s.handleConn(c, sc)
		}()
	}
}

func (s *SSHServer) handleConn(c net.Conn, sc *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(c, sc)
	if err != nil {
		s.cfg.Logf("sshserver: handshake: %v", err)
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	var wg sync.WaitGroup
	for nc := range chans {
		if nc.ChannelType() != "session" {
			// "restrict": no forwarding of any kind.
			nc.Reject(ssh.Prohibited, "only the kran command is allowed")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleSession(ch, creqs)
		}()
	}
	wg.Wait()
}

func (s *SSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	started := false
	done := make(chan int, 1)
	for {
		select {
		case req, ok := <-reqs:
			if !ok {
				if started {
					<-done
				}
				return
			}
			switch req.Type {
			case "exec", "shell":
				if started {
					req.Reply(false, nil)
					continue
				}
				started = true
				req.Reply(true, nil)
				go func() { done <- s.run(ch) }()
			default:
				// pty-req, env, x11, agent forwarding: refused like "restrict".
				req.Reply(false, nil)
			}
		case code := <-done:
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(code))
			ch.SendRequest("exit-status", false, b[:])
			return
		}
	}
}

func (s *SSHServer) run(ch ssh.Channel) int {
	stderr := ch.Stderr()
	if s.cfg.Handle != nil {
		return s.cfg.Handle(s.ctx, ch, ch, stderr)
	}
	if s.cfg.Socket == "" {
		fmt.Fprintln(stderr, "kran: no socket configured")
		return 1
	}
	// The bridge must not close the channel itself before the exit status
	// is sent, so hand it wrappers without Close.
	return Bridge(s.ctx, readerOnly{ch}, &channelWriter{ch: ch}, stderr, s.cfg.Socket)
}

type readerOnly struct{ r io.Reader }

func (r readerOnly) Read(p []byte) (int, error) { return r.r.Read(p) }

// channelWriter half-closes the channel (EOF to the client) on Close so the
// client's reader ends while the exit status can still be sent. x/crypto's
// CloseWrite must not run concurrently with Write, so both take mu; Close
// does not wait for a Write blocked on the client's window.
type channelWriter struct {
	ch     ssh.Channel
	mu     sync.Mutex
	once   sync.Once
	closed bool
}

func (w *channelWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	return w.ch.Write(p)
}

func (w *channelWriter) Close() error {
	w.once.Do(func() {
		go func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.closed = true
			w.ch.CloseWrite()
		}()
	})
	return nil
}
