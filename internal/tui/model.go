package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/hafencity-dev/kran/internal/client"
	"github.com/hafencity-dev/kran/internal/proto"
)

type pane int

const (
	paneMachines pane = iota
	paneRunning
	paneWaiting
	paneCount
)

func (p pane) String() string {
	return [...]string{"Machines", "Running", "Waiting"}[p]
}

// Messages.
type (
	eventsMsg struct {
		ch     <-chan proto.Event
		cancel context.CancelFunc
		err    error
	}
	// Event messages carry their channel so a replaced subscription's
	// leftovers are ignored.
	eventMsg struct {
		ev proto.Event
		ch <-chan proto.Event
	}
	eventsEndMsg  struct{ ch <-chan proto.Event }
	reconnectMsg  struct{}
	tickMsg       time.Time
	actionDoneMsg struct {
		text string
		err  error
	}
	logMsg struct {
		id   string
		data []byte
		done bool
		err  error
	}
)

type confirm struct {
	prompt string
	run    tea.Cmd
}

type logView struct {
	id      string
	title   string
	lines   []string
	partial string
	done    bool
	err     error
	scroll  int // lines up from the bottom
	cancel  context.CancelFunc
	ch      <-chan logMsg
}

const maxLogLines = 3000

// Model is the bubbletea model behind `kran top`.
type Model struct {
	ctx     context.Context
	c       client.Client
	st      styles
	now     func() time.Time
	d       data
	width   int
	height  int
	pane    pane
	sel     [paneCount]int
	ask     *confirm
	flash   string
	flashOK bool
	flashAt time.Time
	conn    string // "" connecting, "live", or why it is not
	events  <-chan proto.Event
	unsub   context.CancelFunc
	log     *logView
}

// Options configure New.
type Options struct {
	NoColor bool
	Now     func() time.Time
}

// New returns the model; Run wires it to a terminal.
func New(ctx context.Context, c client.Client, o Options) *Model {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Model{ctx: ctx, c: c, st: newStyles(o.NoColor), now: o.Now, d: newData(), width: 80, height: 24, pane: paneRunning}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.subscribe(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) subscribe() tea.Cmd {
	parent, c := m.ctx, m.c
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(parent)
		ch, err := c.Events(ctx)
		if err != nil {
			cancel()
		}
		return eventsMsg{ch: ch, cancel: cancel, err: err}
	}
}

func waitEvent(ch <-chan proto.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsEndMsg{ch: ch}
		}
		return eventMsg{ev: ev, ch: ch}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		if m.flash != "" && m.now().Sub(m.flashAt) > 5*time.Second {
			m.flash = ""
		}
		return m, tick()
	case eventsMsg:
		if msg.err != nil {
			m.conn = "cannot reach the kran daemon (" + msg.err.Error() + "); start it with kran service start"
			return m, retry()
		}
		if m.unsub != nil {
			m.unsub()
		}
		m.events, m.unsub = msg.ch, msg.cancel
		return m, waitEvent(msg.ch)
	case eventMsg:
		if msg.ch != m.events {
			return m, nil
		}
		gap, err := m.d.apply(msg.ev)
		switch {
		case gap:
			// Missed events: resubscribe for a fresh state.
			m.conn = "catching up"
			m.d.lastSeq = 0
			return m, m.subscribe()
		case err != nil:
			m.setFlash("Skipped an update the daemon sent: "+err.Error(), false)
		default:
			m.conn = "live"
		}
		m.clamp()
		return m, waitEvent(m.events)
	case eventsEndMsg:
		if msg.ch != m.events {
			return m, nil
		}
		m.conn = "lost the connection to the daemon, retrying"
		return m, retry()
	case reconnectMsg:
		return m, m.subscribe()
	case actionDoneMsg:
		if msg.err != nil {
			m.setFlash(msg.text+": "+msg.err.Error(), false)
		} else {
			m.setFlash(msg.text, true)
		}
		return m, nil
	case logMsg:
		return m, m.onLog(msg)
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func retry() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return reconnectMsg{} })
}

func (m *Model) setFlash(s string, ok bool) {
	m.flash, m.flashOK, m.flashAt = s, ok, m.now()
}

