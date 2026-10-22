package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/hafencity-dev/kran/internal/proto"
)

// ErrNotFound is returned by GetJob for an unknown ID.
var ErrNotFound = errors.New("store: not found")

// Tuning knobs, exported for the daemon's pruning loop and tests.
const (
	// DefaultListLimit applies when JobQuery.Limit is 0.
	DefaultListLimit = 100
	// MaxSamplesPerFingerprint bounds the samples table per fingerprint;
	// the estimator only reads the most recent 20.
	MaxSamplesPerFingerprint = 50
	// keepSamplesOnPrune survive PruneJobs even when old, so a rarely run
	// job keeps its learned estimate.
	keepSamplesOnPrune = 20
)

type sqliteStore struct {
	db *sql.DB
	// wmu serializes this process's writes: sqlite allows one writer, and
	// queueing in Go is cheaper and fairer than sqlite's busy-sleep loop.
	// Readers never take it (WAL lets them run concurrently).
	wmu sync.Mutex
}

func open(path string) (Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("store: database path %q must not contain '?'", path)
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create database dir: %w", err)
		}
		// Create with 0600 before sqlite does (it would use the umask);
		// sqlite gives -wal and -shm the main file's permissions.
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open database %s: %w", path, err)
		}
		f.Close()
	}
	dsn := path + "?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if path == ":memory:" {
		// Every pooled connection would otherwise get its own empty database.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
	}
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) DB() *sql.DB  { return s.db }
func (s *sqliteStore) Close() error { return s.db.Close() }

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// ---------------------------------------------------------------------------
// Jobs

