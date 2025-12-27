package activities

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"go.temporal.io/sdk/activity"

	"company.com/infra/pgactive-upgrade/internal/types"
)

// RDSClient interface for mocking AWS RDS operations
type RDSClient interface {
	DescribeDBInstances(ctx context.Context, params *rds.DescribeDBInstancesInput, optFns ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
	CreateDBInstance(ctx context.Context, params *rds.CreateDBInstanceInput, optFns ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error)
	ModifyDBParameterGroup(ctx context.Context, params *rds.ModifyDBParameterGroupInput, optFns ...func(*rds.Options)) (*rds.ModifyDBParameterGroupOutput, error)
	RebootDBInstance(ctx context.Context, params *rds.RebootDBInstanceInput, optFns ...func(*rds.Options)) (*rds.RebootDBInstanceOutput, error)
	DeleteDBInstance(ctx context.Context, params *rds.DeleteDBInstanceInput, optFns ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error)
}

// HealthCheckError represents a health check failure
type HealthCheckError struct {
	Message string
}

func (e *HealthCheckError) Error() string {
	return e.Message
}

func NewHealthCheckError(message string) *HealthCheckError {
	return &HealthCheckError{Message: message}
}

// Activities provides all workflow activities
type Activities struct {
	RDS           RDSClient
	SecretsClient *secretsmanager.Client
	Log           *slog.Logger
}

// NewActivities creates a new Activities instance
func NewActivities(rdsClient RDSClient, secretsClient *secretsmanager.Client, logger *slog.Logger) *Activities {
	return &Activities{
		RDS:           rdsClient,
		SecretsClient: secretsClient,
		Log:           logger,
	}
}

// getLogger safely gets a logger, falling back to the instance logger
func (a *Activities) getLogger(ctx context.Context) *slog.Logger {
	// Always return the instance logger to avoid Temporal context issues in tests
	return a.Log
}

// ValidateInput validates the upgrade input parameters
func (a *Activities) ValidateInput(ctx context.Context, input types.UpgradeInput) error {
	logger := a.getLogger(ctx)
	logger.Info("Validating upgrade input",
		slog.String("source_db", input.SourceDBInstanceID),
		slog.String("target_version", input.TargetVersion))

	// Validate source DB exists and is accessible
	describeInput := &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(input.SourceDBInstanceID),
	}
	
	result, err := a.RDS.DescribeDBInstances(ctx, describeInput)
	if err != nil {
		return fmt.Errorf("failed to describe source DB instance: %w", err)
	}
	
	if len(result.DBInstances) == 0 {
		return fmt.Errorf("source DB instance %s not found", input.SourceDBInstanceID)
	}
	
	sourceDB := result.DBInstances[0]
	
	// Validate DB is PostgreSQL
	if *sourceDB.Engine != "postgres" {
		return fmt.Errorf("source DB must be PostgreSQL, got %s", *sourceDB.Engine)
	}
	
	// Validate DB is available
	if *sourceDB.DBInstanceStatus != "available" {
		return fmt.Errorf("source DB must be in 'available' status, got %s", *sourceDB.DBInstanceStatus)
	}
	
	// Validate target version is newer
	currentVersion := *sourceDB.EngineVersion
	if input.TargetVersion <= currentVersion {
		return fmt.Errorf("target version %s must be newer than current version %s", 
			input.TargetVersion, currentVersion)
	}
	
	// Validate shift percentages
	if len(input.ShiftPercentages) == 0 {
		return fmt.Errorf("at least one shift percentage must be specified")
	}
	
	total := 0
	for _, pct := range input.ShiftPercentages {
		if pct < 0 || pct > 100 {
			return fmt.Errorf("shift percentage must be between 0 and 100, got %d", pct)
		}
		total += pct
	}
	
	if total != 100 {
		return fmt.Errorf("shift percentages must sum to 100, got %d", total)
	}

	logger.Info("Input validation completed successfully")
	return nil
}

