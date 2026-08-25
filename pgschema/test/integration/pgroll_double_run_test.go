//go:build integration

package integration

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/workflow"
)

// TestSchemaMigrationWorkflow_RunTwice_SecondRunIsIdempotent reproduces the
// reported bug end to end against a real pgroll binary and a real (throwaway,
// testcontainers) Postgres: clicking "Run" on a scenario that has already
// completed — or double-clicking it — re-submits the exact same migration
// JSON as a brand new workflow execution.
//
// Before the fix, the second run failed in two stages:
//  1. `pgroll validate` on the already-applied migration errors ("column ...
//     already exists"), which the workflow treated as a hard failure and
//     tried to compensate via `pgroll rollback`.
//  2. `pgroll rollback` against a state with nothing in progress errors too
//     ("no active migration"), which the workflow treated as rollback
//     failure — paging the operator at "critical" and returning workflow
//     status "rollback_failed" for what is actually a harmless no-op.
//
// This test runs the real SchemaMigrationWorkflow (via the Temporal test
// environment, which executes registered activities synchronously and
// locally) with the real *activities.PgrollActivities driving the actual
// pgroll binary — not stubs — so it fails if either of those two error
// classes stops being handled.
func TestSchemaMigrationWorkflow_RunTwice_SecondRunIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("pgroll"); err != nil {
		t.Skip("pgroll binary not installed")
	}

	ctx := context.Background()
	dsn, db := startPostgres(t, ctx, "pgroll_double_run")
	defer db.Close()

	// `pgroll init` directly, the same way demo-init does it — CheckPgrollReadiness's
	// AllowInitialize auto-init path depends on stderr content this repo's
	// stdout-only exec path currently discards on failure (a separate,
	// pre-existing issue; not what this test is targeting).
	cmd := exec.CommandContext(ctx, "pgroll", "--postgres-url", dsn, "--schema", "public", "init")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "pgroll init: %s", out)

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pgrollActs := activities.NewPgrollActivities(log)
	alertActs := activities.NewAlertActivities(log) // no webhook configured: paging is a local no-op

	input := types.MigrationInput{
		DSN:    dsn,
		Schema: "public",
		MigrationJSON: `{"operations":[{"create_table":{"name":"widgets","columns":[` +
			`{"name":"id","type":"serial","pk":true},` +
			`{"name":"label","type":"text","nullable":true}` +
			`]}}]}`,
	}

	// First run: the migration is genuinely new — must succeed normally.
	first, err := runMigrationOnce(t, pgrollActs, alertActs, input)
	require.NoError(t, err, "first run should complete cleanly")
	require.Equal(t, "completed", first.Status)

	// Second run: identical input, same migration, same target schema —
	// simulates a double click / re-run of an already-applied scenario.
	// This must NOT come back as "rollback_failed" with a returned error;
	// it should resolve as a clean, idempotent no-op.
	second, err := runMigrationOnce(t, pgrollActs, alertActs, input)
	require.NoError(t, err, "re-running an already-applied migration must not surface as a workflow error")
	require.Equal(t, "completed", second.Status, "re-running an already-applied migration should be treated as a no-op, not rolled back")
	require.NotEqual(t, "rollback_failed", second.Status)
}

// TestSchemaMigrationWorkflow_RunTwice_RenameIsAlsoIdempotent covers the
// second, distinct shape of the same bug class, reported separately: a
// RENAME operation re-run a second time doesn't fail with "already exists"
// (the create/add-column shape covered above) — it fails with the flip
// side, `column "full_name" does not exist on table "users"`, because the
// first run already renamed it away. String-matching "already exists"
// alone (the original fix) does not catch this; content-addressable
// migration identity (matched in reconcileDecision before validate ever
// runs) does, for any operation type.
func TestSchemaMigrationWorkflow_RunTwice_RenameIsAlsoIdempotent(t *testing.T) {
	if _, err := exec.LookPath("pgroll"); err != nil {
		t.Skip("pgroll binary not installed")
	}

	ctx := context.Background()
	dsn, db := startPostgres(t, ctx, "pgroll_double_run_rename")
	defer db.Close()

	_, err := db.ExecContext(ctx, `CREATE TABLE users (id SERIAL PRIMARY KEY, full_name TEXT NOT NULL)`)
	require.NoError(t, err)

	cmd := exec.CommandContext(ctx, "pgroll", "--postgres-url", dsn, "--schema", "public", "init")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "pgroll init: %s", out)
	baselineDir := t.TempDir()
	cmd = exec.CommandContext(ctx, "pgroll", "--postgres-url", dsn, "--schema", "public", "baseline", "00_baseline", baselineDir, "-y")
	out, err = cmd.CombinedOutput()
	require.NoErrorf(t, err, "pgroll baseline: %s", out)

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pgrollActs := activities.NewPgrollActivities(log)
	alertActs := activities.NewAlertActivities(log)

	renameInput := types.MigrationInput{
		DSN:           dsn,
		Schema:        "public",
		MigrationJSON: `{"operations":[{"rename_column":{"table":"users","from":"full_name","to":"display_name"}}]}`,
	}

	first, err := runMigrationOnce(t, pgrollActs, alertActs, renameInput)
	require.NoError(t, err, "first rename should complete cleanly")
	require.Equal(t, "completed", first.Status)

	second, err := runMigrationOnce(t, pgrollActs, alertActs, renameInput)
	require.NoError(t, err, "re-running an already-applied rename must not surface as a workflow error")
	require.Equal(t, "completed", second.Status, "re-running an already-applied rename should be a no-op, not rolled back")
}

