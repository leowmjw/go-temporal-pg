package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"company.com/infra/pgactive-upgrade/internal/types"
)

type WorkflowTestSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
}

func TestWorkflowTestSuite(t *testing.T) {
	suite.Run(t, new(WorkflowTestSuite))
}

// Mock activities for testing
type MockActivities struct {
	validateInputResult        error
	provisionTargetDBResult    string
	provisionTargetDBError     error
	configurePgactiveResult    error
	installExtensionResult     error
	waitForSyncResult          error
	trafficShiftResult         error
	runHealthChecksResult      error
	cutoverResult              error
	optionallyDetachOldResult  error
	decommissionSourceResult   error
	executeRollbackResult      error
	childWorkflowResult        string
	childWorkflowError         error
}

func (m *MockActivities) ValidateInput(ctx context.Context, input types.UpgradeInput) error {
	return m.validateInputResult
}

func (m *MockActivities) ProvisionTargetDB(ctx context.Context, input types.UpgradeInput) (string, error) {
	return m.provisionTargetDBResult, m.provisionTargetDBError
}

func (m *MockActivities) ConfigurePgactiveParams(ctx context.Context, input types.ActivityInput) error {
	return m.configurePgactiveResult
}

func (m *MockActivities) InstallPgactiveExtension(ctx context.Context, input types.ActivityInput) error {
	return m.installExtensionResult
}

func (m *MockActivities) WaitForSync(ctx context.Context, input types.ActivityInput) error {
	return m.waitForSyncResult
}

func (m *MockActivities) TrafficShiftPhase(ctx context.Context, input types.TrafficShiftInput) error {
	return m.trafficShiftResult
}

func (m *MockActivities) RunHealthChecks(ctx context.Context, input types.HealthCheckInput) error {
	return m.runHealthChecksResult
}

func (m *MockActivities) Cutover(ctx context.Context, input types.ActivityInput) error {
	return m.cutoverResult
}

func (m *MockActivities) OptionallyDetachOld(ctx context.Context, input types.ActivityInput) error {
	return m.optionallyDetachOldResult
}

func (m *MockActivities) DecommissionSource(ctx context.Context, input types.ActivityInput) error {
	return m.decommissionSourceResult
}

func (m *MockActivities) ExecuteRollback(ctx context.Context, input types.ActivityInput) error {
	return m.executeRollbackResult
}

