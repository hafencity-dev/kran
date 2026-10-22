package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/store"
	"github.com/hafencity-dev/kran/internal/version"
)

// Handler returns the local API as served on the unix socket (full trust).
func (d *Daemon) Handler() http.Handler { return d.unixHandler }

// routes builds the API. unix=false is the TCP (UI) listener: no run
// sessions, token rules from SPEC §9 applied by tcpAuth.
func (d *Daemon) routes(unix bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/version", d.handleVersion)
	mux.HandleFunc("GET /api/v1/state", d.handleState)
	mux.HandleFunc("GET /api/v1/events", d.handleEvents)
	mux.HandleFunc("GET /api/v1/jobs", d.handleJobs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", d.handleJob)
	mux.HandleFunc("GET /api/v1/jobs/{id}/log", d.handleLog)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", d.handleCancel)
	mux.HandleFunc("POST /api/v1/jobs/{id}/priority", d.handlePriority)
	mux.HandleFunc("GET /api/v1/nodes/{name}/metrics", d.handleMetricsHistory)
	mux.HandleFunc("POST /api/v1/nodes/{name}/drain", d.handleDrain(true))
	mux.HandleFunc("POST /api/v1/nodes/{name}/resume", d.handleDrain(false))
	if unix && d.deps.Debug {
		mux.HandleFunc("GET /api/v1/debug/goroutines", handleGoroutines)
		mux.HandleFunc("GET /api/v1/debug/slots", d.handleSlots)
	}

	mux.HandleFunc("GET /api/v1/daemon", d.handleDaemonInfo)
	if unix {
		mux.HandleFunc("POST /api/v1/daemon/stop", d.handleDaemonStop)
		mux.HandleFunc("POST /api/v1/run", d.handleRun)
	} else {
		mux.HandleFunc("POST /api/v1/run", func(w http.ResponseWriter, r *http.Request) {
			writeAPIError(w, http.StatusForbidden, "run sessions are only served on the unix socket", "use kran run on this machine")
		})
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, "no such API endpoint: "+r.Method+" "+r.URL.Path, "check kran version on both sides")
	})
	if d.deps.UI != nil {
		mux.Handle("/", d.deps.UI)
	} else {
		mux.Handle("/", http.NotFoundHandler())
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, msg, hint string) {
	writeJSON(w, status, proto.APIError{Error: msg, Hint: hint})
}

// --- auth (SPEC §9) ---

func (d *Daemon) loadToken() error {
	path := d.deps.Dirs.tokenFile()
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); len(tok) >= 32 {
			d.token = tok
			os.Chmod(path, 0o600)
			return nil
		}
	}
	tok := randomHex(32)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	os.Remove(tmp) // WriteFile keeps the mode of a leftover file
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return fmt.Errorf("write UI token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write UI token: %w", err)
	}
	d.token = tok
	return nil
}

// Token is the UI/API token (for `kran ui --print-url`).
func (d *Daemon) Token() string { return d.token }

func (d *Daemon) tokenOK(r *http.Request, allowQuery bool) bool {
	got := r.Header.Get("X-Kran-Token")
	if got == "" && allowQuery {
		got = r.URL.Query().Get("token")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(d.token)) == 1
}

// tcpAuth guards the UI listener. Every API call needs the token (header,
// or ?token= for GETs such as SSE; mutations only the header), loopback
// included: loopback TCP is open to every account on the machine, and job
// output and argv are the owner's. Static UI assets stay public because
// they carry no data. On loopback the Host header must be a loopback name
// (DNS rebinding).
func (d *Daemon) tcpAuth(next http.Handler, loopback bool, bindHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No cross-origin embedding (<script src=…/log>) of API output.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if loopback && !loopbackHost(r.Host) {
			writeAPIError(w, http.StatusForbidden, "unexpected Host header", "open the UI through 127.0.0.1 or localhost")
			return
		}
		if !loopback && !hostMatches(r.Host, bindHost) && !d.tokenOK(r, true) {
			writeAPIError(w, http.StatusForbidden, "unexpected Host header", "open the UI with kran ui --print-url")
			return
		}
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		switch {
		case !read && !d.tokenOK(r, false):
			writeAPIError(w, http.StatusUnauthorized, "this action needs the kran token (header X-Kran-Token)", "open the UI with kran ui")
			return
		case read && isAPI && !d.tokenOK(r, true):
			writeAPIError(w, http.StatusUnauthorized, "this listener needs the kran token", "open the UI with kran ui --print-url")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostMatches accepts the bind address (any host for a wildcard bind).
func hostMatches(hostport, bindHost string) bool {
	if bindHost == "" || bindHost == "0.0.0.0" || bindHost == "::" {
		return true
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.EqualFold(strings.Trim(host, "[]"), strings.Trim(bindHost, "[]"))
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// --- handlers ---

func (d *Daemon) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, version.Info())
}

func (d *Daemon) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.State())
}

