package activities

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"company.com/infra/pgactive-upgrade/internal/activities/mocks"
	upgradetypes "company.com/infra/pgactive-upgrade/internal/types"
)

func TestValidateInput(t *testing.T) {
	tests := []struct {
		name        string
		input       upgradetypes.UpgradeInput
		setupMocks  func(*mocks.MockRDSClient)
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid input",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
				ShiftPercentages:   []int{25, 25, 50},
				Subnets:           []string{"subnet-1", "subnet-2"},
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), &rds.DescribeDBInstancesInput{
					DBInstanceIdentifier: aws.String("source-db"),
				}, gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBInstanceIdentifier: aws.String("source-db"),
							Engine:              aws.String("postgres"),
							EngineVersion:       aws.String("14.9"),
							DBInstanceStatus:    aws.String("available"),
						},
					},
				}, nil)
			},
			expectError: false,
		},
		{
			name: "source DB not found",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "nonexistent-db",
				TargetVersion:      "15.4",
				ShiftPercentages:   []int{100},
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(
					&rds.DescribeDBInstancesOutput{}, errors.New("DB instance not found"))
			},
			expectError: true,
			errorMsg:    "failed to describe source DB instance",
		},
		{
			name: "non-PostgreSQL engine",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "mysql-db",
				TargetVersion:      "15.4",
				ShiftPercentages:   []int{100},
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBInstanceIdentifier: aws.String("mysql-db"),
							Engine:              aws.String("mysql"),
							EngineVersion:       aws.String("8.0"),
							DBInstanceStatus:    aws.String("available"),
						},
					},
				}, nil)
			},
			expectError: true,
			errorMsg:    "source DB must be PostgreSQL",
		},
		{
			name: "invalid shift percentages",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
				ShiftPercentages:   []int{25, 25, 25}, // Sum is 75, not 100
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBInstanceIdentifier: aws.String("source-db"),
							Engine:              aws.String("postgres"),
							EngineVersion:       aws.String("14.9"),
							DBInstanceStatus:    aws.String("available"),
						},
					},
				}, nil)
			},
			expectError: true,
			errorMsg:    "shift percentages must sum to 100",
		},
		{
			name: "target version not newer",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "14.8", // Older than current 14.9
				ShiftPercentages:   []int{100},
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBInstanceIdentifier: aws.String("source-db"),
							Engine:              aws.String("postgres"),
							EngineVersion:       aws.String("14.9"),
							DBInstanceStatus:    aws.String("available"),
						},
					},
				}, nil)
			},
			expectError: true,
			errorMsg:    "target version 14.8 must be newer than current version 14.9",
		},
		{
			name: "DB not available",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
				ShiftPercentages:   []int{100},
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBInstanceIdentifier: aws.String("source-db"),
							Engine:              aws.String("postgres"),
							EngineVersion:       aws.String("14.9"),
							DBInstanceStatus:    aws.String("backing-up"),
						},
					},
				}, nil)
			},
			expectError: true,
			errorMsg:    "source DB must be in 'available' status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockRDS := mocks.NewMockRDSClient(ctrl)
			logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			activities := NewActivities(mockRDS, nil, logger)

			tt.setupMocks(mockRDS)

			err := activities.ValidateInput(context.Background(), tt.input)

			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestProvisionTargetDB(t *testing.T) {
	tests := []struct {
		name       string
		input      upgradetypes.UpgradeInput
		setupMocks func(*mocks.MockRDSClient)
		expectErr  bool
	}{
		{
			name: "successful provisioning",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
				InstanceClass:      "db.r6g.large",
				SecurityGroupIDs:   []string{"sg-123"},
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				// Mock DescribeDBInstances for source DB
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), &rds.DescribeDBInstancesInput{
					DBInstanceIdentifier: aws.String("source-db"),
				}, gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBInstanceIdentifier: aws.String("source-db"),
							DBInstanceClass:      aws.String("db.r6g.medium"),
							Engine:              aws.String("postgres"),
							EngineVersion:       aws.String("14.9"),
							AllocatedStorage:    aws.Int32(100),
							StorageType:         aws.String("gp2"),
							StorageEncrypted:    aws.Bool(true),
							MasterUsername:      aws.String("postgres"),
							DBSubnetGroup: &types.DBSubnetGroup{
								DBSubnetGroupName: aws.String("default-subnet-group"),
							},
						},
					},
				}, nil)

				// Mock CreateDBInstance
				mockRDS.EXPECT().CreateDBInstance(gomock.Any(), gomock.Any(), gomock.Any()).Return(
					&rds.CreateDBInstanceOutput{}, nil)

				// Mock DescribeDBInstances for waiting (waiter simulation)
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBInstanceIdentifier: aws.String("source-db-upgrade-123"),
							DBInstanceStatus:    aws.String("available"),
						},
					},
				}, nil).AnyTimes()
			},
			expectErr: false,
		},
		{
			name: "source DB describe fails",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(
					nil, errors.New("describe failed"))
			},
			expectErr: true,
		},
		{
			name: "create DB instance fails",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
			mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
			DBInstances: []types.DBInstance{
			{
			DBInstanceIdentifier: aws.String("source-db"),
			DBInstanceClass:      aws.String("db.r6g.medium"),
			Engine:              aws.String("postgres"),
			AllocatedStorage:    aws.Int32(100),
			StorageType:         aws.String("gp2"),
			MasterUsername:      aws.String("postgres"),
			DBSubnetGroup: &types.DBSubnetGroup{
			DBSubnetGroupName: aws.String("default-subnet-group"),
			},
			},
			},
			}, nil)

			mockRDS.EXPECT().CreateDBInstance(gomock.Any(), gomock.Any(), gomock.Any()).Return(
			nil, errors.New("create failed"))
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockRDS := mocks.NewMockRDSClient(ctrl)
			logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			activities := NewActivities(mockRDS, nil, logger)

			tt.setupMocks(mockRDS)

			result, err := activities.ProvisionTargetDB(context.Background(), tt.input)

			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, result)
				assert.Contains(t, result, "source-db-upgrade-")
			}
		})
	}
}

