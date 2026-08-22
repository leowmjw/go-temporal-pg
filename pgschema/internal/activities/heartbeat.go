package activities

import (
	"context"

	"go.temporal.io/sdk/activity"
)

// safeHeartbeat calls activity.RecordHeartbeat but silently drops the call
// when ctx is not a Temporal activity context (e.g. unit tests).
func safeHeartbeat(ctx context.Context, details interface{}) {
	defer func() { recover() }()
	activity.RecordHeartbeat(ctx, details)
}