func (m *Model) count(p pane) int {
	switch p {
	case paneMachines:
		return len(m.d.nodes)
	case paneRunning:
		return len(m.d.running())
	}
	return len(m.d.waiting())
}

func (m *Model) clamp() {
	for p := pane(0); p < paneCount; p++ {
		n := m.count(p)
		if m.sel[p] >= n {
			m.sel[p] = n - 1
		}
		if m.sel[p] < 0 {
			m.sel[p] = 0
		}
	}
}

// selectedJob returns the job under the cursor in the running or waiting pane.
func (m *Model) selectedJob() (proto.Job, bool) {
	var list []proto.Job
	switch m.pane {
	case paneRunning:
		list = m.d.running()
	case paneWaiting:
		list = m.d.waiting()
	default:
		return proto.Job{}, false
	}
	if i := m.sel[m.pane]; i >= 0 && i < len(list) {
		return list[i], true
	}
	return proto.Job{}, false
}

func (m *Model) selectedNode() (proto.NodeState, bool) {
	nodes := m.d.nodeList()
	if i := m.sel[paneMachines]; i >= 0 && i < len(nodes) {
		return nodes[i], true
	}
	return proto.NodeState{}, false
}

func (m *Model) onKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, m.quit()
	}
	if m.log != nil {
		return m.onLogKey(key)
	}
	if m.ask != nil {
		a := m.ask
		m.ask = nil
		switch key {
		case "y", "Y", "enter":
			return m, a.run
		}
		m.setFlash("Nothing changed.", true)
		return m, nil
	}
	switch key {
	case "q":
		return m, m.quit()
	case "tab", "right":
		m.pane = (m.pane + 1) % paneCount
	case "shift+tab", "left":
		m.pane = (m.pane + paneCount - 1) % paneCount
	case "1", "2", "3":
		m.pane = pane(key[0] - '1')
	case "up", "k":
		if m.sel[m.pane] > 0 {
			m.sel[m.pane]--
		}
	case "down", "j":
		if m.sel[m.pane] < m.count(m.pane)-1 {
			m.sel[m.pane]++
		}
	case "home", "g":
		m.sel[m.pane] = 0
	case "end", "G":
		m.sel[m.pane] = max(0, m.count(m.pane)-1)
	case "c", "x":
		return m, m.askCancel()
	case "d":
		return m, m.drainOrResume()
	case "p":
		return m, m.cyclePriority()
	case "l", "enter":
		return m, m.openLog()
	}
	return m, nil
}

func (m *Model) askCancel() tea.Cmd {
	j, ok := m.selectedJob()
	if !ok {
		m.setFlash("Select a running or waiting job first (tab switches panes).", false)
		return nil
	}
	ctx, c, id, cmd := m.ctx, m.c, j.Spec.ID, command(j.Spec)
	verb := "Stop"
	if j.State == proto.JobQueued {
		verb = "Remove from the queue"
	}
	where := ""
	if j.Node != "" {
		where = " on " + j.Node
	}
	m.ask = &confirm{
		prompt: verb + ": " + cmd + where + "?",
		run: func() tea.Msg {
			err := c.Cancel(ctx, id)
			return actionDoneMsg{text: "Canceled " + cmd, err: err}
		},
	}
	return nil
}

func (m *Model) drainOrResume() tea.Cmd {
	if m.pane != paneMachines {
		m.setFlash("Drain works on machines: press tab to reach the Machines pane.", false)
		return nil
	}
	n, ok := m.selectedNode()
	if !ok {
		return nil
	}
	ctx, c, name := m.ctx, m.c, n.Info.Name
	switch n.Status {
	case proto.NodeDraining:
		return func() tea.Msg {
			return actionDoneMsg{text: name + " takes jobs again", err: c.Resume(ctx, name)}
		}
	case proto.NodeOnline, proto.NodeDegraded:
		p := "Drain " + name + "? It finishes running jobs and takes no new ones."
		if n.Running > 0 {
			p = "Drain " + name + "? Its " + strconv.Itoa(n.Running) + " running jobs finish; it takes no new ones."
		}
		m.ask = &confirm{prompt: p, run: func() tea.Msg {
			return actionDoneMsg{text: name + " is draining", err: c.Drain(ctx, name)}
		}}
	default:
		m.setFlash(name+" is "+string(n.Status)+"; there is nothing to drain.", false)
	}
	return nil
}

