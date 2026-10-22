package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Peer is what the dialer needs to reach a peer (from peers.toml).
type Peer struct {
	Name      string
	Addresses []string // host:port, tried in order (the remembered winner first)
	User      string
	HostKey   string // authorized_keys format, e.g. "ssh-ed25519 AAAA…"
}

// Dialer opens SSH sessions to peers with the kran key and wraps the forced
// command's stdio in a yamux session.
type Dialer struct {
	Signer  ssh.Signer
	Session SessionConfig
	// Command is requested as exec; the peer's forced command wins anyway.
	Command string // default "kran serve --stdio"
	// HandshakeTimeout bounds TCP connect + SSH handshake per address.
	HandshakeTimeout time.Duration // default 10 s
	// RaceDelay is how long an address gets before the next one starts
	// racing it (happy eyeballs).
	RaceDelay time.Duration // default 300 ms
	// NetDial is injectable for tests; default net.Dialer.DialContext.
	NetDial func(ctx context.Context, network, addr string) (net.Conn, error)

	mu      sync.Mutex
	winners map[string]string
}

// LoadSigner reads the kran private key (OpenSSH or PEM format).
func LoadSigner(keyFile string) (ssh.Signer, error) {
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read kran key: %w (create it: kran setup)", err)
	}
	s, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse kran key %s: %w", keyFile, err)
	}
	return s, nil
}

// NewDialer returns a dialer using the key at keyFile.
func NewDialer(keyFile string, cfg SessionConfig) (*Dialer, error) {
	s, err := LoadSigner(keyFile)
	if err != nil {
		return nil, err
	}
	return &Dialer{Signer: s, Session: cfg}, nil
}

// Preferred returns the address that won the last race for peer name.
func (d *Dialer) Preferred(name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.winners[name]
}

func (d *Dialer) remember(name, addr string) {
	d.mu.Lock()
	if d.winners == nil {
		d.winners = map[string]string{}
	}
	d.winners[name] = addr
	d.mu.Unlock()
}

func (d *Dialer) order(p Peer) []string {
	pref := d.Preferred(p.Name)
	out := make([]string, 0, len(p.Addresses))
	if pref != "" {
		for _, a := range p.Addresses {
			if a == pref {
				out = append(out, a)
			}
		}
	}
	for _, a := range p.Addresses {
		if a != pref {
			out = append(out, a)
		}
	}
	return out
}

// HostKeyCallback pins the host key given in authorized_keys format.
func HostKeyCallback(hostKey string) (ssh.HostKeyCallback, []string, error) {
	if strings.TrimSpace(hostKey) == "" {
		return nil, nil, fmt.Errorf("%w: no host key pinned (run kran link again)", ErrHostKey)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostKey))
	if err != nil {
		return nil, nil, fmt.Errorf("parse pinned host key: %w", err)
	}
	want := pub.Marshal()
	cb := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if !bytes.Equal(key.Marshal(), want) {
			return fmt.Errorf("%w: %s presented %s %s, expected the key pinned at link time",
				ErrHostKey, hostname, key.Type(), ssh.FingerprintSHA256(key))
		}
		return nil
	}
	algos := []string{pub.Type()}
	if pub.Type() == ssh.KeyAlgoRSA {
		algos = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return cb, algos, nil
}

// Dial races the peer's addresses, verifies the pinned host key, starts the
// kran command and returns a live session (a ping has been answered by the
// peer daemon).
func (d *Dialer) Dial(ctx context.Context, p Peer) (Session, error) {
	if d.Signer == nil {
		return nil, errors.New("dial: no kran key loaded")
	}
	if len(p.Addresses) == 0 {
		return nil, fmt.Errorf("dial %s: no addresses (run kran link %s again)", p.Name, p.Name)
	}
	hkcb, algos, err := HostKeyCallback(p.HostKey)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", p.Name, err)
	}
	cfg := &ssh.ClientConfig{
		User:              p.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(d.Signer)},
		HostKeyCallback:   hkcb,
		HostKeyAlgorithms: algos,
		Timeout:           d.handshakeTimeout(),
	}
	client, addr, err := d.race(ctx, p, cfg)
	if err != nil {
		return nil, err
	}
	d.remember(p.Name, addr)
	s, err := d.start(ctx, p, client, addr)
	if err != nil {
		client.Close()
		return nil, err
	}
	return s, nil
}

func (d *Dialer) handshakeTimeout() time.Duration {
	if d.HandshakeTimeout > 0 {
		return d.HandshakeTimeout
	}
	return 10 * time.Second
}

type raceResult struct {
	client *ssh.Client
	addr   string
	err    error
}