// TestSchemaMigrationWorkflow_DifferentMigrationAfterComplete_StillFails is
// the false-positive guard for the fix above: a genuinely DIFFERENT,
// invalid migration submitted against a schema that's in a stable
// "Complete" state (e.g. clicking a later demo scenario before its
// prerequisite ran — see demo/README.md Troubleshooting) must still
// validate and fail normally. It must NOT be mistaken for a duplicate of
// whatever migration last completed just because both are "Complete".
func TestSchemaMigrationWorkflow_DifferentMigrationAfterComplete_StillFails(t *testing.T) {
	if _, err := exec.LookPath("pgroll"); err != nil {
		t.Skip("pgroll binary not installed")
	}

	ctx := context.Background()
	dsn, db := startPostgres(t, ctx, "pgroll_different_migration")
	defer db.Close()

	_, err := db.ExecContext(ctx, `CREATE TABLE users (id SERIAL PRIMARY KEY, full_name TEXT NOT NULL)`)
	require.NoError(t, err)

	cmd := exec.CommandContext(ctx, "pgroll", "--postgres-url", dsn, "--schema", "public", "init")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "pgroll init: %s", out)
	baselineDir := t.TempDir()
	cmd = exec.CommandContext(ctx, "pgroll", "--postgres-url", dsn, "--schema", "public", "baseline", "00_baseline", baselineDir, "-y")
	out, err = cmd.CombinedOutput()
	require.NoErrorf(t, err, "pgroll baseline: %s", out)

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pgrollActs := activities.NewPgrollActivities(log)
	alertActs := activities.NewAlertActivities(log)

	// Complete an unrelated, real migration first, so pgroll status is
	// "Complete" — the same status a duplicate re-run would also see.
	addEmail := types.MigrationInput{
		DSN:           dsn,
		Schema:        "public",
		MigrationJSON: `{"operations":[{"add_column":{"table":"users","column":{"name":"email","type":"text","nullable":true}}}]}`,
	}
	setup, err := runMigrationOnce(t, pgrollActs, alertActs, addEmail)
	require.NoError(t, err)
	require.Equal(t, "completed", setup.Status)

	// A DIFFERENT migration referencing a column that was never created
	// (its real prerequisite never ran) — must fail for real, not be
	// swallowed as "already applied".
	brokenRename := types.MigrationInput{
		DSN:           dsn,
		Schema:        "public",
		MigrationJSON: `{"operations":[{"rename_column":{"table":"users","from":"display_name","to":"first_name"}}]}`,
	}
	result, err := runMigrationOnce(t, pgrollActs, alertActs, brokenRename)
	require.Error(t, err, "a genuinely different, invalid migration must surface as a real failure")
	require.NotEqual(t, "completed", result.Status)
}

// runMigrationOnce executes SchemaMigrationWorkflow once against the real
// PgrollActivities/AlertActivities, auto-signaling app-ready, and returns
// its final progress + workflow error.
func runMigrationOnce(t *testing.T, pgrollActs *activities.PgrollActivities, alertActs *activities.AlertActivities, input types.MigrationInput) (*types.ProgressResponse, error) {
	t.Helper()
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(pgrollActs)
	env.RegisterActivity(alertActs)
	env.RegisterWorkflow(workflow.SchemaMigrationWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflow.SignalAppReady, nil)
	}, 1*time.Millisecond)

	env.ExecuteWorkflow(workflow.SchemaMigrationWorkflow, input)
	require.True(t, env.IsWorkflowCompleted())

	var result types.ProgressResponse
	_ = env.GetWorkflowResult(&result)
	return &result, env.GetWorkflowError()
}