func (s *sqliteStore) PutJob(ctx context.Context, j *proto.Job) error {
	if j == nil || j.Spec.ID == "" {
		return errors.New("store: job without ID")
	}
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("encode job %s: %w", j.Spec.ID, err)
	}
	active := 1
	if j.State.Terminal() {
		active = 0
	}
	ended := unixNano(j.EndedAt)
	if active == 0 && ended == 0 {
		// Pruning keys on ended_at; a terminal job without one would live forever.
		ended = time.Now().UnixNano()
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO jobs (id, state, active, owner_id, owner_name, node, origin, fingerprint, queued_at, ended_at, data)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
	state = excluded.state, active = excluded.active,
	owner_id = excluded.owner_id, owner_name = excluded.owner_name,
	node = excluded.node, origin = excluded.origin,
	fingerprint = excluded.fingerprint, queued_at = excluded.queued_at,
	ended_at = excluded.ended_at, data = excluded.data`,
		j.Spec.ID, string(j.State), active, j.Spec.Owner.ID, j.Spec.Owner.Name,
		j.Node, j.Origin, j.Fingerprint, unixNano(j.QueuedAt), ended, data)
	if err != nil {
		return fmt.Errorf("store job %s: %w", j.Spec.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetJob(ctx context.Context, id string) (*proto.Job, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM jobs WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("job %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load job %s: %w", id, err)
	}
	var j proto.Job
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("decode job %s: %w", id, err)
	}
	return &j, nil
}

// ListJobs returns jobs newest first. Owner matches the owner ID or display
// name; Node matches the target node or, for jobs never placed, the origin.
func (s *sqliteStore) ListJobs(ctx context.Context, q JobQuery) ([]proto.Job, error) {
	var where []string
	var args []any
	if q.Owner != "" {
		where = append(where, `(owner_id = ? OR owner_name = ?)`)
		args = append(args, q.Owner, q.Owner)
	}
	if q.Node != "" {
		where = append(where, `(node = ? OR (node = '' AND origin = ?))`)
		args = append(args, q.Node, q.Node)
	}
	if !q.Since.IsZero() {
		where = append(where, `queued_at >= ?`)
		args = append(args, q.Since.UnixNano())
	}
	if q.State != "" {
		where = append(where, `state = ?`)
		args = append(args, string(q.State))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	query := `SELECT data FROM jobs`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY queued_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return s.queryJobs(ctx, query, args...)
}

func (s *sqliteStore) ActiveJobs(ctx context.Context) ([]proto.Job, error) {
	return s.queryJobs(ctx, `SELECT data FROM jobs WHERE active = 1 ORDER BY queued_at, id`)
}

func (s *sqliteStore) queryJobs(ctx context.Context, query string, args ...any) ([]proto.Job, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query jobs: %w", err)
	}
	defer rows.Close()
	var out []proto.Job
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		var j proto.Job
		if err := json.Unmarshal(data, &j); err != nil {
			// One corrupt row must not hide the rest of the history.
			continue
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query jobs: %w", err)
	}
	return out, nil
}

// PruneJobs deletes finished jobs that ended before olderThan, and estimate
// samples older than that beyond the newest few per fingerprint. Active jobs
// are never pruned.
func (s *sqliteStore) PruneJobs(ctx context.Context, olderThan time.Time) (int, error) {
	cut := olderThan.UnixNano()
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE active = 0 AND ended_at < ?`, cut)
	if err != nil {
		return 0, fmt.Errorf("prune jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	_, err = s.db.ExecContext(ctx, `
DELETE FROM samples WHERE at < ? AND id NOT IN (
	SELECT s2.id FROM samples s2 WHERE s2.fingerprint = samples.fingerprint
	ORDER BY s2.at DESC, s2.id DESC LIMIT ?)`, cut, keepSamplesOnPrune)
	if err != nil {
		return int(n), fmt.Errorf("prune samples: %w", err)
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Samples

func (s *sqliteStore) AddSample(ctx context.Context, sm Sample) error {
	if sm.Fingerprint == "" {
		return errors.New("store: sample without fingerprint")
	}
	at := sm.At
	if at.IsZero() {
		at = time.Now()
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("add sample: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO samples (fingerprint, node, origin, family, cpu_seconds, peak_mem, wall_seconds, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sm.Fingerprint, sm.Node, sm.Origin, sm.Family, sm.CPUSeconds, sm.PeakMem, sm.WallSeconds, at.UnixNano()); err != nil {
		return fmt.Errorf("add sample: %w", err)
	}
	// Bound per-fingerprint history so hot jobs do not grow the table.
	if _, err := tx.ExecContext(ctx, `
DELETE FROM samples WHERE fingerprint = ? AND id NOT IN (
	SELECT id FROM samples WHERE fingerprint = ? ORDER BY at DESC, id DESC LIMIT ?)`,
		sm.Fingerprint, sm.Fingerprint, MaxSamplesPerFingerprint); err != nil {
		return fmt.Errorf("trim samples: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("add sample: %w", err)
	}
	return nil
}

// Samples returns the newest samples for fingerprint, newest first.
func (s *sqliteStore) Samples(ctx context.Context, fingerprint string, limit int) ([]Sample, error) {
	if limit <= 0 {
		limit = MaxSamplesPerFingerprint
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT fingerprint, node, origin, family, cpu_seconds, peak_mem, wall_seconds, at
FROM samples WHERE fingerprint = ? ORDER BY at DESC, id DESC LIMIT ?`, fingerprint, limit)
	if err != nil {
		return nil, fmt.Errorf("query samples: %w", err)
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var sm Sample
		var at int64
		if err := rows.Scan(&sm.Fingerprint, &sm.Node, &sm.Origin, &sm.Family, &sm.CPUSeconds, &sm.PeakMem, &sm.WallSeconds, &at); err != nil {
			return nil, fmt.Errorf("scan sample: %w", err)
		}
		sm.At = fromUnixNano(at)
		out = append(out, sm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query samples: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// KV

func (s *sqliteStore) Get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get %s: %w", key, err)
	}
	return v, true, nil
}

func (s *sqliteStore) Set(ctx context.Context, key, value string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	return nil
}
