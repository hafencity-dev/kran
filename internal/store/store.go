// Package store is the daemon's sqlite database (modernc.org/sqlite, WAL):
// job history, estimate samples, active remote jobs (for reattach), small
// key/value state. Other packages may create their own tables through Exec
// with a package prefix (e.g. workspace hash cache: "ws_hash").
package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

type Store interface {
	// Jobs
	PutJob(ctx context.Context, j *proto.Job) error // upsert by ID
	GetJob(ctx context.Context, id string) (*proto.Job, error)
	ListJobs(ctx context.Context, q JobQuery) ([]proto.Job, error)
	// ActiveJobs returns non-terminal jobs (for reattach after restart).
	ActiveJobs(ctx context.Context) ([]proto.Job, error)
	PruneJobs(ctx context.Context, olderThan time.Time) (int, error)

	// Estimate samples
	AddSample(ctx context.Context, s Sample) error
	Samples(ctx context.Context, fingerprint string, limit int) ([]Sample, error)

	// KV
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string) error

	// DB exposes the handle for package-owned tables.
	DB() *sql.DB
	Close() error
}

type JobQuery struct {
	Limit int
	Owner string
	Node  string
	Since time.Time
	State proto.JobState
}

type Sample struct {
	Fingerprint string
	Node        string
	Origin      string
	Family      string
	CPUSeconds  float64
	PeakMem     int64
	WallSeconds float64
	At          time.Time
}

// Open opens (and migrates) the database at path.
func Open(path string) (Store, error) { return open(path) }
