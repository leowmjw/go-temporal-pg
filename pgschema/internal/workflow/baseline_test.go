package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

func TestBaselineWorkflow_Success(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(BaselineWorkflow)

	pgroll := &activities.PgrollActivities{
		VersionFn: func(_ context.Context, _ types.MigrationInput) (string, error) { return "v0.16.2", nil },
		ReadinessFn: func(_ context.Context, _ types.MigrationInput) (*types.PgrollReadiness, error) {
			return &types.PgrollReadiness{Initialized: true}, nil
		},
		BaselineFn: func(_ context.Context, input types.BaselineInput) (*types.BaselineResult, error) {
			return &types.BaselineResult{
				Version:       input.Version,
				Directory:     input.Directory,
				Schema:        input.Schema,
				Operator:      input.Operator,
				Status:        "created",
				PgrollVersion: "v0.16.2",
			}, nil
		},
	}
	env.RegisterActivity(pgroll)

	env.ExecuteWorkflow(BaselineWorkflow, types.BaselineInput{
		Schema:    "public",
		Version:   "01_baseline",
		Directory: "/tmp/baseline",
		Operator:  "tester",
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result types.BaselineResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "created", result.Status)
	require.Equal(t, "v0.16.2", result.PgrollVersion)
}

func TestBaselineWorkflow_AlreadyBaselined(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(BaselineWorkflow)

	pgroll := &activities.PgrollActivities{
		VersionFn: func(_ context.Context, _ types.MigrationInput) (string, error) { return "v0.16.2", nil },
		ReadinessFn: func(_ context.Context, _ types.MigrationInput) (*types.PgrollReadiness, error) {
			return &types.PgrollReadiness{Initialized: true}, nil
		},
		BaselineFn: func(_ context.Context, input types.BaselineInput) (*types.BaselineResult, error) {
			return &types.BaselineResult{
				Version:       input.Version,
				Directory:     input.Directory,
				Schema:        input.Schema,
				Status:        "already_baselined",
				PgrollVersion: "v0.16.2",
			}, nil
		},
	}
	env.RegisterActivity(pgroll)

	env.ExecuteWorkflow(BaselineWorkflow, types.BaselineInput{Schema: "public", Version: "01_baseline", Directory: "/tmp/baseline"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result types.BaselineResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "already_baselined", result.Status)
}

func TestBaselineWorkflow_ReadinessFailure(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(BaselineWorkflow)

	pgroll := &activities.PgrollActivities{
		VersionFn: func(_ context.Context, _ types.MigrationInput) (string, error) { return "v0.16.2", nil },
		ReadinessFn: func(_ context.Context, _ types.MigrationInput) (*types.PgrollReadiness, error) {
			return nil, errors.New("pgroll metadata missing")
		},
	}
	env.RegisterActivity(pgroll)

	env.ExecuteWorkflow(BaselineWorkflow, types.BaselineInput{Schema: "public", Version: "01_baseline", Directory: "/tmp/baseline"})
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
}
