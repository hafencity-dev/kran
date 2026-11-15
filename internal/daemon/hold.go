package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// A restart of kran (kran service restart, kran upgrade) waits until no job
// is starting and no queued job could be lost. While it waits it asks the
// daemon to hold NEW jobs: they are accepted and named to their kran run,
// but join the queue only after the restart (the next daemon restores them
// as queued) or when the hold ends. So the queue drains instead of being
// refilled, and a restart does not need --force in ordinary use.
type holdState struct {
	mu     sync.Mutex
	until  time.Time
	reason string
	ch     chan struct{} // closed when the hold is released
}

// set starts (or extends) a hold until until.
func (h *holdState) set(until time.Time, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ch == nil {
		h.ch = make(chan struct{})
	}
	h.until, h.reason = until, reason
}

func (h *holdState) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ch != nil {
		close(h.ch)
		h.ch = nil
	}
	h.until = time.Time{}
}

// active returns the hold's release channel and end, if one holds at now.
func (h *holdState) active(now time.Time) (<-chan struct{}, time.Time, string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ch == nil || !now.Before(h.until) {
		return nil, time.Time{}, "", false
	}
	return h.ch, h.until, h.reason, true
}

// maxHold bounds a hold whatever a client asks: a restart that forgets to
// release it never keeps new jobs out for long.
const maxHold = 15 * time.Minute

// waitHold keeps a new job out of the queue while a hold is active. It
// returns false when the job ended meanwhile (canceled, or the daemon
// stops: then the job stays queued in the store and the next daemon
// restores it).
func (d *Daemon) waitHold(ctx context.Context, e *jobEntry) bool {
	ch, until, why, ok := d.hold.active(d.now())
	if !ok {
		return true
	}
	msg := "waiting: " + why + "; this job joins the queue after it (at the latest at " + until.Local().Format("15:04:05") + ")"
	e.update(func(j *proto.Job) { j.Held, j.Reason = true, msg })
	e.notice("queued", msg)
	timer := time.NewTimer(until.Sub(d.now()))
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-e.cancelCh:
		d.finishCanceledBeforeStart(e)
		return false
	case <-ctx.Done():
		return false
	}
	e.update(func(j *proto.Job) { j.Held = false })
	return true
}

func (d *Daemon) handleHold(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		d.hold.release()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		Seconds int    `json:"seconds"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Seconds <= 0 {
		writeAPIError(w, http.StatusBadRequest, "seconds must be positive", "")
		return
	}
	dur := min(time.Duration(body.Seconds)*time.Second, maxHold)
	why := body.Reason
	if why == "" {
		why = "a restart of kran is pending"
	}
	until := d.now().Add(dur)
	d.hold.set(until, why)
	d.logf("holding new jobs until %s: %s", until.Format(time.RFC3339), why)
	writeJSON(w, http.StatusOK, map[string]string{"until": until.UTC().Format(time.RFC3339)})
}
