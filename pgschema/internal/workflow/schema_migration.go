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
	SignalAppReady = "app-ready" // app has been redeployed on new schema
	SignalRollback = "rollback"  // operator requests rollback

	// Query names.
	QueryMigrationProgress = "migration-progress"

	// GetVersion change IDs — increment when adding non-deterministic code paths.
	versionAddUpdate     = 1
	versionPgrollRoadmap = 1
)

// SchemaMigrationWorkflow orchestrates a zero-downtime pgroll schema migration.
func SchemaMigrationWorkflow(ctx workflow.Context, input types.MigrationInput) (*types.ProgressResponse, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("SchemaMigrationWorkflow starting", slog.String("schema", input.Schema))

	updateVersion := workflow.GetVersion(ctx, "add-update-handler", workflow.DefaultVersion, versionAddUpdate)
	roadmapVersion := workflow.GetVersion(ctx, "pgroll-roadmap", workflow.DefaultVersion, versionPgrollRoadmap)
	roadmapEnabled := roadmapVersion >= versionPgrollRoadmap

	progress := &types.ProgressResponse{
		Status:      "running",
		Phase:       "init",
		LastUpdated: workflow.Now(ctx),
	}
	setProgress := func(phase string, percent int, message string) {
		progress.Phase = phase
		progress.Percent = percent
		progress.LastUpdated = workflow.Now(ctx)
		if message != "" {
			progress.Message = message
		}
	}

	if err := workflow.SetQueryHandler(ctx, QueryMigrationProgress, func() (*types.ProgressResponse, error) {
		return progress, nil
	}); err != nil {
		return nil, fmt.Errorf("register query handler: %w", err)
	}

	extendWaitCh := workflow.NewBufferedChannel(ctx, 16)
	if updateVersion >= versionAddUpdate {
		if err := workflow.SetUpdateHandlerWithOptions(ctx,
			"extend-wait",
			func(uCtx workflow.Context, extraMinutes int) (string, error) {
				if extraMinutes <= 0 {
					return "", errors.New("extraMinutes must be > 0")
				}
				logger.Info("extending app-ready wait", slog.Int("extra_minutes", extraMinutes))
				extendWaitCh.Send(uCtx, extraMinutes)
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

	rollbackCh := workflow.GetSignalChannel(ctx, SignalRollback)
	appReadyCh := workflow.GetSignalChannel(ctx, SignalAppReady)

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

	checkRollback := func() bool {
		var sig string
		return rollbackCh.ReceiveAsync(&sig)
	}
	updateStatus := func(status *types.MigrationStatus) {
		if status == nil {
			return
		}
		progress.PgrollStatus = status
		progress.LastUpdated = workflow.Now(ctx)
	}
	bestEffortStatus := func() {
		if !roadmapEnabled {
			return
		}
		var status types.MigrationStatus
		if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).GetMigrationStatus, input).Get(ctx, &status); err != nil {
			logger.Warn("failed to refresh pgroll status", slog.String("error", err.Error()), slog.String("phase", progress.Phase))
			return
		}
		updateStatus(&status)
	}
	reconcile := func(phase string) (*types.ReconciliationResult, error) {
		if !roadmapEnabled {
			return &types.ReconciliationResult{Action: "continue"}, nil
		}
		var result types.ReconciliationResult
		err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).ReconcileMigrationState, types.ReconcileInput{Migration: input, Phase: phase}).Get(ctx, &result)
		if err != nil {
			return nil, err
		}
		progress.ReconciliationAction = result.Action
		updateStatus(result.Status)
		return &result, nil
	}

	if roadmapEnabled {
		setProgress("preflighting", 1, "checking pgroll binary and metadata")
		if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).CheckPgrollVersion, input).Get(ctx, &progress.PgrollVersion); err != nil {
			progress.Status = "failed"
			progress.Message = err.Error()
			return nil, err
		}
		var readiness types.PgrollReadiness
		if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).CheckPgrollReadiness, input).Get(ctx, &readiness); err != nil {
			progress.Status = "failed"
			progress.Message = err.Error()
			return nil, err
		}
		if readiness.Message != "" {
			progress.Message = readiness.Message
		}
		var risk types.MigrationRiskReport
		if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).AnalyzeMigrationRisk, input).Get(ctx, &risk); err != nil {
			progress.Status = "failed"
			progress.Message = err.Error()
			return nil, err
		}
		progress.RiskReport = &risk
		if risk.Blocked || risk.RequiresApproval {
			progress.Status = "failed"
			progress.Message = risk.Summary
			return nil, temporal.NewNonRetryableApplicationError(risk.Summary, "migration-policy", nil)
		}
		bestEffortStatus()
	}

	setProgress("validating", 5, progress.Message)
	if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).ValidateMigration, input).Get(ctx, nil); err != nil {
		return triggerRollback(ctx, input, progress, err, "validate", roadmapEnabled)
	}
	if checkRollback() {
		return triggerRollback(ctx, input, progress, nil, "post-validate-signal", roadmapEnabled)
	}

	shouldStart := true
	if roadmapEnabled {
		bestEffortStatus()
		result, err := reconcile("before_start")
		if err != nil {
			return triggerRollback(ctx, input, progress, err, "reconcile-before-start", roadmapEnabled)
		}
		switch result.Action {
		case "already_complete":
			bestEffortStatus()
			setProgress("completed", 100, result.Reason)
			progress.Status = "completed"
			return progress, nil
		case "resume_wait":
			shouldStart = false
			progress.Message = result.Reason
		case "fail":
			progress.Status = "failed"
			progress.Message = result.Reason
			return nil, temporal.NewNonRetryableApplicationError(result.Reason, "pgroll-reconcile", nil)
		}
	}

	if shouldStart {
		setProgress("starting", 20, progress.Message)
		startOpts := actOpts
		startOpts.StartToCloseTimeout = 30 * time.Minute
		startOpts.HeartbeatTimeout = 5 * time.Minute
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, startOpts), (*activities.PgrollActivities).StartMigration, input).Get(ctx, nil); err != nil {
			return triggerRollback(ctx, input, progress, err, "start", roadmapEnabled)
		}
	}

	if roadmapEnabled {
		bestEffortStatus()
		if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).GetLatestSchema, input).Get(ctx, &progress.LatestSchema); err != nil {
			return triggerRollback(ctx, input, progress, err, "latest-schema", roadmapEnabled)
		}
	}

	setProgress("waiting_for_app_ready", 40, progress.Message)
	const initialAppReadyWait = 60 * time.Minute
	waitDeadline := workflow.Now(ctx).Add(initialAppReadyWait)

	var appReadyReceived, rollbackRequested, timedOut bool
	for !appReadyReceived && !rollbackRequested && !timedOut {
		remaining := waitDeadline.Sub(workflow.Now(ctx))
		if remaining < 0 {
			remaining = 0
		}
		waitTimeout := workflow.NewTimer(ctx, remaining)
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(appReadyCh, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)
			appReadyReceived = true
		})
		sel.AddFuture(waitTimeout, func(_ workflow.Future) {
			timedOut = true
		})
		sel.AddReceive(rollbackCh, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)
			rollbackRequested = true
		})
		sel.AddReceive(extendWaitCh, func(c workflow.ReceiveChannel, _ bool) {
			var extraMinutes int
			c.Receive(ctx, &extraMinutes)
			waitDeadline = waitDeadline.Add(time.Duration(extraMinutes) * time.Minute)
			logger.Info("app-ready wait deadline extended", slog.Int("extra_minutes", extraMinutes), slog.Time("new_deadline", waitDeadline))
		})
		sel.Select(ctx)
	}
	if !appReadyReceived {
		return triggerRollback(ctx, input, progress, nil, "app-ready-timeout", roadmapEnabled)
	}

	shouldComplete := true
	if roadmapEnabled {
		bestEffortStatus()
		result, err := reconcile("before_complete")
		if err != nil {
			return triggerRollback(ctx, input, progress, err, "reconcile-before-complete", roadmapEnabled)
		}
		switch result.Action {
		case "skip_complete", "already_complete":
			shouldComplete = false
			progress.Message = result.Reason
		case "fail":
			return triggerRollback(ctx, input, progress, temporal.NewNonRetryableApplicationError(result.Reason, "pgroll-reconcile", nil), "reconcile-before-complete", roadmapEnabled)
		}
	}

	if shouldComplete {
		setProgress("completing", 70, progress.Message)
		if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).CompleteMigration, input).Get(ctx, nil); err != nil {
			return triggerRollback(ctx, input, progress, err, "complete", roadmapEnabled)
		}
	}

	setProgress("verifying", 90, progress.Message)
	var status types.MigrationStatus
	if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).GetMigrationStatus, input).Get(ctx, &status); err != nil {
		logger.Warn("failed to verify migration status", slog.String("error", err.Error()))
	} else {
		updateStatus(&status)
		if status.Status != "Complete" {
			logger.Warn("unexpected migration status", slog.String("status", status.Status))
		}
	}

	setProgress("completed", 100, progress.Message)
	progress.Status = "completed"
	logger.Info("SchemaMigrationWorkflow completed", slog.String("schema", input.Schema), slog.String("latest_schema", progress.LatestSchema), slog.String("pgroll_version", progress.PgrollVersion))
	return progress, nil
}

