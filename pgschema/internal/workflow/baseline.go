package workflow

import (
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

const QueryBaselineStatus = "baseline-status"

// BaselineWorkflow creates a pgroll baseline for an existing brownfield schema.
func BaselineWorkflow(ctx workflow.Context, input types.BaselineInput) (*types.BaselineResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("BaselineWorkflow starting", slog.String("schema", input.Schema), slog.String("version", input.Version), slog.String("directory", input.Directory), slog.String("operator", input.Operator))

	actOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    1 * time.Minute,
			MaximumAttempts:    5,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, actOpts)

	result := &types.BaselineResult{
		Version:   input.Version,
		Directory: input.Directory,
		Schema:    input.Schema,
		Operator:  input.Operator,
		Status:    "running",
	}
	if err := workflow.SetQueryHandler(ctx, QueryBaselineStatus, func() (*types.BaselineResult, error) {
		return result, nil
	}); err != nil {
		return nil, fmt.Errorf("register baseline query handler: %w", err)
	}

	migrationInput := types.MigrationInput{
		DSN:                       input.DSN,
		Schema:                    input.Schema,
		AllowInitialize:           input.AllowInitialize,
		ExpectedPgrollVersion:     input.ExpectedPgrollVersion,
		RequireExactPgrollVersion: input.RequireExactPgrollVersion,
	}
	if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).CheckPgrollVersion, migrationInput).Get(ctx, &result.PgrollVersion); err != nil {
		return nil, err
	}
	var readiness types.PgrollReadiness
	if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).CheckPgrollReadiness, migrationInput).Get(ctx, &readiness); err != nil {
		return nil, err
	}
	if readiness.Message != "" {
		logger.Info("baseline preflight complete", slog.String("message", readiness.Message))
	}
	if err := workflow.ExecuteActivity(ctx, (*activities.PgrollActivities).CreateBaseline, input).Get(ctx, result); err != nil {
		return nil, err
	}

	logger.Info("BaselineWorkflow completed", slog.String("schema", result.Schema), slog.String("version", result.Version), slog.String("status", result.Status), slog.String("directory", result.Directory), slog.String("pgroll_version", result.PgrollVersion))
	return result, nil
}
