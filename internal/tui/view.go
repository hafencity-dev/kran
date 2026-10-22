package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/hafencity-dev/kran/internal/proto"
)

// fit truncates plain text to w cells and pads it to exactly w.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

// fitR right-aligns plain text in w cells.
func fitR(s string, w int) string {
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	return strings.Repeat(" ", max(0, w-ansi.StringWidth(s))) + s
}

// clip cuts a styled line to the terminal width.
func clip(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "")
}

// View implements tea.Model; the content always fills the alternate screen.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// render is the whole screen as text (tests inspect it directly).
func (m *Model) render() string {
	if m.log != nil {
		return m.viewLog()
	}
	W, H := max(m.width, 20), max(m.height, 8)
	out := []string{m.header(W)}
	if !m.d.loaded {
		msg := "Connecting to the kran daemon."
		if m.conn != "" && m.conn != "live" {
			msg = "Waiting for the kran daemon: " + m.conn
		}
		out = append(out, "", "  "+msg)
		return strings.Join(out, "\n")
	}

	nodes := m.d.nodeList()
	run := m.d.running()
	wait := m.d.waiting()

	// Height budget: header, three titles, two detail lines, footer.
	avail := max(3, H-7)
	nodeRows := max(1, min(len(nodes), max(1, avail/3)))
	// With room to spare, machines get a second line with their specs.
	need := nodeRows + max(1, len(run)) + max(1, len(wait))
	twoLine := len(nodes) > 0 && nodeRows == len(nodes) && avail-need >= len(nodes)
	nodeLines := nodeRows
	if twoLine {
		nodeLines = 2 * nodeRows
	}
	rest := max(2, avail-nodeLines)
	waitRows := max(1, min(len(wait), rest/2))
	runRows := max(1, rest-waitRows)
	if len(run) < runRows {
		spare := runRows - max(1, len(run))
		runRows -= spare
		waitRows = max(1, min(len(wait), waitRows+spare))
	}

	out = append(out, m.title(paneMachines, len(nodes), nodeRows, W))
	if len(nodes) == 0 {
		out = append(out, m.st.muted.Render("  No machines yet. Link one with: kran link <host>"))
	} else {
		rows := make([]string, len(nodes))
		for i, n := range nodes {
			rows[i] = m.machineRow(n, i == m.sel[paneMachines] && m.pane == paneMachines, W)
			if twoLine {
				rows[i] += "\n" + m.st.muted.Render(clip("  "+strings.Repeat(" ", m.nameW()+1)+m.nodeSpecs(n), W))
			}
		}
		out = append(out, window(rows, m.sel[paneMachines], nodeRows)...)
	}

	out = append(out, m.title(paneRunning, len(run), runRows, W))
	if len(run) == 0 {
		out = append(out, m.st.muted.Render(clip("  Nothing is running. Try: kran run -- go test ./...", W)))
	} else {
		rows := make([]string, len(run))
		for i, j := range run {
			rows[i] = m.runningRow(j, i == m.sel[paneRunning] && m.pane == paneRunning, W)
		}
		out = append(out, window(rows, m.sel[paneRunning], runRows)...)
	}

	out = append(out, m.title(paneWaiting, len(wait), waitRows, W))
	if len(wait) == 0 {
		out = append(out, m.st.muted.Render(clip("  Nothing is waiting.", W)))
	} else {
		rows := make([]string, len(wait))
		for i, j := range wait {
			rows[i] = m.waitingRow(j, i+1, i == m.sel[paneWaiting] && m.pane == paneWaiting, W)
		}
		out = append(out, window(rows, m.sel[paneWaiting], waitRows)...)
	}

	out = strings.Split(strings.Join(out, "\n"), "\n")
	detail := m.detail(W)
	for len(out) < H-1-len(detail) {
		out = append(out, "")
	}
	out = append(out, detail...)
	out = append(out, m.footer(W))
	if len(out) > H {
		out = out[:H]
	}
	return strings.Join(out, "\n")
}

// window returns n rows that keep the selected one visible.
func window(rows []string, sel, n int) []string {
	if len(rows) <= n {
		return rows
	}
	start := max(0, min(sel-n/2, len(rows)-n))
	return rows[start : start+n]
}

