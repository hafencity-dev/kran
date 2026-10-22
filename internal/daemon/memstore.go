package daemon

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/store"
)

// memStore is an in-memory store.Store. The daemon falls back to it when
// the database cannot be opened (history is then lost on restart, but jobs
// still run); tests use it directly.
type memStore struct {
	mu      sync.Mutex
	jobs    map[string]proto.Job
	samples []store.Sample
	kv      map[string]string
}

func newMemStore() *memStore {
	return &memStore{jobs: map[string]proto.Job{}, kv: map[string]string{}}
}

var errNotFound = errors.New("not found")

func (m *memStore) PutJob(ctx context.Context, j *proto.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.Spec.ID] = cloneJob(*j)
	return nil
}

func (m *memStore) GetJob(ctx context.Context, id string) (*proto.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, errNotFound
	}
	c := cloneJob(j)
	return &c, nil
}

func (m *memStore) ListJobs(ctx context.Context, q store.JobQuery) ([]proto.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []proto.Job
	for _, j := range m.jobs {
		if q.Owner != "" && j.Spec.Owner.ID != q.Owner && j.Spec.Owner.Name != q.Owner {
			continue
		}
		if q.Node != "" && j.Node != q.Node {
			continue
		}
		if !q.Since.IsZero() && j.QueuedAt.Before(q.Since) {
			continue
		}
		if q.State != "" && j.State != q.State {
			continue
		}
		out = append(out, cloneJob(j))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Spec.ID > out[b].Spec.ID })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (m *memStore) ActiveJobs(ctx context.Context) ([]proto.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []proto.Job
	for _, j := range m.jobs {
		if !j.State.Terminal() {
			out = append(out, cloneJob(j))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Spec.ID < out[b].Spec.ID })
	return out, nil
}

func (m *memStore) PruneJobs(ctx context.Context, olderThan time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, j := range m.jobs {
		if j.State.Terminal() && j.EndedAt.Before(olderThan) {
			delete(m.jobs, id)
			n++
		}
	}
	return n, nil
}

func (m *memStore) AddSample(ctx context.Context, s store.Sample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samples = append(m.samples, s)
	return nil
}

func (m *memStore) Samples(ctx context.Context, fp string, limit int) ([]store.Sample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Sample
	for i := len(m.samples) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		if m.samples[i].Fingerprint == fp {
			out = append(out, m.samples[i])
		}
	}
	return out, nil
}

func (m *memStore) Get(ctx context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.kv[key]
	return v, ok, nil
}

func (m *memStore) Set(ctx context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kv[key] = value
	return nil
}

func (m *memStore) DB() *sql.DB  { return nil }
func (m *memStore) Close() error { return nil }

// cloneJob deep-copies the maps and pointers of a job so readers never
// share memory with the daemon's live copy.
func cloneJob(j proto.Job) proto.Job {
	j.Spec.Argv = append([]string(nil), j.Spec.Argv...)
	j.Spec.Env = cloneMap(j.Spec.Env)
	j.Spec.Labels = cloneMap(j.Spec.Labels)
	j.Spec.Req.Toolchains = cloneMap(j.Spec.Req.Toolchains)
	if j.Spec.TTY != nil {
		t := *j.Spec.TTY
		j.Spec.TTY = &t
	}
	if j.Spec.OriginEnv != nil {
		o := *j.Spec.OriginEnv
		o.Toolchains = cloneMap(o.Toolchains)
		j.Spec.OriginEnv = &o
	}
	if j.ExitCode != nil {
		c := *j.ExitCode
		j.ExitCode = &c
	}
	if j.Live != nil {
		l := *j.Live
		j.Live = &l
	}
	return j
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
