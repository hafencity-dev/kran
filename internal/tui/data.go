package tui

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hafencity-dev/kran/internal/proto"
)

// maxFinished bounds the finished jobs kept for the log view lookups.
const maxFinished = 200

// data is the TUI's copy of the daemon state, kept current from events.
type data struct {
	loaded  bool
	self    string
	version proto.VersionInfo
	nodes   map[string]proto.NodeState
	jobs    map[string]proto.Job
	queue   []string
	stats   proto.Stats
	lastSeq uint64
}

func newData() data {
	return data{nodes: map[string]proto.NodeState{}, jobs: map[string]proto.Job{}}
}

func (d *data) applyState(st *proto.State) {
	d.loaded = true
	d.self = st.Self
	d.version = st.Version
	d.nodes = make(map[string]proto.NodeState, len(st.Nodes))
	for _, n := range st.Nodes {
		d.nodes[n.Info.Name] = n
	}
	d.jobs = make(map[string]proto.Job, len(st.Jobs))
	for _, j := range st.Jobs {
		d.jobs[j.Spec.ID] = j
	}
	d.queue = append([]string(nil), st.Queue...)
	d.stats = st.Stats
}

// apply folds one event into the state. It reports gap=true when events
// were missed and only a fresh state can be trusted.
func (d *data) apply(ev proto.Event) (gap bool, err error) {
	if ev.Type != proto.EventState && ev.Seq != 0 && d.lastSeq != 0 && ev.Seq > d.lastSeq+1 {
		return true, nil
	}
	if ev.Seq != 0 {
		d.lastSeq = ev.Seq
	}
	switch ev.Type {
	case proto.EventState:
		st, err := decode[proto.State](ev.Data)
		if err != nil {
			return false, err
		}
		d.applyState(&st)
	case proto.EventNode:
		n, err := decode[proto.NodeState](ev.Data)
		if err != nil {
			return false, err
		}
		d.nodes[n.Info.Name] = n
	case proto.EventNodeMetrics:
		m, err := decode[proto.NodeMetricsEvent](ev.Data)
		if err != nil {
			return false, err
		}
		if n, ok := d.nodes[m.Name]; ok {
			n.Metrics = m.Metrics
			d.nodes[m.Name] = n
		}
	case proto.EventJob:
		j, err := decode[proto.Job](ev.Data)
		if err != nil {
			return false, err
		}
		d.jobs[j.Spec.ID] = j
		if j.State.Terminal() {
			d.trim()
		}
	case proto.EventQueue:
		q, err := decode[struct {
			Queue []string `json:"queue"`
		}](ev.Data)
		if err != nil {
			return false, err
		}
		d.queue = q.Queue
	case proto.EventStats:
		s, err := decode[proto.Stats](ev.Data)
		if err != nil {
			return false, err
		}
		d.stats = s
	}
	return false, nil
}

func (d *data) trim() {
	var done []string
	for id, j := range d.jobs {
		if j.State.Terminal() {
			done = append(done, id)
		}
	}
	if len(done) <= maxFinished {
		return
	}
	sort.Strings(done)
	for _, id := range done[:len(done)-maxFinished] {
		delete(d.jobs, id)
	}
}

// decode accepts event data as the typed value (in-process clients), a
// pointer to it, raw JSON, or the generic map a JSON decoder produces.
func decode[T any](v any) (T, error) {
	var out T
	switch x := v.(type) {
	case T:
		return x, nil
	case *T:
		if x == nil {
			return out, fmt.Errorf("decode %T: nil", out)
		}
		return *x, nil
	case json.RawMessage:
		err := json.Unmarshal(x, &out)
		return out, err
	case []byte:
		err := json.Unmarshal(x, &out)
		return out, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return out, fmt.Errorf("decode %T: %w", out, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("decode %T: %w", out, err)
	}
	return out, nil
}

// nodeList returns nodes with this machine first, then by name, so rows
// keep their place as the data changes.
func (d *data) nodeList() []proto.NodeState {
	out := make([]proto.NodeState, 0, len(d.nodes))
	for _, n := range d.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Self != out[b].Self {
			return out[a].Self
		}
		return out[a].Info.Name < out[b].Info.Name
	})
	return out
}

func placed(s proto.JobState) bool {
	return s == proto.JobSyncing || s == proto.JobSetup || s == proto.JobRunning
}

// running returns placed jobs, oldest first.
func (d *data) running() []proto.Job {
	var out []proto.Job
	for _, j := range d.jobs {
		if placed(j.State) {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Spec.ID < out[b].Spec.ID })
	return out
}

// waiting returns queued jobs in dispatch order.
func (d *data) waiting() []proto.Job {
	pos := make(map[string]int, len(d.queue))
	for i, id := range d.queue {
		pos[id] = i
	}
	var out []proto.Job
	for _, j := range d.jobs {
		if j.State == proto.JobQueued {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		pa, oka := pos[out[a].Spec.ID]
		pb, okb := pos[out[b].Spec.ID]
		switch {
		case oka && okb:
			return pa < pb
		case oka != okb:
			return oka
		}
		return out[a].Spec.ID < out[b].Spec.ID
	})
	return out
}

func (d *data) jobsOn(node string) []proto.Job {
	var out []proto.Job
	for _, j := range d.jobs {
		if j.Node == node && placed(j.State) {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Spec.ID < out[b].Spec.ID })
	return out
}
