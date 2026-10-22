package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/ui/mock"
)

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func newTest(t *testing.T, w, h int) (*Model, *mock.Sim) {
	t.Helper()
	sim := mock.New(mock.Options{Seed: 3, Now: func() time.Time { return t0 }})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, mock.Client{S: sim}, Options{NoColor: true, Now: func() time.Time { return t0.Add(30 * time.Second) }})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	// Subscribe the way Init does and apply the first (state) event.
	msg := m.subscribe()().(eventsMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m.Update(msg)
	m.Update(waitEvent(m.events)())
	if !m.d.loaded {
		t.Fatal("state not loaded from the first event")
	}
	return m, sim
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return tea.KeyPressMsg{Code: []rune(s)[0], Text: s}
}

func TestViewFitsTerminal(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{80, 24}, {100, 30}, {160, 50}} {
		m, _ := newTest(t, sz.w, sz.h)
		v := m.render()
		lines := strings.Split(v, "\n")
		if len(lines) > sz.h {
			t.Errorf("%dx%d: %d lines", sz.w, sz.h, len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > sz.w {
				t.Errorf("%dx%d: line %d is %d wide: %q", sz.w, sz.h, i, w, l)
			}
		}
		for _, want := range []string{"gx10", "devmac", "e2e", "Machines 3", "Running", "Waiting", "q quit"} {
			if !strings.Contains(v, want) {
				t.Errorf("%dx%d: view lacks %q\n%s", sz.w, sz.h, want, v)
			}
		}
	}
}

func TestWaitReasonShown(t *testing.T) {
	m, _ := newTest(t, 160, 50)
	wait := m.d.waiting()
	if len(wait) == 0 {
		t.Skip("simulation has no queue with this seed")
	}
	v := m.render()
	if !strings.Contains(v, "waiting: needs") {
		t.Errorf("queue reasons missing:\n%s", v)
	}
}

func TestNoColorHasNoEscapes(t *testing.T) {
	m, _ := newTest(t, 120, 40)
	if v := m.render(); strings.Contains(v, "\x1b") {
		t.Errorf("NO_COLOR view contains escape sequences")
	}
}

func TestCancelAsksFirst(t *testing.T) {
	m, sim := newTest(t, 100, 30)
	run := m.d.running()
	if len(run) == 0 {
		t.Fatal("no running job in the simulation")
	}
	m.pane = paneRunning
	m.sel[paneRunning] = 0
	m.Update(key("c"))
	if m.ask == nil || !strings.Contains(m.ask.prompt, command(run[0].Spec)) {
		t.Fatalf("expected a confirmation naming the job, got %+v", m.ask)
	}
	if !strings.Contains(m.render(), "y yes") {
		t.Error("confirmation not shown in the footer")
	}
	// Anything but y keeps the job.
	m.Update(key("n"))
	if j, _ := sim.Job(run[0].Spec.ID); j.State.Terminal() {
		t.Fatal("job canceled without confirmation")
	}
	m.Update(key("c"))
	_, cmd := m.Update(key("y"))
	if cmd == nil {
		t.Fatal("no command after confirming")
	}
	done := cmd().(actionDoneMsg)
	if done.err != nil {
		t.Fatal(done.err)
	}
	if j, _ := sim.Job(run[0].Spec.ID); j.State != proto.JobCanceled {
		t.Fatalf("job state %s, want canceled", j.State)
	}
}

func TestDrainAndResume(t *testing.T) {
	m, sim := newTest(t, 100, 30)
	m.pane = paneMachines
	m.sel[paneMachines] = 1 // devmac (gx10 is first as this machine)
	m.Update(key("d"))
	if m.ask == nil {
		t.Fatal("drain did not ask")
	}
	_, cmd := m.Update(key("y"))
	if res := cmd().(actionDoneMsg); res.err != nil {
		t.Fatal(res.err)
	}
	if st := sim.State(); st.Nodes[1].Status != proto.NodeDraining {
		t.Fatalf("devmac status %s", st.Nodes[1].Status)
	}
	m.d.applyState(sim.State())
	_, cmd = m.Update(key("d"))
	if m.ask != nil || cmd == nil {
		t.Fatal("resume should not ask")
	}
	if res := cmd().(actionDoneMsg); res.err != nil {
		t.Fatal(res.err)
	}
	if st := sim.State(); st.Nodes[1].Status != proto.NodeOnline {
		t.Fatalf("devmac status %s after resume", st.Nodes[1].Status)
	}
}

