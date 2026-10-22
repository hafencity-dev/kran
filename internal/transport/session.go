package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// KeepaliveConfig controls liveness detection. Every byte from the peer
// counts as a sign of life, not only ping replies: a link busy with a blob
// upload, or one whose far daemon answers late because its machine is
// overloaded, is not dead. A ping goes out every Interval (also to measure
// the RTT); the session ends once nothing at all arrived for the current
// tolerance: DeadAfter on a calm link, up to MaxDeadAfter while the peer
// (or this node) is known to be under heavy load (SetPeerStressed) or the
// link is slow (20 × RTT). This still catches half-open TCP connections
// that the kernel would keep for many minutes.
type KeepaliveConfig struct {
	Interval time.Duration
	// DeadAfter is the tolerated silence on a calm link (zero:
	// Interval × Misses + Timeout, the meaning of the older fields).
	DeadAfter time.Duration
	// MaxDeadAfter bounds the tolerance under load (zero: 3 × DeadAfter).
	MaxDeadAfter time.Duration
	// Timeout and Misses only derive DeadAfter for older configs.
	Timeout time.Duration
	Misses  int
}

// DefaultKeepalive pings every 5 s; a calm peer silent for 45 s is dead, a
// loaded one gets up to 2 minutes. Job leases (15 s, SPEC §6.2) are
// shorter on purpose: a running job needs its heartbeats, while sync and
// setup in flight, and the link itself, survive a starved peer.
var DefaultKeepalive = KeepaliveConfig{Interval: 5 * time.Second, DeadAfter: 45 * time.Second, MaxDeadAfter: 2 * time.Minute}

// SessionConfig configures a yamux session.
type SessionConfig struct {
	Keepalive KeepaliveConfig // zero = DefaultKeepalive
	// WriteTimeout is yamux's safety valve for a single frame write and for
	// a stream open that is never acknowledged; yamux closes the whole
	// session when it fires, so it is never shorter than the tolerance.
	WriteTimeout time.Duration // zero = MaxDeadAfter + Interval
	Logf         func(format string, args ...any)
}