func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming unsupported", "")
		return
	}
	sub, seq := d.bus.subscribe()
	defer d.bus.unsubscribe(sub)
	st := d.State()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	write := func(ev proto.Event) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	if write(proto.Event{Type: proto.EventState, Seq: seq, Time: d.now().UTC(), Data: st}) != nil {
		return
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-sub.ch:
			if !ok {
				return // dropped as a slow consumer, or shutdown: client resyncs
			}
			if ev.Seq <= seq {
				continue
			}
			if write(ev) != nil {
				return
			}
		case <-ping.C:
			rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (d *Daemon) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := store.JobQuery{Limit: 50, Owner: r.URL.Query().Get("owner"), Node: r.URL.Query().Get("node"),
		State: proto.JobState(r.URL.Query().Get("state"))}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeAPIError(w, http.StatusBadRequest, "limit must be a positive number", "")
			return
		}
		q.Limit = n
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	jobs, err := d.deps.Store.ListJobs(ctx, q)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list jobs: "+err.Error(), "check kran doctor")
		return
	}
	// Live state wins over what was last persisted.
	for i := range jobs {
		if e := d.jobs.get(jobs[i].Spec.ID); e != nil {
			jobs[i] = e.snapshot()
		}
	}
	if jobs == nil {
		jobs = []proto.Job{}
	}
	for i := range jobs {
		jobs[i] = publicJob(jobs[i])
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (d *Daemon) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, ok := d.jobs.find(id)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "no job "+id, "list jobs: kran jobs")
		return
	}
	writeJSON(w, http.StatusOK, publicJob(j))
}

