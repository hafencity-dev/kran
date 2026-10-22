package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// Link is an in-memory duplex connection whose middle can be stalled (bytes
// stop flowing but nothing closes: a half-open TCP connection) or cut (both
// ends see the connection die). Tests and the e2e harness use it to simulate
// network failures.
type Link struct {
	a, ra net.Conn // a is endpoint A; ra is the relay's side of A's pipe
	b, rb net.Conn

	mu      sync.Mutex
	cond    *sync.Cond
	stalled bool
	cut     bool
	wg      sync.WaitGroup
}

// NewLink returns a connected link.
func NewLink() *Link {
	l := &Link{}
	l.cond = sync.NewCond(&l.mu)
	l.a, l.ra = net.Pipe()
	l.rb, l.b = net.Pipe()
	l.wg.Add(2)
	go l.relay(l.ra, l.rb)
	go l.relay(l.rb, l.ra)
	return l
}

// Ends returns the two endpoints.
func (l *Link) Ends() (a, b net.Conn) { return l.a, l.b }

func (l *Link) relay(src, dst net.Conn) {
	defer l.wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			l.mu.Lock()
			for l.stalled && !l.cut {
				l.cond.Wait()
			}
			cut := l.cut
			l.mu.Unlock()
			if cut {
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				l.Cut()
				return
			}
		}
		if err != nil {
			l.Cut()
			return
		}
	}
}

// Stall stops (true) or resumes (false) delivery in both directions.
func (l *Link) Stall(on bool) {
	l.mu.Lock()
	l.stalled = on
	l.mu.Unlock()
	l.cond.Broadcast()
}

// Cut closes the link; both endpoints fail.
func (l *Link) Cut() {
	l.mu.Lock()
	if l.cut {
		l.mu.Unlock()
		return
	}
	l.cut = true
	l.mu.Unlock()
	l.cond.Broadcast()
	l.a.Close()
	l.ra.Close()
	l.b.Close()
	l.rb.Close()
}

// Cut reports whether the link was cut.
func (l *Link) IsCut() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cut
}

// Pipe returns a connected client/server session pair over a new Link.
func Pipe(cfg SessionConfig) (client, server Session, link *Link, err error) {
	link = NewLink()
	a, b := link.Ends()
	server, err = Server(b, cfg, "mem")
	if err != nil {
		link.Cut()
		return nil, nil, nil, err
	}
	client, err = Client(a, cfg, "mem")
	if err != nil {
		server.Close()
		link.Cut()
		return nil, nil, nil, err
	}
	return client, server, link, nil
}

// MemNetwork connects named nodes in memory. Each node registers an accept
// function (its peer listener); Dial creates a Link, hands one end to the
// target's accept function and returns a client session over the other.
type MemNetwork struct {
	cfg SessionConfig

	mu    sync.Mutex
	nodes map[string]func(net.Conn)
	down  map[string]bool
	links map[*Link][2]string
}

// NewMemNetwork returns an empty network whose sessions use cfg.
func NewMemNetwork(cfg SessionConfig) *MemNetwork {
	return &MemNetwork{
		cfg:   cfg,
		nodes: map[string]func(net.Conn){},
		down:  map[string]bool{},
		links: map[*Link][2]string{},
	}
}

// ErrUnreachable is returned by MemNetwork.Dial for nodes that are down.
var ErrUnreachable = errors.New("network unreachable")

// Listen registers name's peer listener. accept must not block for long; it
// typically starts a server session over the conn.
func (n *MemNetwork) Listen(name string, accept func(net.Conn)) {
	n.mu.Lock()
	n.nodes[name] = accept
	n.mu.Unlock()
}

// Unlisten removes name's listener (its daemon stopped). Existing links stay.
func (n *MemNetwork) Unlisten(name string) {
	n.mu.Lock()
	delete(n.nodes, name)
	n.mu.Unlock()
}

// Dial connects from → to. The returned session is the client side.
func (n *MemNetwork) Dial(ctx context.Context, from, to string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n.mu.Lock()
	if n.down[from] || n.down[to] {
		n.mu.Unlock()
		return nil, fmt.Errorf("dial %s: %w", to, ErrUnreachable)
	}
	accept := n.nodes[to]
	if accept == nil {
		n.mu.Unlock()
		return nil, fmt.Errorf("kran daemon on %s is not running: %w", to, ErrDaemonDown)
	}
	link := NewLink()
	n.links[link] = [2]string{from, to}
	n.mu.Unlock()

	a, b := link.Ends()
	go accept(b)
	s, err := Client(a, n.cfg, "mem:"+to)
	if err != nil {
		link.Cut()
		return nil, err
	}
	go func() {
		<-s.Done()
		link.Cut()
		n.mu.Lock()
		delete(n.links, link)
		n.mu.Unlock()
	}()
	return s, nil
}

func (n *MemNetwork) linksOf(name string) []*Link {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []*Link
	for l, ends := range n.links {
		if ends[0] == name || ends[1] == name {
			out = append(out, l)
		}
	}
	return out
}

// Cut cuts every link touching name (connections die, redials work).
func (n *MemNetwork) Cut(name string) {
	for _, l := range n.linksOf(name) {
		l.Cut()
	}
}

// SetDown simulates a network outage of name: existing links stall (no
// bytes, no close — like a pulled cable) and new dials fail. SetDown(false)
// resumes the stalled links if they are still alive.
func (n *MemNetwork) SetDown(name string, down bool) {
	n.mu.Lock()
	n.down[name] = down
	n.mu.Unlock()
	for _, l := range n.linksOf(name) {
		l.Stall(down)
	}
}
