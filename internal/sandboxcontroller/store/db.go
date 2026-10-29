// Package store persists the SandboxRun records in SQLite.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the pure Go "sqlite" driver
)

// DatabaseFile is the name of the SQLite file inside the data directory.
const DatabaseFile = "sandbox-controller.db"

// migrations are applied in order; the index plus one is the schema version.
// Never edit an applied entry, append a new one.
var migrations = []string{
	`CREATE TABLE runs (
		id                 TEXT PRIMARY KEY,
		seq                INTEGER NOT NULL,
		spec               TEXT NOT NULL,
		status             TEXT NOT NULL,
		phase              TEXT NOT NULL,
		deletion_requested INTEGER NOT NULL DEFAULT 0
	);
	CREATE UNIQUE INDEX runs_seq ON runs (seq);
	CREATE INDEX runs_phase ON runs (phase);`,
}

// Open opens the database in dir, creating the directory, the file and the
// schema when needed. The connection uses WAL mode and a busy timeout.
func Open(ctx context.Context, dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create data dir: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := (&url.URL{Scheme: "file", Opaque: filepath.ToSlash(filepath.Join(dir, DatabaseFile)), RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: create schema_version: %w", err)
	}
	var current int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	for i := current; i < len(migrations); i++ {
		if err := applyMigration(ctx, db, i+1, migrations[i]); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, version int, statement string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %d: %w", version, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("store: migration %d: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, version); err != nil {
		return fmt.Errorf("store: record migration %d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %d: %w", version, err)
	}
	return nil
}
