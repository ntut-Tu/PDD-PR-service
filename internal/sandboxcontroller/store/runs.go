package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"pdd/services/internal/sandboxcontroller/model"
)

// ErrNotFound is returned when a run does not exist.
var ErrNotFound = errors.New("store: run not found")

// Runs stores SandboxRun records. The spec is written once; only the status and the deletion
// flag change afterwards, each through its own statement so that the API and the reconcile
// loop never overwrite each other.
type Runs struct{ db *sql.DB }

// NewRuns returns the record store on db.
func NewRuns(db *sql.DB) *Runs { return &Runs{db: db} }

// Ping checks the database connection.
func (r *Runs) Ping(ctx context.Context) error { return r.db.PingContext(ctx) }

// Create inserts a new run. The run keeps its creation order.
func (r *Runs) Create(ctx context.Context, run model.Run) error {
	spec, err := json.Marshal(run.Spec)
	if err != nil {
		return fmt.Errorf("store: encode spec: %w", err)
	}
	status, err := json.Marshal(run.Status)
	if err != nil {
		return fmt.Errorf("store: encode status: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO runs (id, seq, spec, status, phase) VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM runs), ?, ?, ?)`,
		run.ID, string(spec), string(status), string(run.Status.Phase))
	if err != nil {
		return fmt.Errorf("store: insert run: %w", err)
	}
	return nil
}

// Get returns one run or ErrNotFound.
func (r *Runs) Get(ctx context.Context, id string) (model.Run, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, spec, status, deletion_requested FROM runs WHERE id = ?`, id)
	run, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, ErrNotFound
	}
	return run, err
}

// List returns every run, oldest first.
func (r *Runs) List(ctx context.Context) ([]model.Run, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, spec, status, deletion_requested FROM runs ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []model.Run
	for rows.Next() {
		run, err := scan(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	return runs, nil
}

// UpdateStatus replaces the status of a run.
func (r *Runs) UpdateStatus(ctx context.Context, id string, status model.Status) error {
	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("store: encode status: %w", err)
	}
	return r.exec(ctx, `UPDATE runs SET status = ?, phase = ? WHERE id = ?`, string(data), string(status.Phase), id)
}

// RequestDeletion marks a run for deletion by the reconcile loop.
func (r *Runs) RequestDeletion(ctx context.Context, id string) error {
	return r.exec(ctx, `UPDATE runs SET deletion_requested = 1 WHERE id = ?`, id)
}

// Delete removes the record.
func (r *Runs) Delete(ctx context.Context, id string) error {
	return r.exec(ctx, `DELETE FROM runs WHERE id = ?`, id)
}

func (r *Runs) exec(ctx context.Context, query string, args ...any) error {
	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scan(s scanner) (model.Run, error) {
	var (
		run          model.Run
		spec, status string
		deletion     int
	)
	if err := s.Scan(&run.ID, &spec, &status, &deletion); err != nil {
		return model.Run{}, err
	}
	if err := json.Unmarshal([]byte(spec), &run.Spec); err != nil {
		return model.Run{}, fmt.Errorf("store: decode spec of %s: %w", run.ID, err)
	}
	if err := json.Unmarshal([]byte(status), &run.Status); err != nil {
		return model.Run{}, fmt.Errorf("store: decode status of %s: %w", run.ID, err)
	}
	run.DeletionRequested = deletion != 0
	return run, nil
}