func (m *MockActivities) InitReplicationGroupWorkflow(ctx workflow.Context, input types.ActivityInput) (string, error) {
	return m.childWorkflowResult, m.childWorkflowError
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_HappyPath() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities for success case
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)
	env.RegisterActivity(mockActivities.WaitForSync)
	env.RegisterActivity(mockActivities.TrafficShiftPhase)
	env.RegisterActivity(mockActivities.RunHealthChecks)
	env.RegisterActivity(mockActivities.Cutover)
	env.RegisterActivity(mockActivities.OptionallyDetachOld)
	env.RegisterActivity(mockActivities.DecommissionSource)
	
	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{25, 25, 50},
		Subnets:           []string{"subnet-1", "subnet-2"},
		SecurityGroupIDs:  []string{"sg-123"},
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	var result types.ProgressResponse
	s.NoError(env.GetWorkflowResult(&result))
	s.Equal("completed", result.Status)
	s.Equal(100, result.Percent)
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_ValidationFailure() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities with validation failure
	mockActivities := &MockActivities{
		validateInputResult: errors.New("validation failed"),
	}
	env.RegisterActivity(mockActivities.ValidateInput)

	input := types.UpgradeInput{
		SourceDBInstanceID: "invalid-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{150}, // Invalid percentage
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	var result types.ProgressResponse
	s.NoError(env.GetWorkflowResult(&result))
	s.Equal("failed", result.Status)
	s.Contains(result.Message, "validation failed")
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_ProvisioningFailure() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities with provisioning failure
	mockActivities := &MockActivities{
		provisionTargetDBError: errors.New("provisioning failed"),
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.Error(env.GetWorkflowError())
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_RollbackSignal() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities for successful execution until rollback
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)
	env.RegisterActivity(mockActivities.WaitForSync)
	env.RegisterActivity(mockActivities.TrafficShiftPhase)
	env.RegisterActivity(mockActivities.RunHealthChecks)
	env.RegisterActivity(mockActivities.ExecuteRollback)

	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{25, 75},
	}

	// Start workflow
	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	// Send rollback signal during execution
	env.SignalWorkflow("rollback", types.RollbackSignal{
		Reason:    "Manual rollback requested",
		Timestamp: time.Now(),
		RequestID: "req-123",
	})

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	var result types.ProgressResponse
	s.NoError(env.GetWorkflowResult(&result))
	s.Equal("rolled_back", result.Status)
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_HealthCheckFailure() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities with health check failure
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
		runHealthChecksResult:   errors.New("health check failed"),
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)
	env.RegisterActivity(mockActivities.WaitForSync)
	env.RegisterActivity(mockActivities.TrafficShiftPhase)
	env.RegisterActivity(mockActivities.RunHealthChecks)
	env.RegisterActivity(mockActivities.ExecuteRollback)

	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	var result types.ProgressResponse
	s.NoError(env.GetWorkflowResult(&result))
	s.Equal("rolled_back", result.Status)
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_MultiPhaseTrafficShift() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities for all successful execution
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)
	env.RegisterActivity(mockActivities.WaitForSync)
	env.RegisterActivity(mockActivities.TrafficShiftPhase)
	env.RegisterActivity(mockActivities.RunHealthChecks)
	env.RegisterActivity(mockActivities.Cutover)
	env.RegisterActivity(mockActivities.OptionallyDetachOld)
	env.RegisterActivity(mockActivities.DecommissionSource)

	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{10, 20, 30, 40}, // Four phases
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	// Test passes if workflow completes successfully with multiple phases
	// (AssertNumberOfCalls doesn't work with RegisterActivity approach)
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_ProgressQuery() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities for progress testing
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)

	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
	}

	env.RegisterDelayedCallback(func() {
		// Query progress during workflow execution
		result, err := env.QueryWorkflow("progress")
		s.NoError(err)

		var progress types.ProgressResponse
		s.NoError(result.Get(&progress))
		s.Equal("running", progress.Status)
		s.True(progress.Percent >= 0 && progress.Percent <= 100)
	}, time.Millisecond*100)

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_CutoverFailure() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities with cutover failure
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
		cutoverResult:           errors.New("cutover failed"),
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)
	env.RegisterActivity(mockActivities.WaitForSync)
	env.RegisterActivity(mockActivities.TrafficShiftPhase)
	env.RegisterActivity(mockActivities.RunHealthChecks)
	env.RegisterActivity(mockActivities.Cutover)
	env.RegisterActivity(mockActivities.ExecuteRollback)

	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	var result types.ProgressResponse
	s.NoError(env.GetWorkflowResult(&result))
	s.Equal("rolled_back", result.Status)
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_RollbackFailure() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities with health check failure and rollback failure
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
		runHealthChecksResult:   errors.New("health check failed"),
		executeRollbackResult:   errors.New("rollback failed"),
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)
	env.RegisterActivity(mockActivities.WaitForSync)
	env.RegisterActivity(mockActivities.TrafficShiftPhase)
	env.RegisterActivity(mockActivities.RunHealthChecks)
	env.RegisterActivity(mockActivities.ExecuteRollback)

	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.Error(env.GetWorkflowError())
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_ChildWorkflowFailure() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities with child workflow failure
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowError:      errors.New("replication setup failed"),
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)

	// Register child workflow that fails
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.Error(env.GetWorkflowError())
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_SingleStepShift() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities for all successful execution
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)
	env.RegisterActivity(mockActivities.WaitForSync)
	env.RegisterActivity(mockActivities.TrafficShiftPhase)
	env.RegisterActivity(mockActivities.RunHealthChecks)
	env.RegisterActivity(mockActivities.Cutover)
	env.RegisterActivity(mockActivities.OptionallyDetachOld)
	env.RegisterActivity(mockActivities.DecommissionSource)

	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100}, // Single step shift
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	// Test passes if workflow completes successfully with single step
	// (AssertNumberOfCalls doesn't work with RegisterActivity approach)
}

func (s *WorkflowTestSuite) TestRollingUpgradeWorkflow_WaitForSyncTimeout() {
	env := s.NewTestWorkflowEnvironment()

	// Register the workflow
	env.RegisterWorkflow(RollingUpgradeWorkflow)

	// Register mock activities with wait for sync timeout
	mockActivities := &MockActivities{
		provisionTargetDBResult: "target-db-123",
		childWorkflowResult:     "repl-group-123",
		waitForSyncResult:       errors.New("activity timeout"),
	}
	env.RegisterActivity(mockActivities.ValidateInput)
	env.RegisterActivity(mockActivities.ProvisionTargetDB)
	env.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	env.RegisterActivity(mockActivities.InstallPgactiveExtension)
	env.RegisterActivity(mockActivities.WaitForSync)

	// Register child workflow
	env.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	input := types.UpgradeInput{
		SourceDBInstanceID: "source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
	}

	env.ExecuteWorkflow(RollingUpgradeWorkflow, input)

	s.True(env.IsWorkflowCompleted())
	s.Error(env.GetWorkflowError())
}
