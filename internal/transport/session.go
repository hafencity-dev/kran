package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// KeepaliveConfig controls liveness detection. A ping is sent every
// Interval; a ping not answered within Timeout is a miss; Misses consecutive
// misses end the session. This catches half-open TCP connections that the
// kernel would keep for many minutes.
type KeepaliveConfig struct {
	Interval time.Duration
	Timeout  time.Duration
	Misses   int
}

// DefaultKeepalive pings every 10 s and gives up after 3 misses.
var DefaultKeepalive = KeepaliveConfig{Interval: 10 * time.Second, Timeout: 10 * time.Second, Misses: 3}

// SessionConfig configures a yamux session.
type SessionConfig struct {
	Keepalive KeepaliveConfig // zero = DefaultKeepalive
	// WriteTimeout is yamux's safety valve for a single frame write; a
	// connection that accepts nothing for this long is considered dead.
	WriteTimeout time.Duration // zero = 30 s
	Logf         func(format string, args ...any)
}

func (c SessionConfig) withDefaults() SessionConfig {
	if c.Keepalive.Interval <= 0 {
		c.Keepalive = DefaultKeepalive
	}
	if c.Keepalive.Timeout <= 0 {
		c.Keepalive.Timeout = c.Keepalive.Interval
	}
	if c.Keepalive.Misses <= 0 {
		c.Keepalive.Misses = 3
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

func yamuxConfig(c SessionConfig) *yamux.Config {
	yc := yamux.DefaultConfig()
	yc.EnableKeepAlive = false // we run our own loop (miss counting, RTT)
	yc.KeepAliveInterval = c.Keepalive.Interval
	yc.ConnectionWriteTimeout = c.WriteTimeout
	// Larger windows keep blob uploads fast over links with real latency
	// (256 KiB at 20 ms RTT caps a stream at ~12 MB/s).
	yc.MaxStreamWindowSize = 4 << 20
	yc.StreamCloseTimeout = 30 * time.Second
	yc.StreamOpenTimeout = 30 * time.Second
	yc.LogOutput = io.Discard
	return yc
}

// Client starts the dialing side of a session over conn.
func Client(conn io.ReadWriteCloser, cfg SessionConfig, remoteAddr string) (Session, error) {
	cfg = cfg.withDefaults()
	ys, err := yamux.Client(conn, yamuxConfig(cfg))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start session: %w", err)
	}
	return newSession(ys, conn, cfg, remoteAddr), nil
}

// Server starts the accepting side of a session over conn.
func Server(conn io.ReadWriteCloser, cfg SessionConfig, remoteAddr string) (Session, error) {
	cfg = cfg.withDefaults()
	ys, err := yamux.Server(conn, yamuxConfig(cfg))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start session: %w", err)
	}
	return newSession(ys, conn, cfg, remoteAddr), nil
}

type session struct {
	ys   *yamux.Session
	conn io.ReadWriteCloser
	cfg  SessionConfig
	addr string

	rtt  rttEstimate
	done chan struct{}

	mu      sync.Mutex
	err     error
	closers []io.Closer
}

func newSession(ys *yamux.Session, conn io.ReadWriteCloser, cfg SessionConfig, addr string) *session {
	s := &session{ys: ys, conn: conn, cfg: cfg, addr: addr, done: make(chan struct{})}
	go s.watch()
	go s.keepalive()
	return s
}

// addCloser registers resources (ssh client, channels) released with the session.
func (s *session) addCloser(c io.Closer) {
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		c.Close()
		return
	default:
	}
	s.closers = append(s.closers, c)
	s.mu.Unlock()
}

func (s *session) watch() {
	<-s.ys.CloseChan()
	s.conn.Close()
	s.mu.Lock()
	if s.err == nil {
		s.err = errors.New("session closed")
	}
	closers := s.closers
	s.closers = nil
	s.mu.Unlock()
	for _, c := range closers {
		c.Close()
	}
	close(s.done)
}

func (s *session) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.ys.Close()
}

func (s *session) keepalive() {
	ka := s.cfg.Keepalive
	t := time.NewTicker(ka.Interval)
	defer t.Stop()
	misses := 0
	// At most one ping is in flight: a stalled link must not pile up
	// goroutines, and an unanswered ping simply counts as another miss.
	var inflight chan error
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		if inflight == nil {
			ch := make(chan error, 1)
			inflight = ch
			go func() {
				d, err := s.ys.Ping()
				if err == nil {
					s.rtt.sample(d)
				}
				ch <- err
			}()
		}
		timer := time.NewTimer(ka.Timeout)
		select {
		case err := <-inflight:
			inflight = nil
			if err == nil {
				misses = 0
			} else {
				misses++
			}
		case <-timer.C:
			misses++
		case <-s.done:
			timer.Stop()
			return
		}
		timer.Stop()
		if misses >= ka.Misses {
			s.cfg.Logf("transport: %s missed %d pings, closing session", s.addr, misses)
			s.fail(fmt.Errorf("%w: no answer to %d pings", ErrKeepalive, misses))
			return
		}
	}
}

// Ping measures the round-trip time once (also used right after dialing to
// prove the far end is a live kran daemon).
func (s *session) Ping(timeout time.Duration) (time.Duration, error) {
	ch := make(chan error, 1)
	var d time.Duration
	go func() {
		var err error
		d, err = s.ys.Ping()
		ch <- err
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-ch:
		if err != nil {
			return 0, err
		}
		s.rtt.handshake(d)
		return d, nil
	case <-t.C:
		return 0, fmt.Errorf("no answer within %s", timeout)
	case <-s.done:
		return 0, s.Err()
	}
}

func (s *session) Open(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := s.ys.Open()
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, s.wrapErr(r.err)
		}
		return r.c, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	case <-s.done:
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, s.Err()
	}
}

func (s *session) wrapErr(err error) error {
	if errors.Is(err, yamux.ErrSessionShutdown) {
		if e := s.Err(); e != nil {
			return e
		}
	}
	return err
}

func (s *session) Accept() (net.Conn, error) {
	c, err := s.ys.Accept()
	if err != nil {
		return nil, s.wrapErr(err)
	}
	return c, nil
}

func (s *session) Close() error {
	s.fail(errors.New("session closed"))
	return nil
}

func (s *session) Done() <-chan struct{} { return s.done }

func (s *session) Err() error {
	select {
	case <-s.done:
	default:
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *session) RTT() time.Duration { return s.rtt.value() }

// rttEstimate smooths the keepalive pings (EWMA). A one-off ping (the
// handshake right after dialing, often slowed by load or by the far daemon
// starting) only stands in until the first keepalive sample.
type rttEstimate struct {
	mu       sync.Mutex
	v        time.Duration
	smoothed bool // v comes from keepalive samples
}

func (r *rttEstimate) handshake(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.smoothed {
		r.v = d
	}
}

func (r *rttEstimate) sample(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.smoothed {
		r.v, r.smoothed = d, true
		return
	}
	r.v = (3*d + 7*r.v) / 10
}

func (r *rttEstimate) value() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.v
}
func (s *session) RemoteAddr() string { return s.addr }

// Pinger is implemented by sessions created in this package.
type Pinger interface {
	Ping(timeout time.Duration) (time.Duration, error)
}
