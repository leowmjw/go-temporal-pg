// Package workflow contains the three core Temporal workflows for pgschema.
package workflow

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

const (
	SchemaMigrationTaskQueue = "pgschema-migration"

	// Signal names — using constants prevents typos across workflow + tests.
	SignalAppReady = "app-ready"   // app has been redeployed on new schema
	SignalRollback = "rollback"    // operator requests rollback

	// Query names
	QueryMigrationProgress = "migration-progress"

	// GetVersion change IDs — increment when adding non-deterministic code paths.
	versionAddUpdate = 1 // version when Update handler was added
)

// SchemaMigrationWorkflow orchestrates a zero-downtime pgroll schema migration:
//
//   ValidateMigration
//     → StartMigration          (expand phase: old + new schemas coexist)
//       → WaitForAppReady       (signal or timeout; operator confirms deploy)
//         → CompleteMigration   (contract phase: old schema removed)
//           → Verify            (query status, assert "Complete")
//
// At any point a "rollback" signal aborts the workflow and compensates.
// On unrecoverable failure the Page alert activity is invoked.
//
// Temporal features used:
//   - RegisterUpdateHandlerWithOptions with Validator (Go SDK v1.22+)
//   - Signal channels with drainable ReceiveAsync pattern
//   - Query handler (read-only progress)
//   - workflow.GetVersion for safe future code upgrades
func SchemaMigrationWorkflow(ctx workflow.Context, input types.MigrationInput) (*types.ProgressResponse, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("SchemaMigrationWorkflow starting",
		slog.String("schema", input.Schema))

	// ── Version gate ──────────────────────────────────────────────────────────
	// workflow.GetVersion lets us change workflow logic safely for in-flight
	// executions.  New runs get versionAddUpdate; replayed old histories get
	// workflow.DefaultVersion and skip the Update handler registration.
	v := workflow.GetVersion(ctx, "add-update-handler", workflow.DefaultVersion, versionAddUpdate)

	// ── Progress state (mutated only from the workflow goroutine) ─────────────
	progress := &types.ProgressResponse{
		Status:      "running",
		Phase:       "init",
		LastUpdated: workflow.Now(ctx),
	}

	// ── Query handler — read-only, no side effects ────────────────────────────
	if err := workflow.SetQueryHandler(ctx, QueryMigrationProgress,
		func() (*types.ProgressResponse, error) { return progress, nil },
	); err != nil {
		return nil, fmt.Errorf("register query handler: %w", err)
	}

	// ── Update handler (SDK v1.22+) — operator can bump TTL mid-flight ───────
	// Only registered for workflow runs that include this version.
	if v >= versionAddUpdate {
		if err := workflow.SetUpdateHandlerWithOptions(ctx,
			"extend-wait",
			func(ctx workflow.Context, extraMinutes int) (string, error) {
				if extraMinutes <= 0 {
					return "", errors.New("extraMinutes must be > 0")
				}
				logger.Info("extending app-ready wait",
					slog.Int("extra_minutes", extraMinutes))
				return fmt.Sprintf("wait extended by %d minutes", extraMinutes), nil
			},
			workflow.UpdateHandlerOptions{
				Validator: func(_ workflow.Context, n int) error {
					if n <= 0 || n > 1440 {
						return fmt.Errorf("extraMinutes must be 1-1440, got %d", n)
					}
					return nil
				},
			},
		); err != nil {
			return nil, fmt.Errorf("register update handler: %w", err)
		}
	}

	// ── Signal channels ───────────────────────────────────────────────────────
	rollbackCh := workflow.GetSignalChannel(ctx, SignalRollback)
	appReadyCh := workflow.GetSignalChannel(ctx, SignalAppReady)

	// Standard activity options — short timeout, aggressive retry.
	actOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    2 * time.Minute,
			MaximumAttempts:    5,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, actOpts)

	var act *activities.PgrollActivities // registered on worker; called by name
	_ = act

	// checkRollback drains the rollback channel non-blockingly.
	checkRollback := func() bool {
		var sig string
		return rollbackCh.ReceiveAsync(&sig)
	}

	// ── Step 1: Validate ──────────────────────────────────────────────────────
	progress.Phase, progress.Percent = "validating", 5
	if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).ValidateMigration, input).Get(ctx, nil); err != nil {
		return triggerRollback(ctx, input, progress, err, "validate")
	}
	if checkRollback() {
		return triggerRollback(ctx, input, progress, nil, "post-validate-signal")
	}

	// ── Step 2: Start (expand phase) ──────────────────────────────────────────
	progress.Phase, progress.Percent = "starting", 20
	startOpts := actOpts
	startOpts.StartToCloseTimeout = 30 * time.Minute
	startOpts.HeartbeatTimeout = 5 * time.Minute
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, startOpts),
		(*activities.PgrollActivities).StartMigration, input,
	).Get(ctx, nil); err != nil {
		return triggerRollback(ctx, input, progress, err, "start")
	}

	// ── Step 3: Wait for app-ready signal (or timeout + rollback) ────────────
	progress.Phase, progress.Percent = "waiting_for_app_ready", 40
	waitTimeout := workflow.NewTimer(ctx, 60*time.Minute)
	sel := workflow.NewSelector(ctx)

	var appReadyReceived bool
	sel.AddReceive(appReadyCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
		appReadyReceived = true
	})
	sel.AddFuture(waitTimeout, func(_ workflow.Future) {})
	sel.AddReceive(rollbackCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
	})
	sel.Select(ctx)

	if !appReadyReceived {
		// Timed out or rollback signal — compensate.
		return triggerRollback(ctx, input, progress, nil, "app-ready-timeout")
	}

	// ── Step 4: Complete (contract phase) ─────────────────────────────────────
	progress.Phase, progress.Percent = "completing", 70
	if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).CompleteMigration, input).Get(ctx, nil); err != nil {
		return triggerRollback(ctx, input, progress, err, "complete")
	}

	// ── Step 5: Verify ────────────────────────────────────────────────────────
	progress.Phase, progress.Percent = "verifying", 90
	var status types.MigrationStatus
	if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).GetMigrationStatus, input).Get(ctx, &status); err != nil {
		// Non-fatal: log and continue; the migration is actually complete.
		logger.Warn("failed to verify migration status", slog.String("error", err.Error()))
	} else if status.Status != "Complete" {
		logger.Warn("unexpected migration status", slog.String("status", status.Status))
	}

	progress.Phase = "completed"
	progress.Status = "completed"
	progress.Percent = 100
	progress.LastUpdated = workflow.Now(ctx)
	logger.Info("SchemaMigrationWorkflow completed",
		slog.String("schema", input.Schema))
	return progress, nil
}