func TestConfigurePgactiveParams(t *testing.T) {
	tests := []struct {
		name       string
		input      upgradetypes.ActivityInput
		setupMocks func(*mocks.MockRDSClient)
		expectErr  bool
	}{
		{
			name: "successful configuration",
			input: upgradetypes.ActivityInput{
				SourceDBInstanceID: "source-db",
				TargetDBInstanceID: "target-db",
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				// Mock for source DB
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), &rds.DescribeDBInstancesInput{
					DBInstanceIdentifier: aws.String("source-db"),
				}, gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBParameterGroups: []types.DBParameterGroupStatus{
								{DBParameterGroupName: aws.String("source-param-group")},
							},
						},
					},
				}, nil)

				// Mock for target DB
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), &rds.DescribeDBInstancesInput{
					DBInstanceIdentifier: aws.String("target-db"),
				}, gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{
							DBParameterGroups: []types.DBParameterGroupStatus{
								{DBParameterGroupName: aws.String("target-param-group")},
							},
						},
					},
				}, nil)

				// Mock parameter group modifications
				mockRDS.EXPECT().ModifyDBParameterGroup(gomock.Any(), gomock.Any(), gomock.Any()).Return(
					&rds.ModifyDBParameterGroupOutput{}, nil).Times(2)

				// Mock reboots
				mockRDS.EXPECT().RebootDBInstance(gomock.Any(), gomock.Any(), gomock.Any()).Return(
					&rds.RebootDBInstanceOutput{}, nil).Times(2)

				// Mock waits for availability after reboot
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
					DBInstances: []types.DBInstance{
						{DBInstanceStatus: aws.String("available")},
					},
				}, nil).AnyTimes()
			},
			expectErr: false,
		},
		{
			name: "describe DB fails",
			input: upgradetypes.ActivityInput{
				SourceDBInstanceID: "source-db",
				TargetDBInstanceID: "target-db",
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(
					nil, errors.New("describe failed"))
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockRDS := mocks.NewMockRDSClient(ctrl)
			logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			activities := NewActivities(mockRDS, nil, logger)

			tt.setupMocks(mockRDS)

			err := activities.ConfigurePgactiveParams(context.Background(), tt.input)

			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTrafficShiftPhase(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	activities := NewActivities(nil, nil, logger)

	tests := []struct {
		name  string
		input upgradetypes.TrafficShiftInput
	}{
		{
			name: "25% traffic shift",
			input: upgradetypes.TrafficShiftInput{
				ActivityInput: upgradetypes.ActivityInput{
					SourceDBInstanceID: "source-db",
					TargetDBInstanceID: "target-db",
				},
				ShiftPercentage: 25,
				Phase:           1,
			},
		},
		{
			name: "100% traffic shift",
			input: upgradetypes.TrafficShiftInput{
				ActivityInput: upgradetypes.ActivityInput{
					SourceDBInstanceID: "source-db",
					TargetDBInstanceID: "target-db",
				},
				ShiftPercentage: 100,
				Phase:           3,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := activities.TrafficShiftPhase(context.Background(), tt.input)
			require.NoError(t, err)
		})
	}
}

func TestDecommissionSource(t *testing.T) {
	tests := []struct {
		name       string
		input      upgradetypes.ActivityInput
		setupMocks func(*mocks.MockRDSClient)
		expectErr  bool
	}{
		{
			name: "successful decommission",
			input: upgradetypes.ActivityInput{
				SourceDBInstanceID: "source-db",
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DeleteDBInstance(gomock.Any(), gomock.Any(), gomock.Any()).Return(
					&rds.DeleteDBInstanceOutput{}, nil)
			},
			expectErr: false,
		},
		{
			name: "delete fails",
			input: upgradetypes.ActivityInput{
				SourceDBInstanceID: "source-db",
			},
			setupMocks: func(mockRDS *mocks.MockRDSClient) {
				mockRDS.EXPECT().DeleteDBInstance(gomock.Any(), gomock.Any(), gomock.Any()).Return(
					nil, errors.New("delete failed"))
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockRDS := mocks.NewMockRDSClient(ctrl)
			logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			activities := NewActivities(mockRDS, nil, logger)

			tt.setupMocks(mockRDS)

			err := activities.DecommissionSource(context.Background(), tt.input)

			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// Edge case tests for error scenarios
func TestValidateInput_EdgeCases(t *testing.T) {
	t.Run("empty shift percentages", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockRDS := mocks.NewMockRDSClient(ctrl)
		logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		activities := NewActivities(mockRDS, nil, logger)

		input := upgradetypes.UpgradeInput{
			SourceDBInstanceID: "source-db",
			TargetVersion:      "15.4",
			ShiftPercentages:   []int{}, // Empty array
		}

		// Mock DescribeDBInstances call that happens before shift percentage validation
		mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
			DBInstances: []types.DBInstance{
				{
					DBInstanceIdentifier: aws.String("source-db"),
					Engine:              aws.String("postgres"),
					EngineVersion:       aws.String("14.9"),
					DBInstanceStatus:    aws.String("available"),
				},
			},
		}, nil)

		err := activities.ValidateInput(context.Background(), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one shift percentage must be specified")
	})

	t.Run("negative shift percentage", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockRDS := mocks.NewMockRDSClient(ctrl)
		logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		activities := NewActivities(mockRDS, nil, logger)

		input := upgradetypes.UpgradeInput{
			SourceDBInstanceID: "source-db",
			TargetVersion:      "15.4",
			ShiftPercentages:   []int{-10, 110}, // Invalid values
		}

		// Mock DescribeDBInstances call that happens before shift percentage validation
		mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
			DBInstances: []types.DBInstance{
				{
					DBInstanceIdentifier: aws.String("source-db"),
					Engine:              aws.String("postgres"),
					EngineVersion:       aws.String("14.9"),
					DBInstanceStatus:    aws.String("available"),
				},
			},
		}, nil)

		err := activities.ValidateInput(context.Background(), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shift percentage must be between 0 and 100")
	})

	t.Run("DB instance not found", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockRDS := mocks.NewMockRDSClient(ctrl)
		logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
		activities := NewActivities(mockRDS, nil, logger)

		input := upgradetypes.UpgradeInput{
			SourceDBInstanceID: "source-db",
			TargetVersion:      "15.4",
			ShiftPercentages:   []int{100},
		}

		mockRDS.EXPECT().DescribeDBInstances(gomock.Any(), gomock.Any()).Return(&rds.DescribeDBInstancesOutput{
			DBInstances: []types.DBInstance{}, // Empty result
		}, nil)

		err := activities.ValidateInput(context.Background(), input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "source DB instance source-db not found")
	})
}