func (d *Daemon) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	e := d.jobs.get(id)
	if e == nil {
		if _, ok := d.jobs.find(id); ok {
			writeAPIError(w, http.StatusConflict, "job "+id+" already ended", "")
			return
		}
		writeAPIError(w, http.StatusNotFound, "no job "+id, "list jobs: kran jobs")
		return
	}
	e.requestCancel("canceled with kran cancel", "TERM")
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handlePriority(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Priority proto.Priority `json:"priority"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "body must be {\"priority\": \"high|normal|low\"}", "")
		return
	}
	switch body.Priority {
	case proto.PriorityHigh, proto.PriorityNormal, proto.PriorityLow:
	default:
		writeAPIError(w, http.StatusBadRequest, "priority must be high, normal or low", "")
		return
	}
	e := d.jobs.get(id)
	if e == nil {
		writeAPIError(w, http.StatusNotFound, "no active job "+id, "list jobs: kran jobs")
		return
	}
	e.update(func(j *proto.Job) { j.Spec.Priority = body.Priority })
	d.deps.Sched.SetPriority(id, body.Priority)
	d.publishQueue()
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleDrain(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !d.peers.setDrained(name, on) {
			writeAPIError(w, http.StatusNotFound, "no machine named "+name, "machine names are in `kran status`")
			return
		}
		if name == d.self {
			d.publishSelf()
			d.deps.Sched.Kick()
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleLog serves a job's output: raw bytes, or SSE ("output" events with
// proto.OutputChunk, then "exit") when follow=1.
func (d *Daemon) handleLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeAPIError(w, http.StatusBadRequest, "invalid job id", "")
		return
	}
	_, ok := d.jobs.find(id)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "no job "+id, "list jobs: kran jobs")
		return
	}
	var offset int64
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeAPIError(w, http.StatusBadRequest, "offset must be a non-negative number", "")
			return
		}
		offset = n
	}
	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
	var sp *spool
	if e := d.jobs.get(id); e != nil {
		sp = e.spool
	} else {
		path := filepath.Join(d.deps.Dirs.spoolDir(), id)
		if _, err := os.Stat(path); err != nil {
			writeAPIError(w, http.StatusNotFound, "the output of job "+id+" is no longer kept", "output is kept for 7 days")
			return
		}
		var err error
		if sp, err = openSpool(path); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "open output: "+err.Error(), "")
			return
		}
		sp.finish()
		defer sp.close()
	}
	rd, err := sp.reader(offset, true)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "open output: "+err.Error(), "")
		return
	}
	defer rd.Close()
	ctx := r.Context()
	if !follow {
		// Only what exists now.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for {
			t, p, err := rd.NextNoWait()
			if err != nil {
				return
			}
			if isOutput(t) {
				if _, err := w.Write(p); err != nil {
					return
				}
			}
		}
	}
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Unnamed messages carry proto.OutputChunk; "data" never splits a UTF-8
	// sequence (incomplete tails wait for the next chunk), "b64" is exact.
	pos := offset
	var pending []byte
	stream := "stdout"
	emit := func(final bool) bool {
		n := len(pending)
		if !final {
			n = utf8Complete(pending)
		}
		if n == 0 {
			return true
		}
		b := pending[:n]
		pos += int64(n)
		msg, _ := json.Marshal(proto.OutputChunk{Offset: pos, Data: string(b), Stream: stream, B64: base64.StdEncoding.EncodeToString(b)})
		pending = append(pending[:0], pending[n:]...)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
			return false
		}
		return true
	}
	end := func(exit json.RawMessage) {
		emit(true)
		b, _ := json.Marshal(struct {
			Offset int64           `json:"offset"`
			Exit   json.RawMessage `json:"exit,omitempty"`
		}{pos, exit})
		fmt.Fprintf(w, "event: end\ndata: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	for {
		t, p, err := rd.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				end(nil)
			}
			return
		}
		switch {
		case isOutput(t):
			s := "stdout"
			if t == proto.FrameStderr {
				s = "stderr"
			}
			if s != stream && !emit(true) {
				return
			}
			stream = s
			pending = append(pending, p...)
			if !emit(false) {
				return
			}
		case t == proto.FrameExit:
			end(append(json.RawMessage(nil), p...))
			return
		default:
			continue
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

// utf8Complete returns the length of b without an incomplete trailing
// UTF-8 sequence (at most 3 bytes are held back).
func utf8Complete(b []byte) int {
	n := len(b)
	for i := 1; i <= 3 && i <= n; i++ {
		c := b[n-i]
		if c < 0x80 || c >= 0xF8 {
			return n // ASCII or an invalid byte: nothing to wait for
		}
		if c >= 0xC0 { // lead byte: complete if the sequence fits
			need := 2
			if c >= 0xF0 {
				need = 4
			} else if c >= 0xE0 {
				need = 3
			}
			if i >= need {
				return n
			}
			return n - i
		}
	}
	return n
}

func (d *Daemon) handleDaemonInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, proto.DaemonInfo{PID: os.Getpid(), Supervisor: d.deps.Supervisor, StartedAt: d.startedAt.UTC()})
}

// handleDaemonStop lets `kran service` replace a daemon its service manager
// does not run: stop now ("now", graceful: jobs are restored by the next
// daemon) or once no job is active here ("idle").
func (d *Daemon) handleDaemonStop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		When string `json:"when"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	switch body.When {
	case "", "idle":
		d.goLoop(d.stopWhenIdle)
	case "now":
		d.requestStop()
	default:
		writeAPIError(w, http.StatusBadRequest, `when must be "idle" or "now"`, "")
		return
	}
	writeJSON(w, http.StatusAccepted, proto.DaemonInfo{PID: os.Getpid(), Supervisor: d.deps.Supervisor, StartedAt: d.startedAt.UTC()})
}

func (d *Daemon) requestStop() { d.stopOnce.Do(func() { close(d.stopReq) }) }

// stopWhenIdle stops the daemon once it originates no active job and holds
// no reservation for a job that has not started (running guest jobs survive
// under their supervisors and are picked up by the next daemon).
func (d *Daemon) stopWhenIdle(ctx context.Context) {
	tk := time.NewTicker(250 * time.Millisecond)
	defer tk.Stop()
	for {
		if len(d.jobs.list()) == 0 && d.target.reservations() == 0 {
			d.requestStop()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}
