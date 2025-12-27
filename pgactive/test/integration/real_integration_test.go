package integration

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"company.com/infra/pgactive-upgrade/internal/activities"
	"company.com/infra/pgactive-upgrade/internal/types"
	"company.com/infra/pgactive-upgrade/internal/workflow"

	_ "github.com/lib/pq"
)

type RealIntegrationTestSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
	
	logger        *slog.Logger
	pgContainer   *postgres.PostgresContainer
	sourceDB      *sql.DB
	targetDB      *sql.DB
	
	// Test environment
	env *testsuite.TestWorkflowEnvironment
}

func TestRealIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(RealIntegrationTestSuite))
}

func (s *RealIntegrationTestSuite) SetupSuite() {
	s.logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	ctx := context.Background()

	// Start PostgreSQL container for testing
	pgContainer, err := postgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:15.4"),
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(5*time.Minute)),
	)
	require.NoError(s.T(), err)
	s.pgContainer = pgContainer

	// Get connection string
	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(s.T(), err)

	// Connect to PostgreSQL
	db, err := sql.Open("postgres", connStr)
	require.NoError(s.T(), err)
	s.sourceDB = db

	// Create test schema and data
	err = s.setupTestDatabase()
	require.NoError(s.T(), err)

	s.logger.Info("Real integration test suite setup complete")
}

func (s *RealIntegrationTestSuite) TearDownSuite() {
	if s.sourceDB != nil {
		s.sourceDB.Close()
	}
	if s.targetDB != nil {
		s.targetDB.Close()
	}
	if s.pgContainer != nil {
		_ = s.pgContainer.Terminate(context.Background())
	}
}

func (s *RealIntegrationTestSuite) SetupTest() {
	// Create a new test environment for each test
	s.env = s.NewTestWorkflowEnvironment()
	
	// Register the workflow
	s.env.RegisterWorkflow(workflow.RollingUpgradeWorkflow)
	
	// Create real activities (but with mock AWS calls)
	realActivities := &RealTestActivities{
		logger:   s.logger,
		sourceDB: s.sourceDB,
		targetDB: s.sourceDB, // For testing, use same DB
	}
	
	// Register real activities
	s.env.RegisterActivity(realActivities.ValidateInput)
	s.env.RegisterActivity(realActivities.ProvisionTargetDB)
	s.env.RegisterActivity(realActivities.ConfigurePgactiveParams)
	s.env.RegisterActivity(realActivities.InstallPgactiveExtension)
	s.env.RegisterActivity(realActivities.WaitForSync)
	s.env.RegisterActivity(realActivities.TrafficShiftPhase)
	s.env.RegisterActivity(realActivities.RunHealthChecks)
	s.env.RegisterActivity(realActivities.Cutover)
	s.env.RegisterActivity(realActivities.OptionallyDetachOld)
	s.env.RegisterActivity(realActivities.DecommissionSource)
	s.env.RegisterActivity(realActivities.ExecuteRollback)
	
	// Register child workflow
	s.env.RegisterWorkflow(realActivities.InitReplicationGroupWorkflow)
}

func (s *RealIntegrationTestSuite) TearDownTest() {
	s.env.AssertExpectations(s.T())
}

func (s *RealIntegrationTestSuite) TestRealWorkflowExecution() {
	// Test input
	input := types.UpgradeInput{
		SourceDBInstanceID: "test-source-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{50, 50},
		Subnets:           []string{"subnet-test"},
		SecurityGroupIDs:  []string{"sg-test"},
		InstanceClass:     "db.t3.micro",
		Tags: map[string]string{
			"Environment": "test",
			"Purpose":     "integration-test",
		},
	}

	// Execute workflow
	s.env.ExecuteWorkflow(workflow.RollingUpgradeWorkflow, input)

	// Verify workflow completed
	require.True(s.T(), s.env.IsWorkflowCompleted())

	// Get result
	var result types.ProgressResponse
	err := s.env.GetWorkflowResult(&result)
	require.NoError(s.T(), err)

	// Verify result
	s.Equal("completed", result.Status)
	s.Equal(100, result.Percent)
	s.Equal("decommissioning", result.Phase)
}