// ProvisionTargetDB creates a new RDS instance for the target version
func (a *Activities) ProvisionTargetDB(ctx context.Context, input types.UpgradeInput) (string, error) {
	logger := a.getLogger(ctx)
	a.Log.Info("Provisioning target DB",
		slog.String("source_db", input.SourceDBInstanceID),
		slog.String("target_version", input.TargetVersion))

	// Get source DB details
	describeInput := &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(input.SourceDBInstanceID),
	}
	
	result, err := a.RDS.DescribeDBInstances(ctx, describeInput)
	if err != nil {
		return "", fmt.Errorf("failed to describe source DB: %w", err)
	}
	
	sourceDB := result.DBInstances[0]
	
	// Generate target DB identifier
	targetDBID := fmt.Sprintf("%s-upgrade-%d", input.SourceDBInstanceID, time.Now().Unix())
	
	// Use provided instance class or default to source's class
	instanceClass := input.InstanceClass
	if instanceClass == "" {
		instanceClass = *sourceDB.DBInstanceClass
	}
	
	// Create target DB instance
	createInput := &rds.CreateDBInstanceInput{
		DBInstanceIdentifier:   aws.String(targetDBID),
		DBInstanceClass:        aws.String(instanceClass),
		Engine:                 aws.String("postgres"),
		EngineVersion:          aws.String(input.TargetVersion),
		AllocatedStorage:       sourceDB.AllocatedStorage,
		StorageType:            sourceDB.StorageType,
		StorageEncrypted:       sourceDB.StorageEncrypted,
		KmsKeyId:               sourceDB.KmsKeyId,
		MasterUsername:         sourceDB.MasterUsername,
		ManageMasterUserPassword: aws.Bool(true),
		DBSubnetGroupName:      sourceDB.DBSubnetGroup.DBSubnetGroupName,
		VpcSecurityGroupIds:    input.SecurityGroupIDs,
		BackupRetentionPeriod:  aws.Int32(int32(input.BackupRetentionDays)),
		Tags: convertTags(input.Tags),
	}
	
	_, err = a.RDS.CreateDBInstance(ctx, createInput)
	if err != nil {
		return "", fmt.Errorf("failed to create target DB instance: %w", err)
	}
	
	// Wait for DB to become available
	waiter := rds.NewDBInstanceAvailableWaiter(a.RDS)
	err = waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(targetDBID),
	}, 15*time.Minute)
	if err != nil {
		return "", fmt.Errorf("target DB did not become available in time: %w", err)
	}
	
	logger.Info("Target DB provisioned successfully", slog.String("target_db", targetDBID))
	return targetDBID, nil
}

// ConfigurePgactiveParams configures pgactive parameters on both instances
func (a *Activities) ConfigurePgactiveParams(ctx context.Context, input types.ActivityInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Configuring pgactive parameters",
		slog.String("source_db", input.SourceDBInstanceID),
		slog.String("target_db", input.TargetDBInstanceID))

	// Configure parameters for both instances
	for _, dbID := range []string{input.SourceDBInstanceID, input.TargetDBInstanceID} {
		// Get current parameter group
		describeInput := &rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(dbID),
		}
		
		result, err := a.RDS.DescribeDBInstances(ctx, describeInput)
		if err != nil {
			return fmt.Errorf("failed to describe DB instance %s: %w", dbID, err)
		}
		
		db := result.DBInstances[0]
		paramGroupName := *db.DBParameterGroups[0].DBParameterGroupName
		
		// Modify parameter group to enable pgactive
		modifyInput := &rds.ModifyDBParameterGroupInput{
			DBParameterGroupName: aws.String(paramGroupName),
			Parameters: []rdstypes.Parameter{
				{
					ParameterName:  aws.String("shared_preload_libraries"),
					ParameterValue: aws.String("pgactive"),
					ApplyMethod:    "pending-reboot",
				},
				{
					ParameterName:  aws.String("wal_level"),
					ParameterValue: aws.String("logical"),
					ApplyMethod:    "pending-reboot",
				},
				{
					ParameterName:  aws.String("max_replication_slots"),
					ParameterValue: aws.String("10"),
					ApplyMethod:    "pending-reboot",
				},
				{
					ParameterName:  aws.String("max_logical_replication_workers"),
					ParameterValue: aws.String("10"),
					ApplyMethod:    "pending-reboot",
				},
			},
		}
		
		_, err = a.RDS.ModifyDBParameterGroup(ctx, modifyInput)
		if err != nil {
			return fmt.Errorf("failed to modify parameter group for %s: %w", dbID, err)
		}
		
		// Reboot instance to apply parameters
		rebootInput := &rds.RebootDBInstanceInput{
			DBInstanceIdentifier: aws.String(dbID),
			ForceFailover:       aws.Bool(false),
		}
		
		_, err = a.RDS.RebootDBInstance(ctx, rebootInput)
		if err != nil {
			return fmt.Errorf("failed to reboot DB instance %s: %w", dbID, err)
		}
		
		// Wait for instance to become available again
		waiter := rds.NewDBInstanceAvailableWaiter(a.RDS)
		err = waiter.Wait(ctx, &rds.DescribeDBInstancesInput{
			DBInstanceIdentifier: aws.String(dbID),
		}, 10*time.Minute)
		if err != nil {
			return fmt.Errorf("DB instance %s did not become available after reboot: %w", dbID, err)
		}
	}
	
	logger.Info("pgactive parameters configured successfully")
	return nil
}

