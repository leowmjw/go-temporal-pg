package workflow

import (
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

const (
	PreviewCloneTaskQueue = "pgschema-preview"

	SignalCleanup = "cleanup"

	QueryPreviewEndpoint = "preview-endpoint"
)

// PreviewCloneWorkflow creates a PII-free, ephemeral copy of the production
// database for agent/QA use, and destroys it after TTL or on cleanup signal.
//
//   CloneDatabase
//     → ApplyAnonymization      (PII removal; MUST succeed before expose)
//       → RunMigrationPreview   (optional: test the pending migration)
//         → ExposePreviewEndpoint
//           → wait(cleanup signal | TTL timer)
//             → DropPreviewDatabase
//
// Temporal features used:
//   - workflow.NewTimer for TTL-based auto-cleanup
//   - Selector: cleanup signal races the TTL timer
//   - Query handler: preview endpoint visible at any time
//   - RegisterUpdateHandlerWithOptions: operator can extend TTL mid-flight
func PreviewCloneWorkflow(ctx workflow.Context, input types.PreviewCloneInput) (*types.PreviewEndpoint, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("PreviewCloneWorkflow starting",
		slog.String("preview_id", input.PreviewID))

	if input.TTL <= 0 {
		input.TTL = 4 * time.Hour // safe default
	}

	// ── Endpoint state ────────────────────────────────────────────────────────
	var endpoint *types.PreviewEndpoint

	// ── Query handler ─────────────────────────────────────────────────────────
	if err := workflow.SetQueryHandler(ctx, QueryPreviewEndpoint,
		func() (*types.PreviewEndpoint, error) { return endpoint, nil },
	); err != nil {
		return nil, fmt.Errorf("register query handler: %w", err)
	}

	// extendTTLCh carries extension durations from the "extend-ttl" Update
	// handler into the Step-5 wait loop below, which is the only place that
	// actually owns the TTL deadline that controls DropPreviewDatabase.
	extendTTLCh := workflow.NewBufferedChannel(ctx, 16)

	// ── Update handler: operator extends TTL ─────────────────────────────────
	if err := workflow.SetUpdateHandlerWithOptions(ctx,
		"extend-ttl",
		func(uCtx workflow.Context, extra time.Duration) (string, error) {
			input.TTL += extra
			if endpoint != nil {
				endpoint.ExpiresAt = endpoint.ExpiresAt.Add(extra)
			}
			// Deliver the extension to the Step-5 wait loop so the actual
			// cleanup deadline moves, not just the query-visible ExpiresAt.
			extendTTLCh.Send(uCtx, extra)
			return fmt.Sprintf("TTL extended by %s", extra), nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(_ workflow.Context, extra time.Duration) error {
				if extra <= 0 || extra > 24*time.Hour {
					return fmt.Errorf("extension must be 1s-24h, got %s", extra)
				}
				return nil
			},
		},
	); err != nil {
		return nil, fmt.Errorf("register update handler: %w", err)
	}

	actOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    3,
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, actOpts)

	// ── Step 1: Clone ─────────────────────────────────────────────────────────
	var targetDSN string
	if err := workflow.ExecuteActivity(ctx,
		(*activities.PreviewDBActivities).CloneDatabase, input,
	).Get(ctx, &targetDSN); err != nil {
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "clone"}, err, "warning", "preview-clone")
		return nil, &types.PreviewError{PreviewID: input.PreviewID, Wrapped: err}
	}

	// ── Step 2: Anonymize (MUST succeed before expose) ────────────────────────
	anonInput := types.AnonymizationInput{
		TargetDSN: targetDSN,
		Rules:     input.AnonymizationRules,
	}
	if err := workflow.ExecuteActivity(ctx,
		(*activities.PreviewDBActivities).ApplyAnonymization, anonInput,
	).Get(ctx, nil); err != nil {
		// Anonymization failure → drop the clone immediately (PII risk).
		logger.Error("anonymization failed; dropping clone",
			slog.String("preview_id", input.PreviewID),
			slog.String("error", err.Error()))
		_ = workflow.ExecuteActivity(ctx,
			(*activities.PreviewDBActivities).DropPreviewDatabase, targetDSN,
		).Get(ctx, nil)
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "anonymize"}, err, "critical", "anon-failed")
		return nil, &types.PreviewError{PreviewID: input.PreviewID, Wrapped: err}
	}

	// ── Step 3: Optional migration preview ────────────────────────────────────
	if input.MigrationJSON != "" {
		if err := workflow.ExecuteActivity(ctx,
			(*activities.PreviewDBActivities).RunMigrationPreview, targetDSN, input.MigrationJSON,
		).Get(ctx, nil); err != nil {
			// Non-fatal: log and continue — the clone is still useful.
			logger.Warn("migration preview failed",
				slog.String("preview_id", input.PreviewID),
				slog.String("error", err.Error()))
		}
	}

	// ── Step 4: Expose endpoint ───────────────────────────────────────────────
	if err := workflow.ExecuteActivity(ctx,
		(*activities.PreviewDBActivities).ExposePreviewEndpoint, targetDSN, input.TTL,
	).Get(ctx, &endpoint); err != nil {
		return nil, fmt.Errorf("expose endpoint: %w", err)
	}
	logger.Info("preview endpoint ready",
		slog.String("preview_id", input.PreviewID),
		slog.Time("expires_at", endpoint.ExpiresAt))

	// ── Step 5: Wait for cleanup signal or TTL ────────────────────────────────
	cleanupCh := workflow.GetSignalChannel(ctx, SignalCleanup)
	ttlDeadline := workflow.Now(ctx).Add(input.TTL)

	var cleanupRequested, ttlExpired bool
	for !cleanupRequested && !ttlExpired {
		remaining := ttlDeadline.Sub(workflow.Now(ctx))
		if remaining < 0 {
			remaining = 0
		}
		ttlTimer := workflow.NewTimer(ctx, remaining)
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(cleanupCh, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)
			cleanupRequested = true
			logger.Info("cleanup signal received", slog.String("preview_id", input.PreviewID))
		})
		sel.AddFuture(ttlTimer, func(_ workflow.Future) {
			ttlExpired = true
			logger.Info("TTL expired, cleaning up", slog.String("preview_id", input.PreviewID))
		})
		sel.AddReceive(extendTTLCh, func(c workflow.ReceiveChannel, _ bool) {
			var extra time.Duration
			c.Receive(ctx, &extra)
			ttlDeadline = ttlDeadline.Add(extra)
			logger.Info("preview TTL deadline extended",
				slog.String("preview_id", input.PreviewID),
				slog.Time("new_deadline", ttlDeadline))
		})
		sel.Select(ctx)
	}

	// ── Step 6: Drop ──────────────────────────────────────────────────────────
	dropOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 5,
			InitialInterval: 10 * time.Second,
		},
	}
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, dropOpts),
		(*activities.PreviewDBActivities).DropPreviewDatabase, targetDSN,
	).Get(ctx, nil); err != nil {
		// Drop failure pages a human — the ephemeral DB is leaking.
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "drop"}, err, "critical", "preview-drop-failed")
		return endpoint, fmt.Errorf("drop preview DB failed: %w", err)
	}

	logger.Info("PreviewCloneWorkflow completed",
		slog.String("preview_id", input.PreviewID))
	return endpoint, nil
}
