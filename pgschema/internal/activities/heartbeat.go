package activities

import (
	"context"
	"log/slog"

	"go.temporal.io/sdk/activity"
)

// safeHeartbeat calls activity.RecordHeartbeat but tolerates ctx not being a
// Temporal activity context (e.g. unit tests calling activity methods
// directly). activity.RecordHeartbeat panics in that case; we recover but log
// the panic instead of swallowing it silently, so a genuinely corrupted
// activity context in production leaves a diagnostic trail rather than just
// going quiet until Temporal's HeartbeatTimeout kills the activity.
func safeHeartbeat(ctx context.Context, details interface{}) {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().WarnContext(ctx, "safeHeartbeat: RecordHeartbeat panicked, dropping heartbeat",
				slog.Any("recovered", r))
		}
	}()
	activity.RecordHeartbeat(ctx, details)
}