func (d *Dialer) race(ctx context.Context, p Peer, cfg *ssh.ClientConfig) (*ssh.Client, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	addrs := d.order(p)
	results := make(chan raceResult, len(addrs))
	delay := d.RaceDelay
	if delay <= 0 {
		delay = 300 * time.Millisecond
	}
	next := 0
	start := func() {
		addr := addrs[next]
		next++
		go func() {
			c, err := d.handshake(ctx, addr, cfg)
			results <- raceResult{c, addr, err}
		}()
	}
	start()
	pending := 1
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var errs []error
	for pending > 0 {
		select {
		case r := <-results:
			pending--
			if r.err == nil {
				// Losers that finish later are closed by the drain below.
				go func(n int) {
					for i := 0; i < n; i++ {
						if rr := <-results; rr.client != nil {
							rr.client.Close()
						}
					}
				}(pending)
				return r.client, r.addr, nil
			}
			errs = append(errs, fmt.Errorf("%s: %w", r.addr, r.err))
			if next < len(addrs) {
				start()
				pending++
				timer.Reset(delay)
			}
		case <-timer.C:
			if next < len(addrs) {
				start()
				pending++
				timer.Reset(delay)
			}
		case <-ctx.Done():
			go func(n int) {
				for i := 0; i < n; i++ {
					if rr := <-results; rr.client != nil {
						rr.client.Close()
					}
				}
			}(pending)
			return nil, "", fmt.Errorf("dial %s: %w", p.Name, ctx.Err())
		}
	}
	return nil, "", fmt.Errorf("dial %s: %w", p.Name, pickError(errs))
}

// pickError returns the most telling error: a wrong host key or refused key
// matters more than a timeout on an address that is not reachable from here.
func pickError(errs []error) error {
	for _, target := range []error{ErrHostKey, ErrAuth} {
		for _, e := range errs {
			if errors.Is(e, target) {
				return e
			}
		}
	}
	return errors.Join(errs...)
}

func (d *Dialer) handshake(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	dial := d.NetDial
	if dial == nil {
		var nd net.Dialer
		dial = nd.DialContext
	}
	hctx, cancel := context.WithTimeout(ctx, d.handshakeTimeout())
	defer cancel()
	conn, err := dial(hctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// The SSH handshake has no context; a deadline plus a watcher bound it
	// so a half-open address cannot hang the race.
	deadline, _ := hctx.Deadline()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(hctx, func() { conn.Close() })
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if !stop() {
		if err == nil {
			cc.Close()
		}
		conn.Close()
		if err == nil {
			err = hctx.Err()
		}
		return nil, classifySSH(err)
	}
	if err != nil {
		conn.Close()
		return nil, classifySSH(err)
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, chans, reqs), nil
}

func classifySSH(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrHostKey) {
		return err
	}
	if strings.Contains(err.Error(), "unable to authenticate") {
		return fmt.Errorf("%w: %v (run kran link again)", ErrAuth, err)
	}
	return err
}

// start runs the forced command and brings up yamux over its stdio.
func (d *Dialer) start(ctx context.Context, p Peer, client *ssh.Client, addr string) (Session, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("dial %s: open ssh session: %w", p.Name, err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stderr := &limitedBuffer{max: 4096}
	sess.Stderr = stderr
	cmd := d.Command
	if cmd == "" {
		cmd = "kran serve --stdio"
	}
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		return nil, fmt.Errorf("dial %s: start kran: %w", p.Name, err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- sess.Wait() }()

	conn := &stdioConn{r: stdout, w: stdin, closeFn: func() error {
		sess.Close()
		return client.Close()
	}}
	ys, err := Client(conn, d.Session, addr)
	if err != nil {
		return nil, err
	}
	s := ys.(*session)
	if err := handshakePing(s, d.handshakeTimeout()); err != nil {
		s.Close()
		// The bridge prints why it gave up and exits; give it a moment.
		var exitErr error
		select {
		case exitErr = <-waitCh:
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
		return nil, bridgeError(p.Name, exitErr, stderr.String(), err)
	}
	return s, nil
}

func bridgeError(node string, exitErr error, stderr string, pingErr error) error {
	var ee *ssh.ExitError
	if errors.As(exitErr, &ee) {
		switch ee.ExitStatus() {
		case ExitDaemonDown:
			return fmt.Errorf("kran daemon on %s is not running (start it there: kran service start): %w", node, ErrDaemonDown)
		case ExitSocketDenied:
			return fmt.Errorf("kran on %s cannot open its daemon socket: %s", node, strings.TrimSpace(stderr))
		case 127:
			return fmt.Errorf("kran is not installed on %s at the linked path (run kran link %s again)", node, node)
		}
		if msg := strings.TrimSpace(stderr); msg != "" {
			return fmt.Errorf("kran on %s exited with %d: %s", node, ee.ExitStatus(), msg)
		}
	}
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("kran on %s did not answer: %s", node, msg)
	}
	return fmt.Errorf("kran on %s did not answer: %w", node, pingErr)
}

// stdioConn adapts the command's stdin/stdout to an io.ReadWriteCloser.
type stdioConn struct {
	r       io.Reader
	w       io.WriteCloser
	once    sync.Once
	closeFn func() error
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// Close tears down the channel and the connection. It deliberately skips
// stdin's CloseWrite: x/crypto does not synchronize it with a concurrent
// Write, and closing the channel ends both directions anyway.
func (c *stdioConn) Close() error {
	var err error
	c.once.Do(func() { err = c.closeFn() })
	return err
}

type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
