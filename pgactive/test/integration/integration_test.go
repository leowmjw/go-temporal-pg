package integration

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"company.com/infra/pgactive-upgrade/internal/activities"
	"company.com/infra/pgactive-upgrade/internal/types"
	"company.com/infra/pgactive-upgrade/internal/workflow"
)

type IntegrationTestSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
	client    client.Client
	worker    worker.Worker
	logger    *slog.Logger
}

func TestIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(IntegrationTestSuite))
}

func (s *IntegrationTestSuite) SetupSuite() {
	s.logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Setup test Temporal client and worker
	var err error
	s.client, err = client.Dial(client.Options{
		HostPort:  "localhost:7233", // Default Temporal dev server
		Namespace: "default",
	})
	if err != nil {
		s.T().Skip("Temporal server not available for integration tests")
		return
	}

	// Create worker with mock activities for integration testing
	s.worker = worker.New(s.client, workflow.TaskQueue, worker.Options{})
	
	// Register workflow
	s.worker.RegisterWorkflow(workflow.RollingUpgradeWorkflow)
	
	// Register mock activities that simulate real behavior
	mockActivities := &MockIntegrationActivities{logger: s.logger}
	s.worker.RegisterActivity(mockActivities.ValidateInput)
	s.worker.RegisterActivity(mockActivities.ProvisionTargetDB)
	s.worker.RegisterActivity(mockActivities.ConfigurePgactiveParams)
	s.worker.RegisterActivity(mockActivities.InstallPgactiveExtension)
	s.worker.RegisterActivity(mockActivities.WaitForSync)
	s.worker.RegisterActivity(mockActivities.TrafficShiftPhase)
	s.worker.RegisterActivity(mockActivities.RunHealthChecks)
	s.worker.RegisterActivity(mockActivities.Cutover)
	s.worker.RegisterActivity(mockActivities.OptionallyDetachOld)
	s.worker.RegisterActivity(mockActivities.DecommissionSource)
	s.worker.RegisterActivity(mockActivities.ExecuteRollback)
	
	// Register child workflow
	s.worker.RegisterWorkflow(mockActivities.InitReplicationGroupWorkflow)

	// Start worker
	err = s.worker.Start()
	require.NoError(s.T(), err)
}

func (s *IntegrationTestSuite) TearDownSuite() {
	if s.worker != nil {
		s.worker.Stop()
	}
	if s.client != nil {
		s.client.Close()
	}
}

func (s *IntegrationTestSuite) TestCompleteUpgradeFlow() {
	ctx := context.Background()
	
	workflowOptions := client.StartWorkflowOptions{
		ID:        "test-upgrade-" + time.Now().Format("20060102-150405"),
		TaskQueue: workflow.TaskQueue,
	}

	input := types.UpgradeInput{
		SourceDBInstanceID: "test-source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{25, 25, 50},
		Subnets:           []string{"subnet-1", "subnet-2"},
		SecurityGroupIDs:  []string{"sg-123"},
		Tags: map[string]string{
			"Environment": "test",
			"Purpose":     "upgrade",
		},
	}

	we, err := s.client.ExecuteWorkflow(ctx, workflowOptions, workflow.WorkflowName, input)
	require.NoError(s.T(), err)

	// Test progress queries during execution
	go func() {
		time.Sleep(500 * time.Millisecond)
		progress, err := s.client.QueryWorkflow(ctx, we.GetID(), we.GetRunID(), "progress")
		if err == nil {
			var result types.ProgressResponse
			progress.Get(&result)
			s.logger.Info("Progress query result", 
				slog.String("phase", result.Phase),
				slog.Int("percent", result.Percent),
				slog.String("status", result.Status))
		}
	}()

	// Wait for completion
	var result types.ProgressResponse
	err = we.Get(ctx, &result)
	require.NoError(s.T(), err)

	s.Equal("completed", result.Status)
	s.Equal(100, result.Percent)
	s.Equal("decommissioning", result.Phase)
}

func (s *IntegrationTestSuite) TestUpgradeWithRollback() {
	ctx := context.Background()
	
	workflowOptions := client.StartWorkflowOptions{
		ID:        "test-rollback-" + time.Now().Format("20060102-150405"),
		TaskQueue: workflow.TaskQueue,
	}

	input := types.UpgradeInput{
		SourceDBInstanceID: "test-rollback-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{50, 50},
	}

	we, err := s.client.ExecuteWorkflow(ctx, workflowOptions, workflow.WorkflowName, input)
	require.NoError(s.T(), err)

	// Send rollback signal after a short delay
	go func() {
		time.Sleep(2 * time.Second)
		err := s.client.SignalWorkflow(ctx, we.GetID(), we.GetRunID(), "rollback", types.RollbackSignal{
			Reason:    "Integration test rollback",
			Timestamp: time.Now(),
			RequestID: "integration-test-req",
		})
		if err != nil {
			s.logger.Error("Failed to send rollback signal", slog.String("error", err.Error()))
		}
	}()

	// Wait for completion
	var result types.ProgressResponse
	err = we.Get(ctx, &result)
	require.NoError(s.T(), err)

	s.Equal("rolled_back", result.Status)
	s.Contains(result.Message, "rolled back")
}

func (s *IntegrationTestSuite) TestUpgradeWithFailedHealthCheck() {
	ctx := context.Background()
	
	workflowOptions := client.StartWorkflowOptions{
		ID:        "test-health-fail-" + time.Now().Format("20060102-150405"),
		TaskQueue: workflow.TaskQueue,
	}

	// Set environment variable to trigger health check failure
	os.Setenv("INTEGRATION_TEST_FAIL_HEALTH_CHECK", "true")
	defer os.Unsetenv("INTEGRATION_TEST_FAIL_HEALTH_CHECK")

	input := types.UpgradeInput{
		SourceDBInstanceID: "test-health-fail-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
	}

	we, err := s.client.ExecuteWorkflow(ctx, workflowOptions, workflow.WorkflowName, input)
	require.NoError(s.T(), err)

	// Wait for completion
	var result types.ProgressResponse
	err = we.Get(ctx, &result)
	require.NoError(s.T(), err)

	s.Equal("rolled_back", result.Status)
}