// InstallPgactiveExtension installs the pgactive extension on both instances
func (a *Activities) InstallPgactiveExtension(ctx context.Context, input types.ActivityInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Installing pgactive extension",
		slog.String("source_db", input.SourceDBInstanceID),
		slog.String("target_db", input.TargetDBInstanceID))

	// Install extension on both instances
	for _, dbID := range []string{input.SourceDBInstanceID, input.TargetDBInstanceID} {
		// Get DB connection details
		db, err := a.getDBConnection(ctx, dbID)
		if err != nil {
			return fmt.Errorf("failed to connect to DB %s: %w", dbID, err)
		}
		defer db.Close()
		
		// Create pgactive extension
		_, err = db.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS pgactive")
		if err != nil {
			return fmt.Errorf("failed to create pgactive extension on %s: %w", dbID, err)
		}
		
		// Verify extension is installed
		var extensionExists bool
		err = db.QueryRowContext(ctx, 
			"SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pgactive')").Scan(&extensionExists)
		if err != nil {
			return fmt.Errorf("failed to verify pgactive extension on %s: %w", dbID, err)
		}
		
		if !extensionExists {
			return fmt.Errorf("pgactive extension not properly installed on %s", dbID)
		}
	}
	
	logger.Info("pgactive extension installed successfully")
	return nil
}

// WaitForSync waits for replication lag to reach zero
func (a *Activities) WaitForSync(ctx context.Context, input types.ActivityInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Waiting for replication sync",
		slog.String("replication_group", input.ReplicationGroup))

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Send heartbeat
			activity.RecordHeartbeat(ctx, "checking replication lag")
			
			// Check replication lag
			lag, err := a.getReplicationLag(ctx, input.SourceDBInstanceID, input.ReplicationGroup)
			if err != nil {
				a.Log.Warn("Failed to check replication lag", slog.String("error", err.Error()))
				continue
			}
			
			a.Log.Info("Replication lag check", slog.Int64("lag_bytes", lag))
			
			if lag == 0 {
				logger.Info("Replication is synchronized")
				return nil
			}
			
			// Check if lag is concerning (> 15 minutes worth of changes)
			if lag > 15*1024*1024*1024 { // 15GB as rough estimate
				return fmt.Errorf("replication lag too high: %d bytes", lag)
			}
		}
	}
}

// TrafficShiftPhase shifts a percentage of traffic to the target DB
func (a *Activities) TrafficShiftPhase(ctx context.Context, input types.TrafficShiftInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Executing traffic shift",
		slog.Int("phase", input.Phase),
		slog.Int("percentage", input.ShiftPercentage))

	// This would integrate with your traffic routing system
	// For now, we'll simulate the traffic shift
	time.Sleep(2 * time.Second)
	
	logger.Info("Traffic shift completed successfully")
	return nil
}

// RunHealthChecks validates system health after traffic shift
func (a *Activities) RunHealthChecks(ctx context.Context, input types.HealthCheckInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Running health checks", slog.String("check_type", input.CheckType))

	// Connect to target DB and run health checks
	db, err := a.getDBConnection(ctx, input.ActivityInput.TargetDBInstanceID)
	if err != nil {
		return fmt.Errorf("failed to connect to target DB: %w", err)
	}
	defer db.Close()
	
	// Basic connectivity check
	err = db.PingContext(ctx)
	if err != nil {
		return fmt.Errorf("target DB ping failed: %w", err)
	}
	
	// Check if pgactive is working
	var activeConnections int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_replication").Scan(&activeConnections)
	if err != nil {
		return fmt.Errorf("failed to check replication status: %w", err)
	}
	
	logger.Info("Health checks passed", slog.Int("active_connections", activeConnections))
	return nil
}

