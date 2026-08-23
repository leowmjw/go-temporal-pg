package workflow

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

const (
	CDCStreamTaskQueue = "pgschema-cdc"

	SignalStopStream = "stop"

	QueryStreamLag = "lag"

	// ContinueAsNew after this many heartbeat cycles to keep history small.
	defaultMaxIterations = 1000
)

// CDCStreamWorkflow orchestrates a continuous pgstream CDC pipeline:
//
//   InitPgstream
//     → RunStream (long-running heartbeat activity)
//       → PollLag (concurrent heartbeat activity reporting lag)
//         → (stop signal | schema drift alert | ContinueAsNew)
//
// Temporal features used:
//   - Long-running activity with HeartbeatTimeout
//   - workflow.Go for concurrent lag polling
//   - Drainable signal queue pattern with ReceiveAsync
//   - ContinueAsNew to prevent history overflow after MaxIterations
//   - Query handler for live lag visibility
//   - workflow.GetVersion for safe code upgrades
func CDCStreamWorkflow(ctx workflow.Context, cfg types.StreamConfig) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("CDCStreamWorkflow starting",
		slog.String("stream_id", cfg.StreamID))

	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = defaultMaxIterations
	}

	// ── Version gate ──────────────────────────────────────────────────────────
	_ = workflow.GetVersion(ctx, "cdc-v1", workflow.DefaultVersion, 1)

	// ── Lag state (only written by lag-polling goroutine) ─────────────────────
	lagState := &types.LagResponse{LastChecked: workflow.Now(ctx)}

	// ── Query handler ─────────────────────────────────────────────────────────
	if err := workflow.SetQueryHandler(ctx, QueryStreamLag,
		func() (*types.LagResponse, error) { return lagState, nil },
	); err != nil {
		return fmt.Errorf("register lag query: %w", err)
	}

	// ── Update handler — operator can change anonymization rules mid-stream ──
	if err := workflow.SetUpdateHandlerWithOptions(ctx,
		"update-anon-rules",
		func(_ workflow.Context, rules []types.AnonymizationRule) (string, error) {
			cfg.AnonymizationRules = rules
			return fmt.Sprintf("anonymization rules updated: %d rules", len(rules)), nil
		},
		workflow.UpdateHandlerOptions{
			Validator: func(_ workflow.Context, rules []types.AnonymizationRule) error {
				if len(rules) == 0 {
					return errors.New("rules list must not be empty")
				}
				return nil
			},
		},
	); err != nil {
		return fmt.Errorf("register update handler: %w", err)
	}

	// ── Signal channel ────────────────────────────────────────────────────────
	stopCh := workflow.GetSignalChannel(ctx, SignalStopStream)

	// ── Short-activity options ────────────────────────────────────────────────
	shortOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
			InitialInterval: 2 * time.Second,
		},
	}

	// ── Step 1: Init ──────────────────────────────────────────────────────────
	if err := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, shortOpts),
		(*activities.PgstreamActivities).InitPgstream, cfg,
	).Get(ctx, nil); err != nil {
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "init"}, err, "critical", "cdc-init")
		return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}

	// ── Step 2: Long-running stream activity ──────────────────────────────────
	streamOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 7 * 24 * time.Hour, // long-lived
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    0, // unlimited retries
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 1.5,
			MaximumInterval:    5 * time.Minute,
		},
	}
	streamFuture := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, streamOpts),
		(*activities.PgstreamActivities).RunStream, cfg,
	)

	// ── Step 3: Concurrent lag polling ────────────────────────────────────────
	// workflow.Go runs in the same coroutine scheduler — safe for Temporal.
	lagDone := workflow.NewChannel(ctx)
	workflow.Go(ctx, func(gCtx workflow.Context) {
		defer lagDone.Send(gCtx, struct{}{})
		lagOpts := workflow.ActivityOptions{
			StartToCloseTimeout: 7 * 24 * time.Hour,
			HeartbeatTimeout:    2 * time.Minute,
		}
		var lag int64
		_ = workflow.ExecuteActivity(
			workflow.WithActivityOptions(gCtx, lagOpts),
			(*activities.PgstreamActivities).PollLag, cfg, 30*time.Second,
		).Get(gCtx, &lag)
		lagState.LagBytes = lag
		lagState.LastChecked = workflow.Now(gCtx)
	})

	// ── Step 4: Wait for stop signal, stream failure, or ContinueAsNew ───────
	iterations := 0
	sel := workflow.NewSelector(ctx)

	var streamErr error
	var stopped bool

	sel.AddFuture(streamFuture, func(f workflow.Future) {
		streamErr = f.Get(ctx, nil)
	})
	sel.AddReceive(stopCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
		stopped = true
	})

	for !stopped && streamErr == nil {
		sel.Select(ctx)
		iterations++

		// Drain any queued stop signals.
		for {
			var s string
			if !stopCh.ReceiveAsync(&s) {
				break
			}
			stopped = true
		}

		if iterations >= cfg.MaxIterations && !stopped && streamErr == nil {
			// ContinueAsNew keeps the event history bounded.
			logger.Info("CDCStreamWorkflow ContinueAsNew",
				slog.Int("iterations", iterations))
			return workflow.NewContinueAsNewError(ctx, CDCStreamWorkflow, cfg)
		}
	}

	// Cancel the lag-polling goroutine by cancelling the root ctx.
	// (In practice, cancellation propagates via Temporal's cancel activity API.)
	lagDone.Receive(ctx, nil)

	if streamErr != nil {
		// Wrap and page.
		var sErr *types.StreamError
		if !errors.As(streamErr, &sErr) {
			streamErr = &types.StreamError{StreamID: cfg.StreamID, Wrapped: streamErr}
		}
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "stream"}, streamErr, "critical", "cdc-stream-died")
		return streamErr
	}

	logger.Info("CDCStreamWorkflow stopped cleanly",
		slog.String("stream_id", cfg.StreamID))
	return nil
}