func (s *IntegrationTestSuite) TestConcurrentUpgrades() {
	ctx := context.Background()
	
	// Start multiple concurrent upgrades
	var workflows []client.WorkflowRun
	
	for i := 0; i < 3; i++ {
		workflowOptions := client.StartWorkflowOptions{
			ID:        "concurrent-upgrade-" + time.Now().Format("20060102-150405") + "-" + string(rune('A'+i)),
			TaskQueue: workflow.TaskQueue,
		}

		input := types.UpgradeInput{
			SourceDBInstanceID: "concurrent-source-" + string(rune('A'+i)),
			TargetVersion:      "15.4",
			ShiftPercentages:   []int{100},
		}

		we, err := s.client.ExecuteWorkflow(ctx, workflowOptions, workflow.WorkflowName, input)
		require.NoError(s.T(), err)
		workflows = append(workflows, we)
	}

	// Wait for all to complete
	for i, we := range workflows {
		var result types.ProgressResponse
		err := we.Get(ctx, &result)
		require.NoError(s.T(), err, "Workflow %d failed", i)
		s.Equal("completed", result.Status, "Workflow %d not completed", i)
	}
}

// Mock activities for integration testing that simulate real behavior
type MockIntegrationActivities struct {
	logger *slog.Logger
}

func (a *MockIntegrationActivities) ValidateInput(ctx context.Context, input types.UpgradeInput) error {
	a.logger.Info("Mock ValidateInput", slog.String("source_db", input.SourceDBInstanceID))
	time.Sleep(100 * time.Millisecond) // Simulate network call
	return nil
}

func (a *MockIntegrationActivities) ProvisionTargetDB(ctx context.Context, input types.UpgradeInput) (string, error) {
	a.logger.Info("Mock ProvisionTargetDB", slog.String("source_db", input.SourceDBInstanceID))
	time.Sleep(2 * time.Second) // Simulate DB creation time
	return input.SourceDBInstanceID + "-target", nil
}

func (a *MockIntegrationActivities) ConfigurePgactiveParams(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Mock ConfigurePgactiveParams", slog.String("target_db", input.TargetDBInstanceID))
	time.Sleep(1 * time.Second) // Simulate parameter configuration and reboot
	return nil
}

func (a *MockIntegrationActivities) InstallPgactiveExtension(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Mock InstallPgactiveExtension", slog.String("target_db", input.TargetDBInstanceID))
	time.Sleep(500 * time.Millisecond) // Simulate extension installation
	return nil
}

func (a *MockIntegrationActivities) WaitForSync(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Mock WaitForSync", slog.String("replication_group", input.ReplicationGroup))
	
	// Simulate gradual sync with heartbeats
	for i := 0; i < 5; i++ {
		time.Sleep(200 * time.Millisecond)
		// In real activity, this would send heartbeats
	}
	return nil
}

func (a *MockIntegrationActivities) TrafficShiftPhase(ctx context.Context, input types.TrafficShiftInput) error {
	a.logger.Info("Mock TrafficShiftPhase", 
		slog.Int("phase", input.Phase),
		slog.Int("percentage", input.ShiftPercentage))
	time.Sleep(300 * time.Millisecond) // Simulate traffic shift
	return nil
}

func (a *MockIntegrationActivities) RunHealthChecks(ctx context.Context, input types.HealthCheckInput) error {
	a.logger.Info("Mock RunHealthChecks", slog.String("check_type", input.CheckType))
	
	// Check for test environment variable to simulate failure
	if os.Getenv("INTEGRATION_TEST_FAIL_HEALTH_CHECK") == "true" {
		return activities.NewHealthCheckError("simulated health check failure")
	}
	
	time.Sleep(200 * time.Millisecond) // Simulate health check
	return nil
}

func (a *MockIntegrationActivities) Cutover(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Mock Cutover", slog.String("target_db", input.TargetDBInstanceID))
	time.Sleep(100 * time.Millisecond) // Simulate cutover
	return nil
}

func (a *MockIntegrationActivities) OptionallyDetachOld(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Mock OptionallyDetachOld", slog.String("source_db", input.SourceDBInstanceID))
	time.Sleep(200 * time.Millisecond) // Simulate detach
	return nil
}

func (a *MockIntegrationActivities) DecommissionSource(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Mock DecommissionSource", slog.String("source_db", input.SourceDBInstanceID))
	time.Sleep(500 * time.Millisecond) // Simulate decommission
	return nil
}

func (a *MockIntegrationActivities) ExecuteRollback(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Mock ExecuteRollback", slog.String("source_db", input.SourceDBInstanceID))
	time.Sleep(1 * time.Second) // Simulate rollback
	return nil
}

func (a *MockIntegrationActivities) InitReplicationGroupWorkflow(ctx context.Context, input types.ActivityInput) (string, error) {
	a.logger.Info("Mock InitReplicationGroupWorkflow", slog.String("source_db", input.SourceDBInstanceID))
	time.Sleep(500 * time.Millisecond) // Simulate replication group setup
	return "mock-replication-group-" + input.SourceDBInstanceID, nil
}

// Custom error type for health check failures
type HealthCheckError struct {
	Message string
}

func (e *HealthCheckError) Error() string {
	return e.Message
}

func NewHealthCheckError(message string) *HealthCheckError {
	return &HealthCheckError{Message: message}
}