func triggerRollback(
	ctx workflow.Context,
	input types.MigrationInput,
	progress *types.ProgressResponse,
	cause error,
	phase string,
	roadmapEnabled bool,
) (*types.ProgressResponse, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("triggering rollback", slog.String("phase", phase))

	progress.Phase = "rolling_back"
	progress.Status = "rolling_back"
	progress.LastUpdated = workflow.Now(ctx)

	if roadmapEnabled {
		var decision types.ReconciliationResult
		if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).ReconcileMigrationState, types.ReconcileInput{Migration: input, Phase: "before_rollback"}).Get(ctx, &decision); err == nil {
			progress.ReconciliationAction = decision.Action
			if decision.Status != nil {
				progress.PgrollStatus = decision.Status
			}
			if decision.Action == "skip_rollback" {
				progress.Status = "rolled_back"
				progress.Message = decision.Reason
				progress.LastUpdated = workflow.Now(ctx)
				return progress, cause
			}
		} else {
			logger.Warn("rollback reconciliation failed", slog.String("error", err.Error()))
		}
	}

	rollbackOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
		},
	}
	rbErr := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, rollbackOpts), (*activities.PgrollActivities).RollbackMigration, input).Get(ctx, nil)
	if rbErr != nil {
		logger.Error("rollback failed — paging operator", slog.String("rollback_error", rbErr.Error()))
		_ = pageOperator(ctx, progress, rbErr, "critical", "rollback-failed")
		progress.Status = "rollback_failed"
		progress.Message = rbErr.Error()
		return progress, rbErr
	}

	if roadmapEnabled {
		var status types.MigrationStatus
		if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).GetMigrationStatus, input).Get(ctx, &status); err == nil {
			progress.PgrollStatus = &status
		}
	}
	if cause != nil {
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
	return workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, pageOpts), (*activities.AlertActivities).Page, msg).Get(ctx, nil)
}
