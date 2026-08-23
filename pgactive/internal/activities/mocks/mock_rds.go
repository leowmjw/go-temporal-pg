// Package mocks provides stub helpers for RDSClientFuncs used in tests.
package mocks

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/leowmjw/go-temporal-pg/pgactive/internal/activities"
)

// NewRDSClientFuncsStub returns an RDSClientFuncs with all operations stubbed to
// return the provided defaults. Individual fields may be overridden by the caller
// before passing the struct to NewActivities.
func NewRDSClientFuncsStub(
	describeOut *rds.DescribeDBInstancesOutput, describeErr error,
	createOut *rds.CreateDBInstanceOutput, createErr error,
	modifyOut *rds.ModifyDBParameterGroupOutput, modifyErr error,
	rebootOut *rds.RebootDBInstanceOutput, rebootErr error,
	deleteOut *rds.DeleteDBInstanceOutput, deleteErr error,
) activities.RDSClientFuncs {
	return activities.RDSClientFuncs{
		DescribeDBInstances: func(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
			return describeOut, describeErr
		},
		CreateDBInstance: func(_ context.Context, _ *rds.CreateDBInstanceInput, _ ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error) {
			return createOut, createErr
		},
		ModifyDBParameterGroup: func(_ context.Context, _ *rds.ModifyDBParameterGroupInput, _ ...func(*rds.Options)) (*rds.ModifyDBParameterGroupOutput, error) {
			return modifyOut, modifyErr
		},
		RebootDBInstance: func(_ context.Context, _ *rds.RebootDBInstanceInput, _ ...func(*rds.Options)) (*rds.RebootDBInstanceOutput, error) {
			return rebootOut, rebootErr
		},
		DeleteDBInstance: func(_ context.Context, _ *rds.DeleteDBInstanceInput, _ ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error) {
			return deleteOut, deleteErr
		},
	}
}