func (c SessionConfig) withDefaults() SessionConfig {
	ka := c.Keepalive
	if ka.Interval <= 0 {
		ka = DefaultKeepalive
	}
	if ka.DeadAfter <= 0 {
		misses := ka.Misses
		if misses <= 0 {
			misses = 3
		}
		timeout := ka.Timeout
		if timeout <= 0 {
			timeout = ka.Interval
		}
		ka.DeadAfter = time.Duration(misses)*ka.Interval + timeout
	}
	if ka.MaxDeadAfter < ka.DeadAfter {
		if ka.MaxDeadAfter <= 0 {
			ka.MaxDeadAfter = 3 * ka.DeadAfter
		} else {
			ka.MaxDeadAfter = ka.DeadAfter
		}
	}
	c.Keepalive = ka
	if min := ka.MaxDeadAfter + ka.Interval; c.WriteTimeout < min {
		c.WriteTimeout = min
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

// tolerance is how long the peer may stay silent before it counts as dead.
func tolerance(ka KeepaliveConfig, stressed bool, rtt time.Duration) time.Duration {
	tol := ka.DeadAfter
	if stressed {
		tol = ka.MaxDeadAfter
	}
	if slow := 20 * rtt; slow > tol {
		tol = slow
	}
	return min(tol, ka.MaxDeadAfter)
}

// liveness accumulates how long the peer has been silent, from periodic
// observations of the inbound byte counter. A late observation (this
// process was not scheduled: CPU starvation, swapped out, a laptop lid)
// counts as one interval at most, so a daemon that was frozen itself does
// not declare its peer dead the moment it wakes up with unread data.
type liveness struct {
	interval time.Duration
	last     time.Time
	rx       uint64
	silent   time.Duration
}

func newLiveness(interval time.Duration, now time.Time) *liveness {
	return &liveness{interval: interval, last: now}
}

func (l *liveness) observe(now time.Time, rx uint64) time.Duration {
	step := now.Sub(l.last)
	l.last = now
	if step > 2*l.interval || step < 0 {
		step = l.interval
	}
	if rx != l.rx {
		l.rx = rx
		l.silent = 0
		return 0
	}
	l.silent += step
	return l.silent
}

// countingConn counts inbound bytes (the liveness signal).
type countingConn struct {
	io.ReadWriteCloser
	rx *atomic.Uint64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	if n > 0 {
		c.rx.Add(uint64(n))
	}
	return n, err
}

// LoadHinter is implemented by sessions of this package: the daemon tells
// a session when the peer (or this node) is under heavy load, so slow
// answers are tolerated longer.
type LoadHinter interface {
	SetPeerStressed(bool)
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
	// An unacknowledged open makes yamux close the session; liveness is
	// our keepalive's call, so this only backs it up.
	yc.StreamOpenTimeout = c.WriteTimeout
	yc.LogOutput = io.Discard
	return yc
}

// Client starts the dialing side of a session over conn.
func Client(conn io.ReadWriteCloser, cfg SessionConfig, remoteAddr string) (Session, error) {
	return start(conn, cfg, remoteAddr, yamux.Client)
}

// Server starts the accepting side of a session over conn.
func Server(conn io.ReadWriteCloser, cfg SessionConfig, remoteAddr string) (Session, error) {
	return start(conn, cfg, remoteAddr, yamux.Server)
}

func start(conn io.ReadWriteCloser, cfg SessionConfig, remoteAddr string,
	mk func(io.ReadWriteCloser, *yamux.Config) (*yamux.Session, error)) (Session, error) {
	cfg = cfg.withDefaults()
	s := &session{conn: conn, cfg: cfg, addr: remoteAddr, done: make(chan struct{})}
	ys, err := mk(countingConn{conn, &s.rx}, yamuxConfig(cfg))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("start session: %w", err)
	}
	s.ys = ys
	go s.watch()
	go s.keepalive()
	return s, nil
}

type session struct {
	ys   *yamux.Session
	conn io.ReadWriteCloser
	cfg  SessionConfig
	addr string

	rtt      rttEstimate
	rx       atomic.Uint64 // inbound bytes
	stressed atomic.Bool   // peer or this node under heavy load
	pinging  atomic.Bool   // a keepalive ping is in flight
	done     chan struct{}

	mu      sync.Mutex
	err     error
	closers []io.Closer
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
	live := newLiveness(ka.Interval, time.Now())
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		silent := live.observe(time.Now(), s.rx.Load())
		// At most one ping is in flight: a stalled link must not pile up
		// goroutines. yamux gives up on it after WriteTimeout.
		if s.pinging.CompareAndSwap(false, true) {
			go func() {
				defer s.pinging.Store(false)
				if d, err := s.ys.Ping(); err == nil {
					s.rtt.sample(d)
				}
			}()
		}
		tol := tolerance(ka, s.stressed.Load(), s.rtt.value())
		if silent >= tol {
			s.cfg.Logf("transport: %s sent nothing for %s, closing session", s.addr, silent.Round(time.Second))
			s.fail(fmt.Errorf("%w: nothing received for %s", ErrKeepalive, silent.Round(time.Second)))
			return
		}
	}
}

// SetPeerStressed implements LoadHinter.
func (s *session) SetPeerStressed(on bool) { s.stressed.Store(on) }

// handshakePing proves the far end is a live kran daemon. The first ping
// waits for the far bridge to start (and, on a busy machine, to be
// scheduled), so a second, warm ping is what the link's RTT starts from.
func handshakePing(s *session, timeout time.Duration) error {
	if _, err := s.ping(timeout); err != nil {
		return err
	}
	d, err := s.ping(timeout)
	if err == nil {
		s.rtt.handshake(d)
	}
	return nil
}

// Ping measures the round-trip time once.
func (s *session) Ping(timeout time.Duration) (time.Duration, error) {
	d, err := s.ping(timeout)
	if err == nil {
		s.rtt.handshake(d)
	}
	return d, err
}

func (s *session) ping(timeout time.Duration) (time.Duration, error) {
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
