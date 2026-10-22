package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

// peerManager keeps one connection per peer from peers.toml, learns each
// peer's status over metrics.subscribe, applies the circuit breaker and
// feeds the scheduler.
type peerManager struct {
	d *Daemon

	mu      sync.Mutex
	peers   map[string]*peer
	stamp   string
	drained map[string]bool // peers drained from this node (persisted)
	loadErr string
}

type peer struct {
	m      *peerManager
	cfg    PeerConfig
	ctx    context.Context
	cancel context.CancelFunc
	kick   chan struct{}

	mu            sync.Mutex
	out, in       transport.Session
	subscribed    transport.Session
	st            peerStatus
	hasStatus     bool
	lastSeen      time.Time
	lastErr       string
	incompatible  string
	bandwidth     float64
	failures      int
	degradedUntil time.Time
	changed       chan struct{} // closed when sessions change
	lastSig       string        // last published status signature
}

var errIncompatible = errors.New("incompatible kran version")

func newPeerManager(d *Daemon) *peerManager {
	return &peerManager{d: d, peers: map[string]*peer{}, drained: map[string]bool{}}
}

func (m *peerManager) run(ctx context.Context) {
	if m.d.deps.Peers == nil {
		<-ctx.Done()
		return
	}
	m.reload()
	t := time.NewTicker(m.d.t.PeerPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case <-t.C:
		}
		if st := m.d.deps.Peers.Stamp(); st != m.stamp {
			m.reload()
		}
		for _, p := range m.list() {
			p.push(false)
		}
	}
}

func (m *peerManager) reload() {
	src := m.d.deps.Peers
	stamp := src.Stamp()
	cfgs, err := src.Load()
	m.mu.Lock()
	if err != nil {
		// Keep the current peers: a half-written or broken file must not
		// disconnect the mesh.
		if m.loadErr != err.Error() {
			m.d.logf("peers: %v", err)
		}
		m.loadErr = err.Error()
		m.stamp = stamp
		m.mu.Unlock()
		return
	}
	m.loadErr = ""
	m.stamp = stamp
	want := map[string]PeerConfig{}
	for _, c := range cfgs {
		if c.Name == m.d.self {
			continue
		}
		want[c.Name] = c
	}
	var stop []*peer
	var start []PeerConfig
	for name, p := range m.peers {
		c, ok := want[name]
		if !ok || !c.sameConnection(p.cfg) {
			stop = append(stop, p)
			delete(m.peers, name)
		}
	}
	for name, c := range want {
		if _, ok := m.peers[name]; !ok {
			start = append(start, c)
		}
	}
	var started []*peer
	for _, c := range start {
		p := m.newPeer(c)
		m.peers[c.Name] = p
		started = append(started, p)
	}
	m.mu.Unlock()

	changed := false
	for _, p := range stop {
		_, still := want[p.cfg.Name]
		// A changed entry keeps the connection the peer opened to us; only
		// an unlinked peer loses it.
		in := p.stop(!still)
		if !still {
			m.d.deps.Sched.RemoveNode(p.cfg.Name)
			m.d.logf("peers: %s removed", p.cfg.Name)
		} else if in != nil {
			defer m.attachInbound(p.cfg.Name, in)
		}
		changed = true
	}
	for _, p := range started {
		m.d.logf("peers: connecting to %s (%s)", p.cfg.Name, strings.Join(p.cfg.Addresses, ", "))
		m.d.goLoop(func(context.Context) { p.connectLoop() })
		p.push(true)
		changed = true
	}
	if changed {
		m.d.publishState()
	}
}

func (m *peerManager) newPeer(c PeerConfig) *peer {
	ctx, cancel := context.WithCancel(m.d.lifetime())
	return &peer{m: m, cfg: c, ctx: ctx, cancel: cancel, kick: make(chan struct{}, 1), changed: make(chan struct{}),
		lastErr: "connecting"}
}

