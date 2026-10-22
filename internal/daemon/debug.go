package daemon

import (
	"net/http"
	"runtime/pprof"
)

// Diagnostics, only on the unix socket and only with Deps.Debug
// (KRAN_DEBUG=1). The chaos suite uses them to find goroutines and slots
// that outlive their jobs and connections.

// handleGoroutines serves the goroutine profile (pprof debug=1 text:
// "goroutine profile: total N" and stacks grouped with counts).
func handleGoroutines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	pprof.Lookup("goroutine").WriteTo(w, 1)
}

// handleSlots lists the busy workspace slots of this node as a target.
func (d *Daemon) handleSlots(w http.ResponseWriter, r *http.Request) {
	busy := []string{}
	if s, ok := d.deps.Exec.(interface{ BusySlots() []string }); ok {
		busy = append(busy, s.BusySlots()...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"busy": busy})
}