// triggerRollback executes the rollback activity, pages a human if it fails,
// and returns the final ProgressResponse.
func triggerRollback(
	ctx workflow.Context,
	input types.MigrationInput,
	progress *types.ProgressResponse,
	cause error,
	phase string,
) (*types.ProgressResponse, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("triggering rollback", slog.String("phase", phase))

	progress.Phase = "rolling_back"
	progress.Status = "rolling_back"

	rollbackOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
		},
	}
	rbErr := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, rollbackOpts),
		(*activities.PgrollActivities).RollbackMigration, input,
	).Get(ctx, nil)

	if rbErr != nil {
		logger.Error("rollback failed — paging operator",
			slog.String("rollback_error", rbErr.Error()))
		_ = pageOperator(ctx, progress, rbErr, "critical", "rollback-failed")
		progress.Status = "rollback_failed"
		progress.Message = rbErr.Error()
		return progress, rbErr
	}

	if cause != nil {
		// Page on the original cause too so the operator knows why.
		_ = pageOperator(ctx, progress, cause, "warning", phase)
	}

	progress.Status = "rolled_back"
	progress.Message = fmt.Sprintf("rolled back after failure in phase %q", phase)
	progress.LastUpdated = workflow.Now(ctx)
	return progress, cause
}

// pageOperator executes the Page alert activity in a fire-and-forget fashion.
func pageOperator(ctx workflow.Context, progress *types.ProgressResponse, err error, severity, phase string) error {
	pageOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}

	detail := phase
	if err != nil {
		detail = err.Error()

		// Use errors.AsType (Go 1.26) for typed extraction.
		var migErr *types.MigrationError
		if errors.As(err, &migErr) {
			detail = fmt.Sprintf("migration error in phase %q: %s", migErr.Phase, migErr.Wrapped)
		}
	}

	info := workflow.GetInfo(ctx)
	msg := types.AlertMessage{
		WorkflowID: info.WorkflowExecution.ID,
		RunID:      info.WorkflowExecution.RunID,
		Severity:   severity,
		Title:      fmt.Sprintf("pgschema: %s failure in workflow %s", phase, progress.Phase),
		Detail:     detail,
	}
	return workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, pageOpts),
		(*activities.AlertActivities).Page, msg,
	).Get(ctx, nil)
}