func (m *peerManager) stopAll() {
	m.mu.Lock()
	ps := make([]*peer, 0, len(m.peers))
	for _, p := range m.peers {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, p := range ps {
		p.stop(true)
	}
}

func (m *peerManager) get(name string) *peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peers[name]
}

func (m *peerManager) list() []*peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*peer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cfg.Name < out[j].cfg.Name })
	return out
}

func (m *peerManager) names() []string {
	var out []string
	for _, p := range m.list() {
		out = append(out, p.cfg.Name)
	}
	return out
}

// session returns a live session to name (our own if up, else theirs).
func (m *peerManager) session(name string) (transport.Session, error) {
	p := m.get(name)
	if p == nil {
		return nil, fmt.Errorf("%s is not a linked node", name)
	}
	if s := p.active(); s != nil {
		return s, nil
	}
	p.mu.Lock()
	reason := p.lastErr
	p.mu.Unlock()
	return nil, fmt.Errorf("%s is not connected (%s)", name, reason)
}

// waitSession waits for a live session to name, asking the connector to
// retry right away.
func (m *peerManager) waitSession(ctx context.Context, name string) (transport.Session, error) {
	for {
		p := m.get(name)
		if p == nil {
			return nil, fmt.Errorf("%s is not a linked node", name)
		}
		p.mu.Lock()
		s := p.activeLocked()
		ch := p.changed
		p.mu.Unlock()
		if s != nil {
			return s, nil
		}
		select {
		case p.kick <- struct{}{}:
		default:
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// attachInbound registers a session a peer opened to us (after its hello).
func (m *peerManager) attachInbound(name string, s transport.Session) {
	p := m.get(name)
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.in != nil && p.in != s {
		p.in.Close()
	}
	p.in = s
	p.notifyLocked()
	p.mu.Unlock()
	p.ensureSubscription()
	p.push(true)
	go func() {
		<-s.Done()
		p.mu.Lock()
		if p.in == s {
			p.in = nil
			p.notifyLocked()
		}
		p.mu.Unlock()
		p.push(true)
	}()
}

func (m *peerManager) recordFailure(name string, err error) {
	p := m.get(name)
	if p == nil {
		return
	}
	p.mu.Lock()
	p.failures++
	tripped := false
	if p.failures >= m.d.t.BreakerThreshold {
		p.failures = 0
		p.degradedUntil = m.d.now().Add(m.d.t.BreakerCooldown)
		tripped = true
	}
	p.mu.Unlock()
	if tripped {
		m.d.logf("peers: %s failed %d jobs in a row before they started (last: %v); not using it for %s",
			name, m.d.t.BreakerThreshold, err, m.d.t.BreakerCooldown)
	}
	p.push(true)
}

func (m *peerManager) recordSuccess(name string) {
	if p := m.get(name); p != nil {
		p.mu.Lock()
		p.failures = 0
		p.mu.Unlock()
	}
}

func (m *peerManager) recordBandwidth(name string, bps float64) {
	if p := m.get(name); p != nil && bps > 0 {
		p.mu.Lock()
		if p.bandwidth == 0 {
			p.bandwidth = bps
		} else {
			p.bandwidth = 0.7*p.bandwidth + 0.3*bps
		}
		p.mu.Unlock()
	}
}

func (m *peerManager) setDrained(name string, on bool) bool {
	known := name == m.d.self || m.get(name) != nil
	if !known {
		return false
	}
	m.mu.Lock()
	if name == m.d.self {
		m.d.draining.Store(on)
	} else if on {
		m.drained[name] = true
	} else {
		delete(m.drained, name)
	}
	var names []string
	for n := range m.drained {
		names = append(names, n)
	}
	if m.d.draining.Load() {
		names = append(names, m.d.self)
	}
	m.mu.Unlock()
	sort.Strings(names)
	b, _ := json.Marshal(names)
	m.d.kvSet("drained", string(b))
	if p := m.get(name); p != nil {
		p.push(true)
	}
	return true
}

func (m *peerManager) isDrained(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drained[name]
}

// --- peer ---

// stop ends the peer's connector and its own session. It returns the
// inbound session when it is kept open (closeInbound false).
func (p *peer) stop(closeInbound bool) transport.Session {
	p.cancel()
	p.mu.Lock()
	out, in := p.out, p.in
	p.out, p.in = nil, nil
	p.notifyLocked()
	p.mu.Unlock()
	if out != nil {
		out.Close()
	}
	if in != nil && closeInbound {
		in.Close()
		in = nil
	}
	if in != nil && in.Err() != nil {
		in = nil
	}
	return in
}

func (p *peer) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *peer) activeLocked() transport.Session {
	if p.out != nil && p.out.Err() == nil {
		return p.out
	}
	if p.in != nil && p.in.Err() == nil {
		return p.in
	}
	return nil
}

func (p *peer) active() transport.Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeLocked()
}

func (p *peer) backoff(attempt int) time.Duration {
	t := p.m.d.t
	d := t.BackoffMin
	for i := 0; i < attempt && d < t.BackoffMax; i++ {
		d *= 2
	}
	if d > t.BackoffMax {
		d = t.BackoffMax
	}
	// Jitter in [d/2, d] keeps a mesh of daemons from reconnecting in lockstep.
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func (p *peer) connectLoop() {
	d := p.m.d
	if !p.cfg.dials() || d.deps.Dialer == nil {
		p.mu.Lock()
		if d.deps.Dialer == nil && p.cfg.dials() {
			p.lastErr = "no kran key on this node (run kran setup)"
		} else {
			p.lastErr = "waiting for " + p.cfg.Name + " to connect"
		}
		p.mu.Unlock()
		<-p.ctx.Done()
		return
	}
	attempt := 0
	for p.ctx.Err() == nil {
		dctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
		sess, err := d.deps.Dialer.Dial(dctx, p.cfg)
		var hello proto.Hello
		if err == nil {
			hello, err = d.hello(dctx, sess)
			if err != nil {
				sess.Close()
			}
		}
		cancel()
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			wait := p.backoff(attempt)
			attempt++
			p.mu.Lock()
			msg := humanDialError(p.cfg.Name, err)
			if p.lastErr != msg {
				d.logf("peers: %s: %s", p.cfg.Name, msg)
			}
			p.lastErr = msg
			p.incompatible = ""
			if errors.Is(err, errIncompatible) {
				p.incompatible = msg
				wait = d.t.BackoffMax
			}
			p.mu.Unlock()
			p.push(true)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(wait):
			case <-p.kick:
			}
			continue
		}
		attempt = 0
		connected := d.now()
		if hello.Node.Name != "" && hello.Node.Name != p.cfg.Name {
			d.logf("peers: %s calls itself %q; using the linked name", p.cfg.Name, hello.Node.Name)
		}
		untrack := d.trackSession(sess)
		p.mu.Lock()
		p.out = sess
		p.lastErr = ""
		p.incompatible = ""
		p.notifyLocked()
		p.mu.Unlock()
		d.logf("peers: connected to %s via %s (rtt %s)", p.cfg.Name, sess.RemoteAddr(), sess.RTT().Round(time.Millisecond))
		go d.servePeerSession(sess, p.cfg.Name)
		p.ensureSubscription()
		p.push(true)

		select {
		case <-sess.Done():
		case <-p.ctx.Done():
			sess.Close()
		}
		untrack()
		p.mu.Lock()
		if p.out == sess {
			p.out = nil
			p.notifyLocked()
		}
		reason := "connection lost"
		if err := sess.Err(); err != nil && p.ctx.Err() == nil {
			reason = "connection lost: " + err.Error()
		}
		p.lastErr = reason
		p.mu.Unlock()
		if p.ctx.Err() != nil {
			return
		}
		d.logf("peers: %s: %s", p.cfg.Name, reason)
		p.push(true)
		if d.now().Sub(connected) < 30*time.Second {
			attempt = 1 // a flapping link backs off; a long-lived one retries at once
		}
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(p.backoff(attempt)):
		case <-p.kick:
		}
	}
}

