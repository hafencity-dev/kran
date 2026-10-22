package mock

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Handler serves the local API subset the UI uses (SPEC §8.1) from the
// simulation. With a non-empty token, mutations require X-Kran-Token and
// every request must carry the token (header or ?token=), mirroring a UI
// bound to a non-loopback address.
func (s *Sim) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.State().Version)
	})
	mux.HandleFunc("GET /api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.State())
	})
	mux.HandleFunc("GET /api/v1/events", s.serveEvents)
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 200
		}
		writeJSON(w, http.StatusOK, s.Jobs(limit, q.Get("owner"), q.Get("node"), q.Get("state")))
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, err := s.Job(r.PathValue("id"))
		if err != nil {
			writeErr(w, err, "job "+r.PathValue("id"))
			return
		}
		writeJSON(w, http.StatusOK, j)
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/log", s.serveLog)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Cancel(r.PathValue("id")); err != nil {
			writeErr(w, err, "job "+r.PathValue("id"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/jobs/{id}/priority", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Priority proto.Priority `json:"priority"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, proto.APIError{Error: "body must be JSON {\"priority\":\"high|normal|low\"}"})
			return
		}
		if err := s.SetPriority(r.PathValue("id"), body.Priority); err != nil {
			writeErr(w, err, "job "+r.PathValue("id"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/nodes/{name}/drain", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Drain(r.PathValue("name")); err != nil {
			writeErr(w, err, "node "+r.PathValue("name"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/nodes/{name}/resume", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Resume(r.PathValue("name")); err != nil {
			writeErr(w, err, "node "+r.PathValue("name"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if token == "" {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Kran-Token")
		if got == "" && r.Method == http.MethodGet {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, proto.APIError{Error: "this dashboard needs a token", Hint: "run kran ui --print-url on the node and open the printed link"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, proto.APIError{Error: what + " not found", Hint: "it may have aged out of history"})
	case errors.Is(err, ErrConflict):
		writeJSON(w, http.StatusConflict, proto.APIError{Error: what + " cannot do that in its current state", Hint: "reload to see its current state"})
	default:
		writeJSON(w, http.StatusInternalServerError, proto.APIError{Error: err.Error()})
	}
}

func sseHeaders(w http.ResponseWriter) (http.Flusher, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return f, true
}

// serveEvents writes one SSE message per proto.Event: "id: <seq>" and
// "data: <Event JSON>" (no event name, so EventSource.onmessage sees all).
func (s *Sim) serveEvents(w http.ResponseWriter, r *http.Request) {
	f, ok := sseHeaders(w)
	if !ok {
		return
	}
	ch, cancel := s.Subscribe()
	defer cancel()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	fmt.Fprint(w, "retry: 2000\n\n")
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			f.Flush()
		case ev, ok := <-ch:
			if !ok {
				return // too slow; the client reconnects and resyncs
			}
			b, err := json.Marshal(ev)
			if err != nil {
				return
			}
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
			// Drain whatever is already buffered before flushing so a burst
			// costs one write.
			for drained := false; !drained; {
				select {
				case ev, ok := <-ch:
					if !ok {
						f.Flush()
						return
					}
					b, _ := json.Marshal(ev)
					fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
				default:
					drained = true
				}
			}
			f.Flush()
		}
	}
}

// LogChunkMsg is one SSE message of a followed log.
type LogChunkMsg struct {
	Offset int    `json:"offset"` // offset after this chunk
	Data   string `json:"data"`
}

// serveLog returns output as text/plain, or as SSE with follow=1:
// unnamed messages carry {"offset","data"}; a final "end" event carries
// {"offset"} once the job's output is complete.
func (s *Sim) serveLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if r.URL.Query().Get("follow") != "1" {
		chunk, _, _, err := s.LogChunk(id, offset)
		if err != nil {
			writeErr(w, err, "job "+id)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(chunk)
		return
	}
	if _, _, _, err := s.LogChunk(id, offset); err != nil {
		writeErr(w, err, "job "+id)
		return
	}
	f, ok := sseHeaders(w)
	if !ok {
		return
	}
	for {
		chunk, done, wait, err := s.LogChunk(id, offset)
		if err != nil {
			return
		}
		if len(chunk) > 0 {
			offset += len(chunk)
			b, _ := json.Marshal(LogChunkMsg{Offset: offset, Data: string(chunk)})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if done {
			fmt.Fprintf(w, "event: end\ndata: {\"offset\":%d}\n\n", offset)
			f.Flush()
			return
		}
		f.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-wait:
		case <-time.After(15 * time.Second):
			fmt.Fprint(w, ": ping\n\n")
		}
	}
}
