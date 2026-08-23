// Package activities provides Temporal activity implementations for pgroll
// zero-downtime schema migrations.
package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"


	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// PgrollActivities holds all pgroll-related Temporal activities.
// Replace any function field with an anonymous function in tests.
type PgrollActivities struct {
	ValidateFn func(ctx context.Context, input types.MigrationInput) error
	StartFn    func(ctx context.Context, input types.MigrationInput) error
	CompleteFn func(ctx context.Context, input types.MigrationInput) error
	RollbackFn func(ctx context.Context, input types.MigrationInput) error
	StatusFn   func(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error)
	log        *slog.Logger
}

// NewPgrollActivities returns a PgrollActivities wired to the real pgroll binary.
func NewPgrollActivities(log *slog.Logger) *PgrollActivities {
	a := &PgrollActivities{log: log}
	a.ValidateFn = a.defaultValidate
	a.StartFn = a.defaultStart
	a.CompleteFn = a.defaultComplete
	a.RollbackFn = a.defaultRollback
	a.StatusFn = a.defaultStatus
	return a
}
// logger returns the struct's logger, falling back to slog.Default() if nil.
func (a *PgrollActivities) logger() *slog.Logger {
	if a.log == nil {
		return slog.Default()
	}
	return a.log
}


// ValidateMigration dry-runs the migration JSON before any DDL touches the DB.
func (a *PgrollActivities) ValidateMigration(ctx context.Context, input types.MigrationInput) error {
	a.logger().InfoContext(ctx, "validating migration", slog.String("schema", input.Schema))
	if err := a.ValidateFn(ctx, input); err != nil {
		return &types.MigrationError{Phase: "validate", Wrapped: err}
	}
	return nil
}

// StartMigration runs pgroll start (expand phase: old+new schema coexist).
func (a *PgrollActivities) StartMigration(ctx context.Context, input types.MigrationInput) error {
	a.logger().InfoContext(ctx, "starting migration", slog.String("schema", input.Schema))
	safeHeartbeat(ctx, "starting")
	if err := a.StartFn(ctx, input); err != nil {
		return &types.MigrationError{Phase: "start", Wrapped: err}
	}
	safeHeartbeat(ctx, "started")
	return nil
}

// CompleteMigration runs pgroll complete (contract phase: old schema removed).
func (a *PgrollActivities) CompleteMigration(ctx context.Context, input types.MigrationInput) error {
	a.logger().InfoContext(ctx, "completing migration", slog.String("schema", input.Schema))
	safeHeartbeat(ctx, "completing")
	if err := a.CompleteFn(ctx, input); err != nil {
		return &types.MigrationError{Phase: "complete", Wrapped: err}
	}
	safeHeartbeat(ctx, "completed")
	return nil
}

// RollbackMigration reverts the expand phase.
func (a *PgrollActivities) RollbackMigration(ctx context.Context, input types.MigrationInput) error {
	a.logger().InfoContext(ctx, "rolling back migration", slog.String("schema", input.Schema))
	if err := a.RollbackFn(ctx, input); err != nil {
		return &types.MigrationError{Phase: "rollback", Wrapped: err}
	}
	return nil
}

// GetMigrationStatus returns the current pgroll migration state.
func (a *PgrollActivities) GetMigrationStatus(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error) {
	status, err := a.StatusFn(ctx, input)
	if err != nil {
		return nil, &types.MigrationError{Phase: "status", Wrapped: err}
	}
	return status, nil
}

func (a *PgrollActivities) defaultValidate(ctx context.Context, input types.MigrationInput) error {
	return runPgroll(ctx, input.DSN, input.Schema, []string{"validate"}, input.MigrationJSON)
}

func (a *PgrollActivities) defaultStart(ctx context.Context, input types.MigrationInput) error {
	return runPgroll(ctx, input.DSN, input.Schema, []string{"start", "--complete=false"}, input.MigrationJSON)
}

func (a *PgrollActivities) defaultComplete(ctx context.Context, input types.MigrationInput) error {
	return runPgroll(ctx, input.DSN, input.Schema, []string{"complete"}, "")
}

func (a *PgrollActivities) defaultRollback(ctx context.Context, input types.MigrationInput) error {
	return runPgroll(ctx, input.DSN, input.Schema, []string{"rollback"}, "")
}

func (a *PgrollActivities) defaultStatus(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error) {
	var stdout bytes.Buffer
	args := []string{"--dsn", input.DSN, "--schema", input.Schema, "status", "--output", "json"}
	cmd := exec.CommandContext(ctx, "pgroll", args...)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pgroll status: %w", err)
	}
	var s types.MigrationStatus
	if err := json.Unmarshal(stdout.Bytes(), &s); err != nil {
		return nil, fmt.Errorf("parsing pgroll status output: %w", err)
	}
	return &s, nil
}

// runPgroll invokes the pgroll CLI; migrationJSON is piped on stdin when non-empty.
func runPgroll(ctx context.Context, dsn, schema string, args []string, migrationJSON string) error {
	base := []string{"--dsn", dsn, "--schema", schema}
	cmd := exec.CommandContext(ctx, "pgroll", append(base, args...)...)
	if migrationJSON != "" {
		cmd.Stdin = strings.NewReader(migrationJSON)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pgroll %s: %w — %s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

// redactDSN masks the password field for safe logging.
// Supports keyword=value (****** and URI (://user:pass@host) forms.
func redactDSN(dsn string) string {
	pwKey := " password="
	if i := strings.Index(dsn, pwKey); i >= 0 {
		rest := dsn[i+len(pwKey):]
		end := strings.IndexAny(rest, " 	")
		if end < 0 {
			end = len(rest)
		}
		return dsn[:i] + pwKey + "******" + rest[end:]
	}
	if i := strings.Index(dsn, "://"); i >= 0 {
		after := dsn[i+3:]
		if atIdx := strings.Index(after, "@"); atIdx >= 0 {
			cred := after[:atIdx]
			if colonIdx := strings.Index(cred, ":"); colonIdx >= 0 {
				return dsn[:i+3] + cred[:colonIdx+1] + "******" + "@" + after[atIdx+1:]
			}
		}
	}
	return dsn
}
