package workflow

import (
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"company.com/infra/pgactive-upgrade/internal/types"
)

const (
	WorkflowName = "RollingUpgradeWorkflow"
	TaskQueue    = "pgactive-upgrade"
)

// RollingUpgradeWorkflow orchestrates the PostgreSQL major version upgrade
func RollingUpgradeWorkflow(ctx workflow.Context, input types.UpgradeInput) (*types.ProgressResponse, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Starting rolling upgrade workflow",
		slog.String("source_db", input.SourceDBInstanceID),
		slog.String("target_version", input.TargetVersion))

	// Set up workflow state and signals
	var rollbackSignal types.RollbackSignal
	rollbackChannel := workflow.GetSignalChannel(ctx, "rollback")

	var progress types.ProgressResponse
	progress.Status = "running"
	progress.LastUpdated = workflow.Now(ctx)

	// Set up query handler for progress
	err := workflow.SetQueryHandler(ctx, "progress", func() (types.ProgressResponse, error) {
		return progress, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to set query handler: %w", err)
	}

	// Activity options
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, activityOptions)

	// Step 1: Validate input
	progress.Phase = "validating"
	progress.Percent = 5
	
	validateCtx, validateCancel := workflow.WithCancel(ctx)
	validateFuture := workflow.ExecuteActivity(validateCtx, "ValidateInput", input)
	
	selector := workflow.NewSelector(ctx)
	selector.AddFuture(validateFuture, func(f workflow.Future) {
		if err := f.Get(validateCtx, nil); err != nil {
			logger.Error("Validation failed", slog.String("error", err.Error()))
			progress.Status = "failed"
			progress.Message = err.Error()
		}
	})
	selector.AddReceive(rollbackChannel, func(c workflow.ReceiveChannel, more bool) {
		c.Receive(ctx, &rollbackSignal)
		logger.Info("Rollback signal received", slog.String("reason", rollbackSignal.Reason))
		validateCancel()
		progress.Status = "rolling_back"
	})
	
	selector.Select(ctx)
	if progress.Status == "failed" || progress.Status == "rolling_back" {
		return &progress, nil
	}

	// Step 2: Provision target DB
	progress.Phase = "provisioning"
	progress.Percent = 15
	
	var targetDBID string
	activityInput := types.ActivityInput{
		SourceDBInstanceID: input.SourceDBInstanceID,
	}
	
	err = workflow.ExecuteActivity(ctx, "ProvisionTargetDB", input).Get(ctx, &targetDBID)
	if err != nil {
		logger.Error("Failed to provision target DB", slog.String("error", err.Error()))
		progress.Status = "failed"
		progress.Message = err.Error()
		return &progress, err
	}
	
	activityInput.TargetDBInstanceID = targetDBID
	logger.Info("Target DB provisioned", slog.String("target_db", targetDBID))

	// Step 3: Configure pgactive parameters
	progress.Phase = "configuring_pgactive"
	progress.Percent = 25
	
	err = workflow.ExecuteActivity(ctx, "ConfigurePgactiveParams", activityInput).Get(ctx, nil)
	if err != nil {
		logger.Error("Failed to configure pgactive", slog.String("error", err.Error()))
		progress.Status = "failed"
		progress.Message = err.Error()
		return &progress, err
	}

	// Step 4: Install pgactive extension
	progress.Phase = "installing_extension"
	progress.Percent = 35
	
	err = workflow.ExecuteActivity(ctx, "InstallPgactiveExtension", activityInput).Get(ctx, nil)
	if err != nil {
		logger.Error("Failed to install pgactive extension", slog.String("error", err.Error()))
		progress.Status = "failed"
		progress.Message = err.Error()
		return &progress, err
	}

	// Step 5: Initialize replication group (child workflow)
	progress.Phase = "initializing_replication"
	progress.Percent = 45
	
	childWorkflowOptions := workflow.ChildWorkflowOptions{
		WorkflowID: fmt.Sprintf("replication-group-%s-%d", input.SourceDBInstanceID, workflow.Now(ctx).Unix()),
	}
	childCtx := workflow.WithChildOptions(ctx, childWorkflowOptions)
	
	var replicationGroupID string
	err = workflow.ExecuteChildWorkflow(childCtx, "InitReplicationGroupWorkflow", activityInput).Get(ctx, &replicationGroupID)
	if err != nil {
		logger.Error("Failed to initialize replication", slog.String("error", err.Error()))
		progress.Status = "failed"
		progress.Message = err.Error()
		return &progress, err
	}
	
	activityInput.ReplicationGroup = replicationGroupID

	// Step 6: Wait for sync with heartbeat
	progress.Phase = "waiting_for_sync"
	progress.Percent = 55
	
	heartbeatOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 24 * time.Hour,
		HeartbeatTimeout:    5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    10 * time.Second,
			BackoffCoefficient: 1.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    1,
		},
	}
	heartbeatCtx := workflow.WithActivityOptions(ctx, heartbeatOptions)
	
	err = workflow.ExecuteActivity(heartbeatCtx, "WaitForSync", activityInput).Get(ctx, nil)
	if err != nil {
		logger.Error("Sync wait failed", slog.String("error", err.Error()))
		progress.Status = "failed"
		progress.Message = err.Error()
		return &progress, err
	}

	// Step 7: Traffic shift phases
	basePercent := 65
	percentPerPhase := 25 / len(input.ShiftPercentages)
	
	for i, shiftPercent := range input.ShiftPercentages {
		progress.Phase = fmt.Sprintf("traffic_shift_%d", i+1)
		progress.Percent = basePercent + (i * percentPerPhase)
		
		shiftInput := types.TrafficShiftInput{
			ActivityInput:   activityInput,
			ShiftPercentage: shiftPercent,
			Phase:           i + 1,
		}
		
		// Check for rollback signal before each shift
		selector := workflow.NewSelector(ctx)
		selector.AddDefault(func() {
			// Continue with traffic shift
		})
		selector.AddReceive(rollbackChannel, func(c workflow.ReceiveChannel, more bool) {
			c.Receive(ctx, &rollbackSignal)
			logger.Info("Rollback during traffic shift", slog.String("reason", rollbackSignal.Reason))
			progress.Status = "rolling_back"
		})
		selector.Select(ctx)
		
		if progress.Status == "rolling_back" {
			return executeRollback(ctx, activityInput, &progress)
		}
		
		err = workflow.ExecuteActivity(ctx, "TrafficShiftPhase", shiftInput).Get(ctx, nil)
		if err != nil {
			logger.Error("Traffic shift failed", slog.Int("phase", i+1), slog.String("error", err.Error()))
			progress.Status = "failed"
			progress.Message = err.Error()
			return &progress, err
		}
		
		// Health checks after each shift
		healthInput := types.HealthCheckInput{
			ActivityInput: activityInput,
			CheckType:     "post_shift",
			Timeout:       300,
		}
		
		err = workflow.ExecuteActivity(ctx, "RunHealthChecks", healthInput).Get(ctx, nil)
		if err != nil {
			logger.Error("Health check failed", slog.Int("phase", i+1), slog.String("error", err.Error()))
			// Auto-rollback on health check failure
			return executeRollback(ctx, activityInput, &progress)
		}
	}

	// Step 8: Final cutover
	progress.Phase = "cutover"
	progress.Percent = 90
	
	err = workflow.ExecuteActivity(ctx, "Cutover", activityInput).Get(ctx, nil)
	if err != nil {
		logger.Error("Cutover failed", slog.String("error", err.Error()))
		return executeRollback(ctx, activityInput, &progress)
	}

	// Step 9: Optional cleanup
	progress.Phase = "cleanup"
	progress.Percent = 95
	
	err = workflow.ExecuteActivity(ctx, "OptionallyDetachOld", activityInput).Get(ctx, nil)
	if err != nil {
		logger.Warn("Failed to detach old DB", slog.String("error", err.Error()))
		// Non-fatal error, continue
	}

	// Step 10: Decommission source
	progress.Phase = "decommissioning"
	progress.Percent = 100
	
	err = workflow.ExecuteActivity(ctx, "DecommissionSource", activityInput).Get(ctx, nil)
	if err != nil {
		logger.Warn("Failed to decommission source", slog.String("error", err.Error()))
		// Non-fatal error for completed upgrade
	}

	progress.Status = "completed"
	progress.Message = "Rolling upgrade completed successfully"
	progress.LastUpdated = workflow.Now(ctx)
	
	logger.Info("Rolling upgrade completed successfully",
		slog.String("source_db", input.SourceDBInstanceID),
		slog.String("target_db", targetDBID))

	return &progress, nil
}

func executeRollback(ctx workflow.Context, input types.ActivityInput, progress *types.ProgressResponse) (*types.ProgressResponse, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Executing rollback procedure")
	
	progress.Phase = "rolling_back"
	progress.Status = "rolling_back"
	
	rollbackOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
	rollbackCtx := workflow.WithActivityOptions(ctx, rollbackOptions)
	
	err := workflow.ExecuteActivity(rollbackCtx, "ExecuteRollback", input).Get(ctx, nil)
	if err != nil {
		logger.Error("Rollback failed", slog.String("error", err.Error()))
		progress.Status = "rollback_failed"
		progress.Message = fmt.Sprintf("Rollback failed: %s", err.Error())
		return progress, err
	}
	
	progress.Status = "rolled_back"
	progress.Message = "Successfully rolled back to original state"
	progress.LastUpdated = workflow.Now(ctx)
	
	return progress, nil
}
