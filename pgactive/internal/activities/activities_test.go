package activities

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	upgradetypes "github.com/leowmjw/go-temporal-pg/pgactive/internal/types"
)

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

// availableDBOutput returns a DescribeDBInstancesOutput for a single available postgres instance.
func availableDBOutput(id, version string) *rds.DescribeDBInstancesOutput {
	return &rds.DescribeDBInstancesOutput{
		DBInstances: []types.DBInstance{
			{
				DBInstanceIdentifier: aws.String(id),
				Engine:               aws.String("postgres"),
				EngineVersion:        aws.String(version),
				DBInstanceStatus:     aws.String("available"),
			},
		},
	}
}

func TestValidateInput(t *testing.T) {
	tests := []struct {
		name        string
		input       upgradetypes.UpgradeInput
		rds         RDSClientFuncs
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid input",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
				ShiftPercentages:   []int{25, 25, 50},
				Subnets:            []string{"subnet-1", "subnet-2"},
			},
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return availableDBOutput("source-db", "14.9"), nil
				},
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
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return &rds.DescribeDBInstancesOutput{}, errors.New("DB instance not found")
				},
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
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return &rds.DescribeDBInstancesOutput{
						DBInstances: []types.DBInstance{
							{
								DBInstanceIdentifier: aws.String("mysql-db"),
								Engine:               aws.String("mysql"),
								EngineVersion:        aws.String("8.0"),
								DBInstanceStatus:     aws.String("available"),
							},
						},
					}, nil
				},
			},
			expectError: true,
			errorMsg:    "source DB must be PostgreSQL",
		},
		{
			name: "invalid shift percentages",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
				ShiftPercentages:   []int{25, 25, 25}, // sum is 75, not 100
			},
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return availableDBOutput("source-db", "14.9"), nil
				},
			},
			expectError: true,
			errorMsg:    "shift percentages must sum to 100",
		},
		{
			name: "target version not newer",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "14.8", // older than 14.9
				ShiftPercentages:   []int{100},
			},
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return availableDBOutput("source-db", "14.9"), nil
				},
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
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return &rds.DescribeDBInstancesOutput{
						DBInstances: []types.DBInstance{
							{
								DBInstanceIdentifier: aws.String("source-db"),
								Engine:               aws.String("postgres"),
								EngineVersion:        aws.String("14.9"),
								DBInstanceStatus:     aws.String("backing-up"),
							},
						},
					}, nil
				},
			},
			expectError: true,
			errorMsg:    "source DB must be in 'available' status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewActivities(tt.rds, nil, newLogger())
			err := a.ValidateInput(context.Background(), tt.input)
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
		name      string
		input     upgradetypes.UpgradeInput
		rds       RDSClientFuncs
		expectErr bool
	}{
		{
			name: "successful provisioning",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
				InstanceClass:      "db.r6g.large",
				SecurityGroupIDs:   []string{"sg-123"},
			},
			rds: func() RDSClientFuncs {
				// First call: describe source DB; subsequent calls: waiter polling.
				var callCount int32
				return RDSClientFuncs{
					DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
						if atomic.AddInt32(&callCount, 1) == 1 {
							return &rds.DescribeDBInstancesOutput{
								DBInstances: []types.DBInstance{
									{
										DBInstanceIdentifier: aws.String("source-db"),
										DBInstanceClass:      aws.String("db.r6g.medium"),
										Engine:               aws.String("postgres"),
										EngineVersion:        aws.String("14.9"),
										AllocatedStorage:     aws.Int32(100),
										StorageType:          aws.String("gp2"),
										StorageEncrypted:     aws.Bool(true),
										MasterUsername:       aws.String("postgres"),
										DBSubnetGroup: &types.DBSubnetGroup{
											DBSubnetGroupName: aws.String("default-subnet-group"),
										},
									},
								},
							}, nil
						}
						return &rds.DescribeDBInstancesOutput{
							DBInstances: []types.DBInstance{
								{
									DBInstanceIdentifier: aws.String("source-db-upgrade-123"),
									DBInstanceStatus:     aws.String("available"),
								},
							},
						}, nil
					},
					CreateDBInstance: func(_ context.Context, _ *rds.CreateDBInstanceInput, _ ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error) {
						return &rds.CreateDBInstanceOutput{}, nil
					},
				}
			}(),
			expectErr: false,
		},
		{
			name: "source DB describe fails",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
			},
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return nil, errors.New("describe failed")
				},
			},
			expectErr: true,
		},
		{
			name: "create DB instance fails",
			input: upgradetypes.UpgradeInput{
				SourceDBInstanceID: "source-db",
				TargetVersion:      "15.4",
			},
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return &rds.DescribeDBInstancesOutput{
						DBInstances: []types.DBInstance{
							{
								DBInstanceIdentifier: aws.String("source-db"),
								DBInstanceClass:      aws.String("db.r6g.medium"),
								Engine:               aws.String("postgres"),
								AllocatedStorage:     aws.Int32(100),
								StorageType:          aws.String("gp2"),
								MasterUsername:       aws.String("postgres"),
								DBSubnetGroup: &types.DBSubnetGroup{
									DBSubnetGroupName: aws.String("default-subnet-group"),
								},
							},
						},
					}, nil
				},
				CreateDBInstance: func(_ context.Context, _ *rds.CreateDBInstanceInput, _ ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error) {
					return nil, errors.New("create failed")
				},
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewActivities(tt.rds, nil, newLogger())
			result, err := a.ProvisionTargetDB(context.Background(), tt.input)
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
		name      string
		input     upgradetypes.ActivityInput
		rds       RDSClientFuncs
		expectErr bool
	}{
		{
			name: "successful configuration",
			input: upgradetypes.ActivityInput{
				SourceDBInstanceID: "source-db",
				TargetDBInstanceID: "target-db",
			},
			rds: func() RDSClientFuncs {
				paramGroup := func(name string) *rds.DescribeDBInstancesOutput {
					return &rds.DescribeDBInstancesOutput{
						DBInstances: []types.DBInstance{
							{
								DBParameterGroups: []types.DBParameterGroupStatus{
									{DBParameterGroupName: aws.String(name)},
								},
								DBInstanceStatus: aws.String("available"),
							},
						},
					}
				}
				return RDSClientFuncs{
					DescribeDBInstances: func(_ context.Context, p *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
						if p.DBInstanceIdentifier != nil && *p.DBInstanceIdentifier == "source-db" {
							return paramGroup("source-param-group"), nil
						}
						if p.DBInstanceIdentifier != nil && *p.DBInstanceIdentifier == "target-db" {
							return paramGroup("target-param-group"), nil
						}
						// waiter poll: return available, no param group needed
						return &rds.DescribeDBInstancesOutput{
							DBInstances: []types.DBInstance{
								{DBInstanceStatus: aws.String("available")},
							},
						}, nil
					},
					ModifyDBParameterGroup: func(_ context.Context, _ *rds.ModifyDBParameterGroupInput, _ ...func(*rds.Options)) (*rds.ModifyDBParameterGroupOutput, error) {
						return &rds.ModifyDBParameterGroupOutput{}, nil
					},
					RebootDBInstance: func(_ context.Context, _ *rds.RebootDBInstanceInput, _ ...func(*rds.Options)) (*rds.RebootDBInstanceOutput, error) {
						return &rds.RebootDBInstanceOutput{}, nil
					},
				}
			}(),
			expectErr: false,
		},
		{
			name: "describe DB fails",
			input: upgradetypes.ActivityInput{
				SourceDBInstanceID: "source-db",
				TargetDBInstanceID: "target-db",
			},
			rds: RDSClientFuncs{
				DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
					return nil, errors.New("describe failed")
				},
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewActivities(tt.rds, nil, newLogger())
			err := a.ConfigurePgactiveParams(context.Background(), tt.input)
			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTrafficShiftPhase(t *testing.T) {
	a := NewActivities(RDSClientFuncs{}, nil, newLogger())

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
			err := a.TrafficShiftPhase(context.Background(), tt.input)
			require.NoError(t, err)
		})
	}
}