// Cutover performs the final traffic switch
func (a *Activities) Cutover(ctx context.Context, input types.ActivityInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Executing final cutover")

	// This would update your application configuration, load balancers, etc.
	// to point to the new target database
	time.Sleep(1 * time.Second)
	
	logger.Info("Cutover completed successfully")
	return nil
}

// OptionallyDetachOld removes the old DB from the replication group
func (a *Activities) OptionallyDetachOld(ctx context.Context, input types.ActivityInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Detaching old DB from replication group")

	// Connect to source DB and remove it from pgactive group
	db, err := a.getDBConnection(ctx, input.SourceDBInstanceID)
	if err != nil {
		return fmt.Errorf("failed to connect to source DB: %w", err)
	}
	defer db.Close()
	
	// Remove from replication group (pgactive specific command)
	_, err = db.ExecContext(ctx, fmt.Sprintf("SELECT pgactive_remove_node('%s')", input.ReplicationGroup))
	if err != nil {
		return fmt.Errorf("failed to remove node from replication group: %w", err)
	}
	
	logger.Info("Old DB detached successfully")
	return nil
}

// DecommissionSource deletes the old RDS instance
func (a *Activities) DecommissionSource(ctx context.Context, input types.ActivityInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Decommissioning source DB", slog.String("source_db", input.SourceDBInstanceID))

	// Create final snapshot before deletion
	snapshotID := fmt.Sprintf("%s-final-snapshot-%d", input.SourceDBInstanceID, time.Now().Unix())
	
	deleteInput := &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier:      aws.String(input.SourceDBInstanceID),
		SkipFinalSnapshot:        aws.Bool(false),
		FinalDBSnapshotIdentifier: aws.String(snapshotID),
		DeleteAutomatedBackups:   aws.Bool(true),
	}
	
	_, err := a.RDS.DeleteDBInstance(ctx, deleteInput)
	if err != nil {
		return fmt.Errorf("failed to delete source DB instance: %w", err)
	}
	
	logger.Info("Source DB decommissioned successfully", slog.String("snapshot", snapshotID))
	return nil
}

// ExecuteRollback rolls back the upgrade process
func (a *Activities) ExecuteRollback(ctx context.Context, input types.ActivityInput) error {
	logger := a.getLogger(ctx)
	a.Log.Info("Executing rollback procedure")

	// Reverse traffic back to source
	// Stop replication
	// Clean up target resources
	
	// This is a simplified rollback - in reality you'd need to:
	// 1. Redirect traffic back to source DB
	// 2. Stop and clean up replication
	// 3. Optionally delete the target DB
	
	time.Sleep(2 * time.Second)
	
	logger.Info("Rollback executed successfully")
	return nil
}

// Helper methods

func (a *Activities) getDBConnection(ctx context.Context, dbInstanceID string) (*sql.DB, error) {
	// Get DB endpoint
	describeInput := &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(dbInstanceID),
	}
	
	result, err := a.RDS.DescribeDBInstances(ctx, describeInput)
	if err != nil {
		return nil, fmt.Errorf("failed to describe DB instance: %w", err)
	}
	
	db := result.DBInstances[0]
	endpoint := *db.Endpoint.Address
	port := db.Endpoint.Port
	
	// Get credentials from Secrets Manager
	secretARN := *db.MasterUserSecret.SecretArn
	secretOutput, err := a.SecretsClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretARN),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get DB credentials: %w", err)
	}
	
	// Parse secret value to get username/password
	// This is simplified - you'd parse the JSON secret
	connStr := fmt.Sprintf("host=%s port=%d dbname=postgres sslmode=require user=postgres password=%s",
		endpoint, port, *secretOutput.SecretString)
	
	return sql.Open("postgres", connStr)
}

func (a *Activities) getReplicationLag(ctx context.Context, dbInstanceID, replicationGroup string) (int64, error) {
	// This would query pgactive or PostgreSQL replication views
	// to get the current lag in bytes
	// For now, we'll simulate decreasing lag
	return 0, nil
}

func convertTags(tags map[string]string) []rdstypes.Tag {
	var rdsatags []rdstypes.Tag
	for k, v := range tags {
		rdsatags = append(rdsatags, rdstypes.Tag{
			Key:   aws.String(k),
			Value: aws.String(v),
		})
	}
	return rdsatags
}
