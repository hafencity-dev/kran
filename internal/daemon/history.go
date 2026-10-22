package daemon

import (
	"net/http"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// metricsHistory keeps the last 10 minutes of metrics per node (2 s
// samples) so a freshly opened UI or kran top can draw trends at once.
type metricsHistory struct {
	mu    sync.Mutex
	nodes map[string][]proto.NodeMetrics
}

const historyLen = 300

func (h *metricsHistory) add(node string, m proto.NodeMetrics) {
	if m.Time.IsZero() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nodes == nil {
		h.nodes = map[string][]proto.NodeMetrics{}
	}
	s := h.nodes[node]
	if n := len(s); n > 0 && m.Time.Sub(s[n-1].Time) < 1500*time.Millisecond {
		return
	}
	s = append(s, m)
	if len(s) > historyLen {
		s = append(s[:0:0], s[len(s)-historyLen:]...)
	}
	h.nodes[node] = s
}

func (h *metricsHistory) since(node string, t time.Time) ([]proto.NodeMetrics, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.nodes[node]
	out := []proto.NodeMetrics{}
	for _, m := range s {
		if m.Time.After(t) {
			out = append(out, m)
		}
	}
	return out, ok
}

func (h *metricsHistory) remove(node string) {
	h.mu.Lock()
	delete(h.nodes, node)
	h.mu.Unlock()
}

// handleMetricsHistory: GET /api/v1/nodes/{name}/metrics?since=<RFC 3339>.
func (d *Daemon) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "since must be an RFC 3339 time", "")
			return
		}
		since = t
	}
	out, ok := d.history.since(name, since)
	if !ok && name != d.self && d.peers.get(name) == nil {
		writeAPIError(w, http.StatusNotFound, "no machine named "+name, "machine names are in `kran status`")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
