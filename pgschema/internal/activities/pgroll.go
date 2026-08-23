// Package activities provides Temporal activity implementations for pgroll
// zero-downtime schema migrations.
package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// PgrollActivities holds all pgroll-related Temporal activities.
// Replace any function field with an anonymous function in tests.
type PgrollActivities struct {
	baseActivities
	ValidateFn func(ctx context.Context, input types.MigrationInput) error
	StartFn    func(ctx context.Context, input types.MigrationInput) error
	CompleteFn func(ctx context.Context, input types.MigrationInput) error
	RollbackFn func(ctx context.Context, input types.MigrationInput) error
	StatusFn   func(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error)
}

// NewPgrollActivities returns a PgrollActivities wired to the real pgroll binary.
func NewPgrollActivities(log *slog.Logger) *PgrollActivities {
	a := &PgrollActivities{baseActivities: baseActivities{log: log}}
	a.ValidateFn = a.defaultValidate
	a.StartFn = a.defaultStart
	a.CompleteFn = a.defaultComplete
	a.RollbackFn = a.defaultRollback
	a.StatusFn = a.defaultStatus
	return a
}

// ValidateMigration dry-runs the migration JSON before any DDL touches the DB.
func (a *PgrollActivities) ValidateMigration(ctx context.Context, input types.MigrationInput) error {
	end := a.startTrace(ctx, "pgroll.validate", slog.String("schema", input.Schema))
	err := a.ValidateFn(ctx, input)
	end(err)
	if err != nil {
		return &types.MigrationError{Phase: "validate", Wrapped: err}
	}
	return nil
}

// StartMigration runs pgroll start (expand phase: old+new schema coexist).
func (a *PgrollActivities) StartMigration(ctx context.Context, input types.MigrationInput) error {
	end := a.startTrace(ctx, "pgroll.start", slog.String("schema", input.Schema))
	safeHeartbeat(ctx, "starting")
	err := a.StartFn(ctx, input)
	safeHeartbeat(ctx, "started")
	end(err)
	if err != nil {
		return &types.MigrationError{Phase: "start", Wrapped: err}
	}
	return nil
}

// CompleteMigration runs pgroll complete (contract phase: old schema removed).
func (a *PgrollActivities) CompleteMigration(ctx context.Context, input types.MigrationInput) error {
	end := a.startTrace(ctx, "pgroll.complete", slog.String("schema", input.Schema))
	safeHeartbeat(ctx, "completing")
	err := a.CompleteFn(ctx, input)
	safeHeartbeat(ctx, "completed")
	end(err)
	if err != nil {
		return &types.MigrationError{Phase: "complete", Wrapped: err}
	}
	return nil
}

// RollbackMigration reverts the expand phase.
func (a *PgrollActivities) RollbackMigration(ctx context.Context, input types.MigrationInput) error {
	end := a.startTrace(ctx, "pgroll.rollback", slog.String("schema", input.Schema))
	err := a.RollbackFn(ctx, input)
	end(err)
	if err != nil {
		return &types.MigrationError{Phase: "rollback", Wrapped: err}
	}
	return nil
}

// GetMigrationStatus returns the current pgroll migration state.
func (a *PgrollActivities) GetMigrationStatus(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error) {
	end := a.startTrace(ctx, "pgroll.status", slog.String("schema", input.Schema))
	status, err := a.StatusFn(ctx, input)
	end(err)
	if err != nil {
		return nil, &types.MigrationError{Phase: "status", Wrapped: err}
	}
	return status, nil
}

func (a *PgrollActivities) defaultValidate(ctx context.Context, input types.MigrationInput) error {
	return a.runPgroll(ctx, input.DSN, input.Schema, []string{"validate"}, input.MigrationJSON)
}

func (a *PgrollActivities) defaultStart(ctx context.Context, input types.MigrationInput) error {
	return a.runPgroll(ctx, input.DSN, input.Schema, []string{"start", "--complete=false"}, input.MigrationJSON)
}

func (a *PgrollActivities) defaultComplete(ctx context.Context, input types.MigrationInput) error {
	return a.runPgroll(ctx, input.DSN, input.Schema, []string{"complete"}, "")
}

func (a *PgrollActivities) defaultRollback(ctx context.Context, input types.MigrationInput) error {
	return a.runPgroll(ctx, input.DSN, input.Schema, []string{"rollback"}, "")
}

func (a *PgrollActivities) defaultStatus(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error) {
	out, err := a.runPgrollOutput(ctx, input.DSN, input.Schema, []string{"status"})
	if err != nil {
		return nil, fmt.Errorf("pgroll status: %w", err)
	}
	var s types.MigrationStatus
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("parsing pgroll status output: %w", err)
	}
	return &s, nil
}