func humanDialError(name string, err error) string {
	switch {
	case errors.Is(err, errIncompatible):
		return err.Error()
	case errors.Is(err, transport.ErrDaemonDown):
		return fmt.Sprintf("kran daemon on %s is not running", name)
	case errors.Is(err, transport.ErrHostKey):
		return fmt.Sprintf("%s presented a different host key than at link time; if it was reinstalled, run kran link %s again", name, name)
	case errors.Is(err, transport.ErrAuth):
		return fmt.Sprintf("%s refused kran's key; run kran link %s again", name, name)
	case errors.Is(err, transport.ErrUnreachable), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("%s is unreachable", name)
	}
	return err.Error()
}

// ensureSubscription keeps one metrics subscription on the active session.
func (p *peer) ensureSubscription() {
	p.mu.Lock()
	s := p.activeLocked()
	if s == nil || p.subscribed == s {
		p.mu.Unlock()
		return
	}
	p.subscribed = s
	p.mu.Unlock()
	go p.subscribe(s)
}

func (p *peer) subscribe(s transport.Session) {
	d := p.m.d
	defer func() {
		p.mu.Lock()
		if p.subscribed == s {
			p.subscribed = nil
		}
		p.mu.Unlock()
		if p.ctx.Err() == nil {
			p.ensureSubscription()
		}
	}()
	for p.ctx.Err() == nil && s.Err() == nil {
		if p.active() != s {
			return
		}
		ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)
		pc, err := peerCall(ctx, s, proto.PeerMetricsSubscribe, "", nil, nil)
		cancel()
		if err != nil {
			select {
			case <-p.ctx.Done():
				return
			case <-s.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		stop := context.AfterFunc(p.ctx, func() { pc.Close() })
		for {
			pc.SetReadDeadline(time.Now().Add(3*d.t.MetricsEvery + 5*time.Second))
			var st peerStatus
			if err := pc.readJSON(&st); err != nil {
				break
			}
			p.mu.Lock()
			p.st = st
			p.hasStatus = true
			p.lastSeen = d.now()
			p.mu.Unlock()
			p.push(false)
			d.applyRemoteLive(p.cfg.Name, st.Allocs)
		}
		stop()
		pc.Close()
	}
}

// nodeState is this peer as the scheduler and the UI see it.
func (p *peer) nodeState() proto.NodeState {
	d := p.m.d
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.st
	ns := proto.NodeState{
		Info:     st.Info,
		Metrics:  st.Metrics,
		Reserve:  st.Reserve,
		Accepts:  st.Accepts,
		LastSeen: p.lastSeen,
	}
	ns.Info.Name = p.cfg.Name
	if ns.Info.ID == "" {
		ns.Info.ID = p.cfg.ID
	}
	s := p.activeLocked()
	now := d.now()
	switch {
	case p.incompatible != "":
		ns.Status, ns.Reason = proto.NodeOffline, p.incompatible
	case s == nil:
		ns.Status, ns.Reason = proto.NodeOffline, p.lastErr
	case !p.hasStatus:
		ns.Status, ns.Reason = proto.NodeOffline, "connecting"
	case st.Draining:
		ns.Status, ns.Reason = proto.NodeDraining, p.cfg.Name+" is draining"
	case p.m.isDrained(p.cfg.Name):
		ns.Status, ns.Reason = proto.NodeDraining, "drained from "+d.self+" (undo: kran resume "+p.cfg.Name+")"
	case now.Before(p.degradedUntil):
		ns.Status = proto.NodeDegraded
		ns.Reason = fmt.Sprintf("%d jobs in a row failed before they started; trying again at %s",
			d.t.BreakerThreshold, p.degradedUntil.Local().Format("15:04:05"))
	case st.Degraded != "":
		ns.Status, ns.Reason = proto.NodeDegraded, st.Degraded
	default:
		ns.Status = proto.NodeOnline
	}
	// Capacity is net of what other origins (and jobs this scheduler does
	// not track, e.g. reattached after a restart) hold on the node, so the
	// scheduler only subtracts its own allocations.
	ns.Capacity = st.Capacity
	for _, a := range st.Allocs {
		ns.Allocated.CPUs += a.Res.CPUs
		ns.Allocated.Mem += a.Res.Mem
		if a.Running {
			ns.Running++
		}
		if a.Origin == d.self && d.jobs.schedHolds(a.JobID) {
			continue
		}
		ns.Capacity.CPUs -= a.Res.CPUs
		ns.Capacity.Mem -= a.Res.Mem
	}
	if ns.Capacity.CPUs < 0 {
		ns.Capacity.CPUs = 0
	}
	if ns.Capacity.Mem < 0 {
		ns.Capacity.Mem = 0
	}
	ns.Link = proto.LinkInfo{Direction: p.cfg.Direction, BandwidthBps: p.bandwidth, Connected: s != nil}
	if s != nil {
		ns.Link.Address = s.RemoteAddr()
		ns.Link.RTTMillis = float64(s.RTT().Microseconds()) / 1000
	}
	return ns
}

// push sends the peer's state to the scheduler and, when something visible
// changed, to the UI. Metrics alone go out throttled.
func (p *peer) push(force bool) {
	d := p.m.d
	ns := p.nodeState()
	d.deps.Sched.UpdateNode(ns)
	sig := fmt.Sprintf("%s|%s|%v|%v|%d", ns.Status, ns.Reason, ns.Link.Connected, ns.Accepts, ns.Running)
	p.mu.Lock()
	changed := sig != p.lastSig
	p.lastSig = sig
	p.mu.Unlock()
	if changed || force {
		d.bus.publish(proto.EventNode, ns)
	} else if !ns.Metrics.Time.IsZero() {
		d.bus.publishMetrics(ns.Info.Name, ns.Metrics)
	}
	if changed {
		d.deps.Sched.Kick()
	}
}

// hello performs the version handshake on a fresh session.
func (d *Daemon) hello(ctx context.Context, s transport.Session) (proto.Hello, error) {
	var theirs proto.Hello
	err := peerOneShot(ctx, s, proto.PeerHello, "", d.helloBody(), &theirs)
	if err != nil {
		if peerCode(err) == codeVersion {
			return theirs, fmt.Errorf("%w: %v", errIncompatible, err)
		}
		return theirs, fmt.Errorf("hello: %w", err)
	}
	if !compatibleProtocol(theirs.Protocol) {
		return theirs, fmt.Errorf("%w: %s runs kran %s (protocol %s), this node runs %s (protocol %s); run kran upgrade",
			errIncompatible, theirs.Node.Name, theirs.Node.Version, theirs.Protocol, d.nodeInfo().Version, protocolString())
	}
	return theirs, nil
}

func (d *Daemon) helloBody() proto.Hello {
	return proto.Hello{Node: d.nodeInfo(), Protocol: protocolString(), Peers: d.peers.names()}
}

func (d *Daemon) loadDrains() {
	v, ok := d.kvGet("drained")
	if !ok || v == "" {
		return
	}
	var names []string
	if json.Unmarshal([]byte(v), &names) != nil {
		return
	}
	d.peers.mu.Lock()
	for _, n := range names {
		if n == d.self {
			d.draining.Store(true)
			continue
		}
		d.peers.drained[n] = true
	}
	d.peers.mu.Unlock()
}

// applyRemoteLive copies the live usage a target reports into our jobs.
func (d *Daemon) applyRemoteLive(node string, allocs []allocInfo) {
	for _, a := range allocs {
		if a.Origin != d.self || a.Live == nil {
			continue
		}
		e := d.jobs.get(a.JobID)
		if e == nil {
			continue
		}
		live := *a.Live
		e.mu.Lock()
		same := e.job.Live != nil && *e.job.Live == live
		running := e.job.State == proto.JobRunning && e.job.Node == node
		e.mu.Unlock()
		if running && !same {
			e.update(func(j *proto.Job) { j.Live = &live })
		}
	}
}