func (s *RealIntegrationTestSuite) TestRealWorkflowWithDatabaseOperations() {
	// Create a test table first
	_, err := s.sourceDB.Exec(`
		CREATE TABLE IF NOT EXISTS test_upgrade_log (
			id SERIAL PRIMARY KEY,
			operation VARCHAR(100),
			timestamp TIMESTAMP DEFAULT NOW(),
			details TEXT
		)
	`)
	require.NoError(s.T(), err)

	input := types.UpgradeInput{
		SourceDBInstanceID: "test-db-ops",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
		InstanceClass:     "db.t3.micro",
	}

	// Execute workflow
	s.env.ExecuteWorkflow(workflow.RollingUpgradeWorkflow, input)

	// Verify workflow completed
	require.True(s.T(), s.env.IsWorkflowCompleted())

	// Check that operations were logged to the database
	var count int
	err = s.sourceDB.QueryRow("SELECT COUNT(*) FROM test_upgrade_log").Scan(&count)
	require.NoError(s.T(), err)
	s.Greater(count, 0, "Expected database operations to be logged")

	// Verify specific operations were logged
	var operations []string
	rows, err := s.sourceDB.Query("SELECT operation FROM test_upgrade_log ORDER BY timestamp")
	require.NoError(s.T(), err)
	defer rows.Close()

	for rows.Next() {
		var op string
		err := rows.Scan(&op)
		require.NoError(s.T(), err)
		operations = append(operations, op)
	}

	// Verify we have the expected operations
	s.Contains(operations, "validate_input")
	s.Contains(operations, "provision_target")
	s.Contains(operations, "cutover")
}

func (s *RealIntegrationTestSuite) TestRealWorkflowRollback() {
	input := types.UpgradeInput{
		SourceDBInstanceID: "test-rollback-db",
		TargetVersion:      "15.4",
		ShiftPercentages:   []int{100},
		InstanceClass:     "db.t3.micro",
	}

	// Set environment variable to trigger rollback
	os.Setenv("TEST_TRIGGER_ROLLBACK", "true")
	defer os.Unsetenv("TEST_TRIGGER_ROLLBACK")

	// Execute workflow
	s.env.ExecuteWorkflow(workflow.RollingUpgradeWorkflow, input)

	// Verify workflow completed
	require.True(s.T(), s.env.IsWorkflowCompleted())

	// Get result
	var result types.ProgressResponse
	err := s.env.GetWorkflowResult(&result)
	require.NoError(s.T(), err)

	// Verify rollback occurred
	s.Equal("rolled_back", result.Status)
}