func TestTabCyclesPanes(t *testing.T) {
	m, _ := newTest(t, 80, 24)
	start := m.pane
	seen := map[pane]bool{}
	for i := 0; i < int(paneCount); i++ {
		m.Update(key("tab"))
		seen[m.pane] = true
	}
	if m.pane != start || len(seen) != int(paneCount) {
		t.Fatalf("tab did not cycle through all panes: %v", seen)
	}
}

func TestFollowLog(t *testing.T) {
	m, sim := newTest(t, 80, 24)
	m.pane = paneRunning
	m.sel[paneRunning] = -1
	for i, j := range m.d.running() {
		if j.State == proto.JobRunning {
			m.sel[paneRunning] = i
			break
		}
	}
	if m.sel[paneRunning] < 0 {
		t.Fatal("no job in the running phase")
	}
	_, cmd := m.Update(key("l"))
	if m.log == nil || cmd == nil {
		t.Fatal("log view not opened")
	}
	// Let the simulation produce output, then drain what arrived.
	for i := 0; i < 20; i++ {
		sim.Step()
	}
	deadline := time.After(5 * time.Second)
	for len(m.log.lines) == 0 {
		ch := make(chan tea.Msg, 1)
		go func() { ch <- cmd() }()
		select {
		case msg := <-ch:
			_, cmd = m.Update(msg)
			if cmd == nil {
				t.Fatal("log stream ended without output")
			}
		case <-deadline:
			t.Fatal("no log output")
		}
	}
	if v := m.render(); !strings.Contains(v, "Output") || strings.Contains(v, "\x1b") {
		t.Errorf("log view: %q", v)
	}
	m.Update(key("esc"))
	if m.log != nil {
		t.Fatal("esc did not close the log view")
	}
}

func TestApplyDecodesJSONData(t *testing.T) {
	d := newData()
	sim := mock.New(mock.Options{Seed: 1, Now: func() time.Time { return t0 }})
	roundtrip := func(v any) any {
		b, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(b, &out)
		return out
	}
	if _, err := d.apply(proto.Event{Type: proto.EventState, Seq: 10, Data: roundtrip(sim.State())}); err != nil {
		t.Fatal(err)
	}
	if len(d.nodes) != 3 || d.self != "gx10" {
		t.Fatalf("state not applied: %d nodes, self %q", len(d.nodes), d.self)
	}
	gx := d.nodes["gx10"]
	gx.Metrics.Load1 = 42
	if _, err := d.apply(proto.Event{Type: proto.EventNodeMetrics, Seq: 11, Data: roundtrip(proto.NodeMetricsEvent{Name: "gx10", Metrics: gx.Metrics})}); err != nil {
		t.Fatal(err)
	}
	if d.nodes["gx10"].Metrics.Load1 != 42 {
		t.Error("metrics event not applied")
	}
	if _, err := d.apply(proto.Event{Type: proto.EventQueue, Seq: 12, Data: roundtrip(map[string][]string{"queue": {"a", "b"}})}); err != nil || len(d.queue) != 2 {
		t.Fatalf("queue event: %v %v", err, d.queue)
	}
	if gap, _ := d.apply(proto.Event{Type: proto.EventStats, Seq: 15, Data: roundtrip(proto.Stats{})}); !gap {
		t.Error("missed seq 13..14 not detected")
	}
}

func TestLogLineCleanup(t *testing.T) {
	var l logView
	l.write("\x1b[32mok\x1b[0m  pkg\t1.2s\r\nprogress 10%\rprogress 100%\npart")
	if got := strings.Join(l.lines, "|"); got != "ok  pkg    1.2s|progress 100%" {
		t.Fatalf("lines = %q", got)
	}
	if l.partial != "part" {
		t.Fatalf("partial = %q", l.partial)
	}
}

func TestColorStylesEmitColor(t *testing.T) {
	// Guards the NO_COLOR test against passing because styles never color.
	if s := newStyles(false).good.Render("ok"); !strings.Contains(s, "\x1b[") {
		t.Fatalf("colored style rendered %q", s)
	}
	if s := newStyles(true).good.Render("ok"); s != "ok" {
		t.Fatalf("NO_COLOR style rendered %q", s)
	}
}