func (m *Model) header(W int) string {
	s := m.st
	parts := []string{s.crane.Render("kran top")}
	if m.d.self != "" {
		parts = append(parts, s.bold.Render(m.d.self))
	}
	switch {
	case m.conn == "live":
		parts = append(parts, s.good.Render("live"))
	case m.conn == "":
		parts = append(parts, s.muted.Render("connecting"))
	default:
		parts = append(parts, s.warn.Render("not live"))
	}
	if m.d.loaded {
		parts = append(parts, fmt.Sprintf("%d running", len(m.d.running())), fmt.Sprintf("%d waiting", len(m.d.waiting())))
		if st := m.d.stats; st.JobsToday > 0 {
			parts = append(parts, s.muted.Render(fmt.Sprintf("today %d jobs, %d moved off %s", st.JobsToday, st.OffloadedToday, m.d.self)))
			if st.SavedSecondsToday > 0 {
				parts = append(parts, s.muted.Render("saved "+fmtDur(time.Duration(st.SavedSecondsToday*float64(time.Second)))))
			}
		}
	}
	// Whole parts or nothing: a header cut mid-word reads as a glitch.
	line := ""
	for i, p := range parts {
		next := line
		if i > 0 {
			next += "  "
		}
		next += p
		if ansi.StringWidth(next) > W {
			break
		}
		line = next
	}
	return line
}

func (m *Model) title(p pane, n, shown, W int) string {
	label := fmt.Sprintf("%s %d", p, n)
	if n > shown {
		label += fmt.Sprintf("  (%d shown, move to scroll)", shown)
	}
	if m.pane == p {
		return clip(m.st.active.Render("▸ "+label), W)
	}
	return clip(m.st.title.Render("  "+label), W)
}

func marker(sel bool) string {
	if sel {
		return "› "
	}
	return "  "
}

func (m *Model) row(sel bool, s string) string {
	if sel && !m.st.noColor {
		return m.st.sel.Render(ansi.Strip(s))
	}
	return s
}

// ---------------------------------------------------------------------------
// Machines

func (m *Model) nameW() int {
	w := 6
	for _, x := range m.d.nodes {
		w = max(w, min(12, len(x.Info.Name)))
	}
	return w
}

func (m *Model) nodeSpecs(n proto.NodeState) string {
	i := n.Info
	t := fmt.Sprintf("%s %s, %d cores, %s", i.OSVersion, i.Arch, i.CPUs, fmtBytes(i.MemTotal))
	if len(i.GPUs) > 0 {
		t += ", " + i.GPUs[0].Model
	}
	for _, r := range i.Runtimes {
		if r.Kind == proto.RuntimeContainer && i.OS != "linux" && r.Available {
			t += fmt.Sprintf("; Linux jobs in a %d-core %s Docker VM", r.CPUs, fmtBytes(r.Mem))
		}
	}
	if n.Self {
		t += "; this machine"
	} else if n.Link.RTTMillis > 0 {
		t += fmt.Sprintf("; %.1f ms away", n.Link.RTTMillis)
	}
	return t
}

func (m *Model) machineRow(n proto.NodeState, sel bool, W int) string {
	s := m.st
	nameW := m.nameW()
	status := string(n.Status)
	stStyle := s.good
	switch n.Status {
	case proto.NodeDraining:
		stStyle = s.warn
	case proto.NodeOffline, proto.NodeDegraded:
		stStyle = s.bad
	}
	head := marker(sel) + s.bold.Render(fit(n.Info.Name, nameW)) + " " + stStyle.Render(fit(status, 8)) + " "
	if n.Status == proto.NodeOffline {
		reason := n.Reason
		if reason == "" {
			reason = "not reachable"
		}
		return m.row(sel, clip(head+s.muted.Render(reason), W))
	}
	wide := W >= 110
	fixed := 2 + nameW + 1 + 8 + 1 + 4 + 8 + 1 + 4 + 10
	if wide {
		fixed += 11
	}
	bars := max(16, W-fixed)
	cpuW := bars * 55 / 100
	memW := bars - cpuW
	jobs := m.d.jobsOn(n.Info.Name)
	line := head +
		"cpu " + m.bar(n, jobs, true, cpuW) + fitR(fmtCores(n.Free.CPUs)+" free", 8) + " " +
		"mem " + m.bar(n, jobs, false, memW) + fitR(fmtGiB(n.Free.Mem)+"G free", 10)
	if wide {
		line += s.muted.Render(fitR(fmt.Sprintf("load %.1f", n.Metrics.Load1), 11))
	}
	return m.row(sel, clip(line, W))
}