func (s *RealIntegrationTestSuite) setupTestDatabase() error {
	// Create test tables and sample data
	queries := []string{
		`CREATE TABLE IF NOT EXISTS test_upgrade_log (
			id SERIAL PRIMARY KEY,
			operation VARCHAR(100),
			timestamp TIMESTAMP DEFAULT NOW(),
			details TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS test_data (
			id SERIAL PRIMARY KEY,
			name VARCHAR(100),
			value INTEGER,
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		`INSERT INTO test_data (name, value) VALUES 
			('test1', 100),
			('test2', 200),
			('test3', 300)
		ON CONFLICT DO NOTHING`,
	}

	for _, query := range queries {
		if _, err := s.sourceDB.Exec(query); err != nil {
			return fmt.Errorf("failed to execute query: %w", err)
		}
	}

	return nil
}

// RealTestActivities implements activities that interact with real databases
// but mock AWS operations
type RealTestActivities struct {
	logger   *slog.Logger
	sourceDB *sql.DB
	targetDB *sql.DB
}

func (a *RealTestActivities) ValidateInput(ctx context.Context, input types.UpgradeInput) error {
	a.logger.Info("Real ValidateInput", slog.String("source_db", input.SourceDBInstanceID))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"validate_input", fmt.Sprintf("source_db: %s, version: %s", input.SourceDBInstanceID, input.TargetVersion),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	// Simulate validation
	if input.SourceDBInstanceID == "" {
		return fmt.Errorf("source DB instance ID is required")
	}
	if input.TargetVersion == "" {
		return fmt.Errorf("target version is required")
	}

	return nil
}

func (a *RealTestActivities) ProvisionTargetDB(ctx context.Context, input types.UpgradeInput) (string, error) {
	a.logger.Info("Real ProvisionTargetDB", slog.String("source_db", input.SourceDBInstanceID))
	
	targetID := fmt.Sprintf("%s-target-%d", input.SourceDBInstanceID, time.Now().Unix())
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"provision_target", fmt.Sprintf("target_id: %s, instance_class: %s", targetID, input.InstanceClass),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	// Simulate DB provisioning time
	time.Sleep(100 * time.Millisecond)
	
	return targetID, nil
}

func (a *RealTestActivities) ConfigurePgactiveParams(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Real ConfigurePgactiveParams", slog.String("target_db", input.TargetDBInstanceID))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"configure_params", fmt.Sprintf("target_db: %s", input.TargetDBInstanceID),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	return nil
}

func (a *RealTestActivities) InstallPgactiveExtension(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Real InstallPgactiveExtension", slog.String("target_db", input.TargetDBInstanceID))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"install_extension", fmt.Sprintf("target_db: %s", input.TargetDBInstanceID),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	return nil
}

func (a *RealTestActivities) WaitForSync(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Real WaitForSync", slog.String("replication_group", input.ReplicationGroup))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"wait_sync", fmt.Sprintf("replication_group: %s", input.ReplicationGroup),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	// Simulate sync wait
	time.Sleep(50 * time.Millisecond)
	return nil
}

func (a *RealTestActivities) TrafficShiftPhase(ctx context.Context, input types.TrafficShiftInput) error {
	a.logger.Info("Real TrafficShiftPhase", 
		slog.Int("phase", input.Phase),
		slog.Int("percentage", input.ShiftPercentage))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"traffic_shift", fmt.Sprintf("phase: %d, percentage: %d", input.Phase, input.ShiftPercentage),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	return nil
}

func (a *RealTestActivities) RunHealthChecks(ctx context.Context, input types.HealthCheckInput) error {
	a.logger.Info("Real RunHealthChecks", slog.String("check_type", input.CheckType))
	
	// Check for test rollback trigger
	if os.Getenv("TEST_TRIGGER_ROLLBACK") == "true" {
		return activities.NewHealthCheckError("test-triggered health check failure")
	}
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"health_check", fmt.Sprintf("check_type: %s", input.CheckType),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	return nil
}

func (a *RealTestActivities) Cutover(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Real Cutover", slog.String("target_db", input.TargetDBInstanceID))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"cutover", fmt.Sprintf("target_db: %s", input.TargetDBInstanceID),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	return nil
}

func (a *RealTestActivities) OptionallyDetachOld(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Real OptionallyDetachOld", slog.String("source_db", input.SourceDBInstanceID))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"detach_old", fmt.Sprintf("source_db: %s", input.SourceDBInstanceID),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	return nil
}

func (a *RealTestActivities) DecommissionSource(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Real DecommissionSource", slog.String("source_db", input.SourceDBInstanceID))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"decommission", fmt.Sprintf("source_db: %s", input.SourceDBInstanceID),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	return nil
}

func (a *RealTestActivities) ExecuteRollback(ctx context.Context, input types.ActivityInput) error {
	a.logger.Info("Real ExecuteRollback", slog.String("source_db", input.SourceDBInstanceID))
	
	// Log to database
	_, err := a.sourceDB.Exec(
		"INSERT INTO test_upgrade_log (operation, details) VALUES ($1, $2)",
		"rollback", fmt.Sprintf("source_db: %s", input.SourceDBInstanceID),
	)
	if err != nil {
		a.logger.Warn("Failed to log to database", slog.String("error", err.Error()))
	}

	return nil
}

func (a *RealTestActivities) InitReplicationGroupWorkflow(ctx temporalworkflow.Context, input types.ActivityInput) (string, error) {
	logger := temporalworkflow.GetLogger(ctx)
	logger.Info("Real InitReplicationGroupWorkflow", "source_db", input.SourceDBInstanceID)
	
	groupID := fmt.Sprintf("repl-group-%s-%d", input.SourceDBInstanceID, temporalworkflow.Now(ctx).Unix())
	
	// This is a simple child workflow that just returns a group ID
	// In a real implementation, this would orchestrate replication setup
	
	return groupID, nil
}
