package mock

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestSimInvariants(t *testing.T) {
	c := &clock{time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)}
	s := New(Options{Seed: 9, Now: c.now})
	for i := 0; i < 1200; i++ { // ten simulated minutes
		c.t = c.t.Add(500 * time.Millisecond)
		s.Step()
	}
	st := s.State()
	queued := map[string]bool{}
	for _, j := range st.Jobs {
		if j.State == proto.JobQueued {
			queued[j.Spec.ID] = true
			if !strings.HasPrefix(j.Reason, "waiting") {
				t.Errorf("queued job %s has reason %q", j.Spec.ID, j.Reason)
			}
		}
		if j.State == proto.JobRunning && j.Node == "" {
			t.Errorf("running job %s has no node", j.Spec.ID)
		}
	}
	if len(st.Queue) != len(queued) {
		t.Errorf("queue has %d ids, %d jobs are queued", len(st.Queue), len(queued))
	}
	for _, id := range st.Queue {
		if !queued[id] {
			t.Errorf("queue lists %s which is not queued", id)
		}
	}
	for _, n := range st.Nodes {
		if n.Allocated.CPUs > n.Capacity.CPUs*1.25+16 {
			t.Errorf("%s: %.1f cores allocated of %.0f", n.Info.Name, n.Allocated.CPUs, n.Capacity.CPUs)
		}
		if n.Allocated.Mem > n.Capacity.Mem {
			t.Errorf("%s: memory overcommitted", n.Info.Name)
		}
	}
	if st.Stats.JobsToday == 0 {
		t.Error("no jobs finished")
	}
}

func TestEventsStreamStartsWithState(t *testing.T) {
	s := New(Options{Seed: 1})
	srv := httptest.NewServer(s.Handler(""))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev proto.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != proto.EventState {
			t.Fatalf("first event %q, want state", ev.Type)
		}
		return
	}
	t.Fatal("no event")
}

func TestTokenRequired(t *testing.T) {
	s := New(Options{Seed: 1})
	h := s.Handler("sekrit")
	job := s.State().Jobs[0].Spec.ID
	cases := []struct {
		method, path, header string
		code                 int
	}{
		{"GET", "/api/v1/state", "", 401},
		{"GET", "/api/v1/state?token=sekrit", "", 200},
		{"GET", "/api/v1/state", "sekrit", 200},
		{"POST", "/api/v1/jobs/" + job + "/cancel?token=sekrit", "", 401}, // mutations need the header
		{"POST", "/api/v1/nodes/devmac/drain", "wrong", 401},
		{"POST", "/api/v1/nodes/devmac/drain", "sekrit", 204},
		{"POST", "/api/v1/nodes/devmac/drain", "sekrit", 409},
		{"POST", "/api/v1/nodes/nope/resume", "sekrit", 404},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.header != "" {
			req.Header.Set("X-Kran-Token", c.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.code {
			t.Errorf("%s %s (%q): %d, want %d: %s", c.method, c.path, c.header, rec.Code, c.code, rec.Body)
		}
	}
}
