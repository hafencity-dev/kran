package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; index i brings the schema to version i+1.
// Never edit an applied migration: append a new one.
var migrations = []string{
	// 1: jobs, estimate samples, key/value.
	`
CREATE TABLE jobs (
	id          TEXT PRIMARY KEY,
	state       TEXT NOT NULL,
	active      INTEGER NOT NULL,          -- 1 while not terminal
	owner_id    TEXT NOT NULL DEFAULT '',
	owner_name  TEXT NOT NULL DEFAULT '',
	node        TEXT NOT NULL DEFAULT '',
	origin      TEXT NOT NULL DEFAULT '',
	fingerprint TEXT NOT NULL DEFAULT '',
	queued_at   INTEGER NOT NULL DEFAULT 0, -- unix ns
	ended_at    INTEGER NOT NULL DEFAULT 0, -- unix ns, 0 while active
	data        BLOB NOT NULL               -- proto.Job JSON
);
CREATE INDEX jobs_queued   ON jobs (queued_at DESC, id DESC);
CREATE INDEX jobs_owner    ON jobs (owner_id, queued_at DESC);
CREATE INDEX jobs_node     ON jobs (node, queued_at DESC);
CREATE INDEX jobs_state    ON jobs (state, queued_at DESC);
CREATE INDEX jobs_active   ON jobs (active) WHERE active = 1;
CREATE INDEX jobs_ended    ON jobs (ended_at) WHERE active = 0;

CREATE TABLE samples (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	fingerprint  TEXT NOT NULL,
	node         TEXT NOT NULL,
	origin       TEXT NOT NULL,
	family       TEXT NOT NULL,
	cpu_seconds  REAL NOT NULL,
	peak_mem     INTEGER NOT NULL,
	wall_seconds REAL NOT NULL,
	at           INTEGER NOT NULL           -- unix ns
);
CREATE INDEX samples_fp ON samples (fingerprint, at DESC, id DESC);
CREATE INDEX samples_at ON samples (at);

CREATE TABLE kv (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
`,
}

// migrate brings the database to the latest schema version. Each migration
// runs in its own transaction together with the version bump, so a crash
// mid-way leaves the previous version intact.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	for {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration: %w", err)
		}
		var v int
		err = tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
				tx.Rollback()
				return fmt.Errorf("init schema_version: %w", err)
			}
		case err != nil:
			tx.Rollback()
			return fmt.Errorf("read schema version: %w", err)
		}
		if v > len(migrations) {
			tx.Rollback()
			return fmt.Errorf("database schema version %d is newer than this kran (knows %d); upgrade kran", v, len(migrations))
		}
		if v == len(migrations) {
			return tx.Rollback()
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, v+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("bump schema version to %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", v+1, err)
		}
	}
}