// bar draws capacity as cells: kran jobs (by owner), jobs from other
// origins, busy outside kran, free, and the owner's reserve. Glyphs differ
// so the bar reads without color.
func (m *Model) bar(n proto.NodeState, jobs []proto.Job, cpu bool, w int) string {
	val := func(r proto.Resources) float64 {
		if cpu {
			return r.CPUs
		}
		return float64(r.Mem)
	}
	capacity, reserve, alloc, free := val(n.Capacity), val(n.Reserve), val(n.Allocated), val(n.Free)
	var sum float64
	for _, j := range jobs {
		sum += val(j.Alloc)
	}
	used := math.Max(alloc, sum)
	others := math.Max(0, alloc-sum)
	outside := math.Max(0, capacity-used-free)
	total := math.Max(capacity, used+outside+free) + reserve
	if total <= 0 || w <= 0 {
		return strings.Repeat(" ", max(0, w))
	}
	type seg struct {
		v     float64
		glyph string
		style lipgloss.Style
	}
	var segs []seg
	for i, j := range jobs {
		g := "█"
		if i%2 == 1 {
			g = "▓" // neighbours stay distinct without color
		}
		segs = append(segs, seg{val(j.Alloc), g, m.st.ownerStyle(j.Spec.Owner.Kind)})
	}
	segs = append(segs,
		seg{others, "▒", m.st.others},
		seg{outside, "▒", m.st.outside},
		seg{free, "·", m.st.free},
		seg{reserve, "░", m.st.reserve},
	)
	var b strings.Builder
	x, prev := 0.0, 0
	for _, sg := range segs {
		if sg.v <= 0 {
			continue
		}
		x += sg.v
		end := min(w, int(math.Round(x/total*float64(w))))
		if end > prev {
			b.WriteString(sg.style.Render(strings.Repeat(sg.glyph, end-prev)))
			prev = end
		}
	}
	if prev < w {
		b.WriteString(m.st.free.Render(strings.Repeat("·", w-prev)))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Jobs

func (m *Model) ownerCell(o proto.Owner, w int) string {
	name := o.Name
	if name == "" {
		name = o.ID
	}
	return m.st.ownerStyle(o.Kind).Render("●") + " " + fit(compactOwner(name, w-2), w-2)
}

// compactOwner keeps what tells sessions apart: "claude · acme-a1" in
// 12 cells becomes "claude…se-a1", not "claude · lo…".
func compactOwner(name string, w int) string {
	if ansi.StringWidth(name) <= w {
		return name
	}
	kind, rest, ok := strings.Cut(name, " · ")
	if !ok || len(kind)+3 > w {
		return name
	}
	tail := w - len(kind) - 1
	r := []rune(rest)
	if tail >= len(r) {
		return kind + " " + rest
	}
	return kind + "…" + string(r[len(r)-tail:])
}

func phaseLabel(s proto.JobState) string {
	switch s {
	case proto.JobSetup:
		return "setup"
	}
	return string(s)
}

func (m *Model) runningRow(j proto.Job, sel bool, W int) string {
	s := m.st
	ownerW, nodeW, timeW := 14, 8, 12
	live := W >= 100
	if W >= 120 {
		ownerW, nodeW = 22, 18
	}
	fixed := 2 + 8 + 1 + 1 + ownerW + 1 + nodeW + 1 + timeW
	if live {
		fixed += 14
	}
	cmdW := max(10, W-fixed)
	elapsed := ""
	if validTime(j.StartedAt) {
		elapsed = fmtDur(m.now().Sub(j.StartedAt))
	}
	if j.Estimate.Duration > 0 {
		elapsed += "/~" + fmtDur(time.Duration(j.Estimate.Duration)*time.Second)
	}
	node := j.Node
	if j.Runtime == proto.RuntimeContainer && W >= 120 {
		node += " (container)"
	}
	if j.Fallback {
		node += " (fallback)"
	}
	phaseStyle := s.bold
	if j.State != proto.JobRunning {
		phaseStyle = s.muted
	}
	line := marker(sel) + phaseStyle.Render(fit(phaseLabel(j.State), 8)) + " " +
		fit(command(j.Spec), cmdW) + " " + m.ownerCell(j.Spec.Owner, ownerW) + " " + fit(node, nodeW) + " " + fitR(elapsed, timeW)
	if live {
		use := ""
		if j.Live != nil && j.State == proto.JobRunning {
			use = fmtCores(j.Live.CPUs) + "c " + fmtGiB(j.Live.Mem) + "G"
		}
		line += s.muted.Render(fitR(use, 14))
	}
	return m.row(sel, clip(line, W))
}

func (m *Model) waitingRow(j proto.Job, pos int, sel bool, W int) string {
	s := m.st
	cmdW, ownerW := 24, 14
	if W >= 120 {
		cmdW, ownerW = 34, 22
	}
	reasonW := max(10, W-(2+4+cmdW+1+ownerW+1))
	reason := j.Reason
	if reason == "" {
		reason = "waiting for the scheduler"
	}
	line := marker(sel) + s.muted.Render(fitR(fmt.Sprint(pos), 3)) + " " + fit(command(j.Spec), cmdW) + " " +
		m.ownerCell(j.Spec.Owner, ownerW) + " " + s.reason.Render(fit(reason, reasonW))
	return m.row(sel, clip(line, W))
}

// ---------------------------------------------------------------------------
// Detail and footer

// detail explains the selected item in up to two lines: a node's specs and
// status reason, or a job's full placement or wait reason.
func (m *Model) detail(W int) []string {
	var text string
	style := m.st.reason
	switch m.pane {
	case paneMachines:
		n, ok := m.selectedNode()
		if !ok {
			return nil
		}
		text = n.Info.Name + ": " + m.nodeSpecs(n)
		if n.Reason != "" {
			text += ". " + n.Reason
		}
		style = m.st.muted
	default:
		j, ok := m.selectedJob()
		if !ok {
			return nil
		}
		text = j.Reason
	}
	if text == "" {
		return nil
	}
	lines := strings.Split(ansi.Wrap(text, W, ""), "\n")
	if len(lines) > 2 {
		lines = lines[:2]
		lines[1] = ansi.Truncate(lines[1], W-1, "") + "…"
	}
	for i := range lines {
		lines[i] = style.Render(clip(lines[i], W))
	}
	return lines
}

func (m *Model) footer(W int) string {
	s := m.st
	switch {
	case m.ask != nil:
		return clip(s.bad.Render(m.ask.prompt)+"  "+s.bold.Render("y")+" yes  "+s.bold.Render("n")+" no", W)
	case m.flash != "":
		if m.flashOK {
			return clip(s.good.Render(m.flash), W)
		}
		return clip(s.warn.Render(m.flash), W)
	case m.conn != "" && m.conn != "live":
		return clip(s.warn.Render(m.conn), W)
	}
	keys := [][2]string{{"tab", "pane"}, {"↑↓", "select"}, {"c", "cancel"}, {"d", "drain/resume"}, {"p", "priority"}, {"l", "log"}, {"q", "quit"}}
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(s.bold.Render(k[0]) + " " + s.muted.Render(k[1]))
	}
	return clip(b.String(), W)
}

func (m *Model) viewLog() string {
	l := m.log
	s := m.st
	W, H := max(m.width, 20), max(m.height, 4)
	status := "following"
	switch {
	case l.err != nil:
		status = "stopped: " + l.err.Error()
	case l.done:
		status = "complete"
	}
	head := clip(s.crane.Render("Output")+" "+s.bold.Render(l.title)+"  "+s.muted.Render(status), W)
	body := H - 2
	lines := l.lines
	if l.partial != "" {
		lines = append(lines[:len(lines):len(lines)], ansi.Strip(l.partial))
	}
	end := max(0, len(lines)-l.scroll)
	start := max(0, end-body)
	out := []string{head}
	for _, ln := range lines[start:end] {
		out = append(out, clip(ln, W))
	}
	for len(out) < H-1 {
		out = append(out, "")
	}
	foot := s.bold.Render("esc") + " back  " + s.bold.Render("↑↓") + " scroll  " + s.bold.Render("G") + " latest"
	if l.scroll > 0 {
		foot += s.warn.Render(fmt.Sprintf("  %d lines below", l.scroll))
	}
	out = append(out, clip(foot, W))
	return strings.Join(out, "\n")
}