func TestDecommissionSource(t *testing.T) {
	tests := []struct {
		name      string
		input     upgradetypes.ActivityInput
		rds       RDSClientFuncs
		expectErr bool
	}{
		{
			name: "successful decommission",
			input: upgradetypes.ActivityInput{
				SourceDBInstanceID: "source-db",
			},
			rds: RDSClientFuncs{
				DeleteDBInstance: func(_ context.Context, _ *rds.DeleteDBInstanceInput, _ ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error) {
					return &rds.DeleteDBInstanceOutput{}, nil
				},
			},
			expectErr: false,
		},
		{
			name: "delete fails",
			input: upgradetypes.ActivityInput{
				SourceDBInstanceID: "source-db",
			},
			rds: RDSClientFuncs{
				DeleteDBInstance: func(_ context.Context, _ *rds.DeleteDBInstanceInput, _ ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error) {
					return nil, errors.New("delete failed")
				},
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewActivities(tt.rds, nil, newLogger())
			err := a.DecommissionSource(context.Background(), tt.input)
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
		a := NewActivities(RDSClientFuncs{
			DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
				return availableDBOutput("source-db", "14.9"), nil
			},
		}, nil, newLogger())

		err := a.ValidateInput(context.Background(), upgradetypes.UpgradeInput{
			SourceDBInstanceID: "source-db",
			TargetVersion:      "15.4",
			ShiftPercentages:   []int{},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one shift percentage must be specified")
	})

	t.Run("negative shift percentage", func(t *testing.T) {
		a := NewActivities(RDSClientFuncs{
			DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
				return availableDBOutput("source-db", "14.9"), nil
			},
		}, nil, newLogger())

		err := a.ValidateInput(context.Background(), upgradetypes.UpgradeInput{
			SourceDBInstanceID: "source-db",
			TargetVersion:      "15.4",
			ShiftPercentages:   []int{-10, 110},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shift percentage must be between 0 and 100")
	})

	t.Run("DB instance not found", func(t *testing.T) {
		a := NewActivities(RDSClientFuncs{
			DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
				return &rds.DescribeDBInstancesOutput{DBInstances: []types.DBInstance{}}, nil
			},
		}, nil, newLogger())

		err := a.ValidateInput(context.Background(), upgradetypes.UpgradeInput{
			SourceDBInstanceID: "source-db",
			TargetVersion:      "15.4",
			ShiftPercentages:   []int{100},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "source DB instance source-db not found")
	})
}