func (m *Model) cyclePriority() tea.Cmd {
	j, ok := m.selectedJob()
	if !ok || j.State != proto.JobQueued {
		m.setFlash("Priority applies to waiting jobs.", false)
		return nil
	}
	next := map[proto.Priority]proto.Priority{proto.PriorityHigh: proto.PriorityLow, proto.PriorityNormal: proto.PriorityHigh, proto.PriorityLow: proto.PriorityNormal}[j.Spec.Priority]
	if next == "" {
		next = proto.PriorityNormal
	}
	ctx, c, id, cmd := m.ctx, m.c, j.Spec.ID, command(j.Spec)
	return func() tea.Msg {
		return actionDoneMsg{text: "Priority " + string(next) + " for " + cmd, err: c.SetPriority(ctx, id, next)}
	}
}

// ---------------------------------------------------------------------------
// Log view

type chanWriter struct {
	ctx context.Context
	id  string
	ch  chan<- logMsg
}

func (w chanWriter) Write(p []byte) (int, error) {
	b := append([]byte(nil), p...)
	select {
	case w.ch <- logMsg{id: w.id, data: b}:
		return len(p), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

func (m *Model) openLog() tea.Cmd {
	j, ok := m.selectedJob()
	if !ok {
		m.setFlash("Select a running or waiting job to follow its output.", false)
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	ch := make(chan logMsg, 64)
	id := j.Spec.ID
	title := command(j.Spec)
	if j.Node != "" {
		title += " on " + j.Node
	}
	m.log = &logView{id: id, title: title, cancel: cancel, ch: ch}
	c := m.c
	go func() {
		defer close(ch)
		err := c.Log(ctx, id, true, chanWriter{ctx: ctx, id: id, ch: ch})
		if ctx.Err() != nil {
			return
		}
		select {
		case ch <- logMsg{id: id, done: true, err: err}:
		case <-ctx.Done():
		}
	}()
	return waitLog(ch)
}

func waitLog(ch <-chan logMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m *Model) onLog(msg logMsg) tea.Cmd {
	l := m.log
	if l == nil || msg.id != l.id {
		return nil
	}
	if msg.done {
		l.done, l.err = true, msg.err
		if l.partial != "" {
			l.push(l.partial)
			l.partial = ""
		}
		return nil
	}
	l.write(string(msg.data))
	return waitLog(l.ch)
}

func (l *logView) write(s string) {
	s = l.partial + s
	parts := strings.Split(s, "\n")
	l.partial = parts[len(parts)-1]
	for _, p := range parts[:len(parts)-1] {
		l.push(p)
	}
}

func (l *logView) push(line string) {
	line = strings.TrimSuffix(line, "\r")
	// A bare \r redraws the line (progress bars): keep the last state.
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	line = strings.ReplaceAll(ansi.Strip(line), "\t", "    ")
	l.lines = append(l.lines, line)
	if len(l.lines) > maxLogLines {
		l.lines = append([]string(nil), l.lines[len(l.lines)-maxLogLines:]...)
	}
}

// quit releases the subscription and any log follower before exiting.
func (m *Model) quit() tea.Cmd {
	m.closeLog()
	if m.unsub != nil {
		m.unsub()
		m.unsub = nil
	}
	return tea.Quit
}

func (m *Model) closeLog() {
	if m.log != nil {
		m.log.cancel()
		m.log = nil
	}
}

func (m *Model) onLogKey(key string) (tea.Model, tea.Cmd) {
	l := m.log
	page := max(1, m.height-3)
	switch key {
	case "q", "esc", "l", "backspace":
		m.closeLog()
	case "up", "k":
		l.scroll++
	case "down", "j":
		l.scroll--
	case "pgup", "b":
		l.scroll += page
	case "pgdown", "space", "f":
		l.scroll -= page
	case "end", "G":
		l.scroll = 0
	case "home", "g":
		l.scroll = len(l.lines)
	}
	l.scroll = max(0, min(l.scroll, len(l.lines)-1))
	return m, nil
}
