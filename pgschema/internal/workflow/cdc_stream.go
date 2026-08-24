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

	QueryStreamLag    = "lag"
	QueryStreamHealth = "health"

	defaultMaxIterations = 1000
	defaultHealthPoll    = 30 * time.Second
)

// CDCStreamWorkflow orchestrates a continuous pgstream CDC or snapshot
// pipeline:
//
//	CheckPreflight
//	  → InitPgstream (except snapshot-only)
//	    → RunStream (snapshot, replication, or snapshot+replication)
//	      → periodic GetStreamHealth polling
//	        → (stop signal | health guardrail | config restart | ContinueAsNew)
func CDCStreamWorkflow(ctx workflow.Context, cfg types.StreamConfig) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("CDCStreamWorkflow starting", slog.String("stream_id", cfg.StreamID))

	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = defaultMaxIterations
	}
	_ = workflow.GetVersion(ctx, "cdc-v2-health-and-preflight", workflow.DefaultVersion, 2)

	healthState := &types.StreamHealthResponse{
		Phase:               "starting",
		Status:              "running",
		Mode:                cfg.Mode,
		TargetType:          cfg.Target.Type,
		SchemaChangePolicy:  cfg.SchemaChangePolicy,
		ReplicationSlotName: cfg.ReplicationSlotName,
		LastChecked:         workflow.Now(ctx),
		Restart: types.RestartMetadata{
			Count:     cfg.RestartCount,
			Reason:    cfg.RestartReason,
			Initiator: cfg.RestartInitiator,
			LastAt:    cfg.LastRestartAt,
		},
	}

	if err := workflow.SetQueryHandler(ctx, QueryStreamLag, func() (*types.LagResponse, error) {
		return &types.LagResponse{
			LagBytes:    healthState.LagBytes,
			LastChecked: healthState.LastChecked,
		}, nil
	}); err != nil {
		return fmt.Errorf("register lag query: %w", err)
	}
	if err := workflow.SetQueryHandler(ctx, QueryStreamHealth, func() (*types.StreamHealthResponse, error) {
		return healthState, nil
	}); err != nil {
		return fmt.Errorf("register health query: %w", err)
	}

	stopCh := workflow.GetSignalChannel(ctx, SignalStopStream)
	restartCh := workflow.NewBufferedChannel(ctx, 4)

	shortOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
			InitialInterval: 2 * time.Second,
		},
	}
	shortCtx := workflow.WithActivityOptions(ctx, shortOpts)

	if err := workflow.SetUpdateHandlerWithOptions(ctx,
		"update-anon-rules",
		func(uCtx workflow.Context, rules []types.AnonymizationRule) (string, error) {
			if err := rejectRestartByPolicy(cfg, workflow.Now(uCtx)); err != nil {
				healthState.Restart.Rejected = true
				healthState.Restart.RejectNote = err.Error()
				return "", err
			}
			validateCtx := workflow.WithActivityOptions(uCtx, shortOpts)
			if err := workflow.ExecuteActivity(
				validateCtx,
				(*activities.PgstreamActivities).ValidateAnonymizationRules,
				cfg,
				rules,
			).Get(uCtx, nil); err != nil {
				return "", err
			}
			cfg.AnonymizationRules = rules
			cfg.RestartCount++
			cfg.LastRestartAt = workflow.Now(uCtx)
			cfg.RestartReason = "anonymization rules updated"
			cfg.RestartInitiator = "update-anon-rules"
			healthState.Restart = types.RestartMetadata{
				Count:     cfg.RestartCount,
				Reason:    cfg.RestartReason,
				Initiator: cfg.RestartInitiator,
				LastAt:    cfg.LastRestartAt,
			}
			restartCh.Send(uCtx, struct{}{})
			return fmt.Sprintf(
				"anonymization rules updated: %d rules; stream restarting to apply them",
				len(rules),
			), nil
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

	healthState.Phase = "preflight"
	var preflight *types.PreflightStatus
	if err := workflow.ExecuteActivity(
		shortCtx,
		(*activities.PgstreamActivities).CheckPreflight,
		cfg,
	).Get(ctx, &preflight); err != nil {
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "preflight"}, err, "critical", "cdc-preflight")
		return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	if preflight != nil {
		healthState.Preflight = *preflight
		healthState.SourceReachable = preflight.SourceReachable
		healthState.TargetReachable = preflight.TargetReachable
	}

	if cfg.Mode != types.StreamModeSnapshot {
		healthState.Phase = "init"
		if err := workflow.ExecuteActivity(
			shortCtx,
			(*activities.PgstreamActivities).InitPgstream,
			cfg,
		).Get(ctx, nil); err != nil {
			_ = pageOperator(ctx, &types.ProgressResponse{Phase: "init"}, err, "critical", "cdc-init")
			return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
		}
	}

	runCtx, cancelRun := workflow.WithCancel(ctx)

	streamOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 7 * 24 * time.Hour,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    20,
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 1.5,
			MaximumInterval:    5 * time.Minute,
		},
	}
	healthState.Phase = "stream"
	streamFuture := workflow.ExecuteActivity(
		workflow.WithActivityOptions(runCtx, streamOpts),
		(*activities.PgstreamActivities).RunStream,
		cfg,
	)

	healthDone := workflow.NewChannel(ctx)
	healthErrCh := workflow.NewBufferedChannel(ctx, 1)
	pollTickCh := workflow.NewBufferedChannel(ctx, 1)
	workflow.Go(runCtx, func(gCtx workflow.Context) {
		defer healthDone.Send(gCtx, struct{}{})

		pollCtx := workflow.WithActivityOptions(gCtx, workflow.ActivityOptions{
			StartToCloseTimeout: 2 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 1,
			},
		})

		var lagBreachedAt time.Time
		var inactiveSlotAt time.Time
		for {
			var sample *types.StreamHealthResponse
			err := workflow.ExecuteActivity(
				pollCtx,
				(*activities.PgstreamActivities).GetStreamHealth,
				cfg,
			).Get(gCtx, &sample)
			now := workflow.Now(gCtx)
			if err != nil {
				healthState.ConsecutivePollFailures++
				healthState.LastChecked = now
				healthState.LastError = err.Error()
				if max := cfg.Guardrails.MaxConsecutivePollFailures; max > 0 &&
					healthState.ConsecutivePollFailures >= max &&
					cfg.Guardrails.OnViolation == types.GuardrailActionStop {
					healthErrCh.Send(gCtx, fmt.Sprintf("health polling failed %d consecutive times: %v", healthState.ConsecutivePollFailures, err))
					return
				}
			} else if sample != nil {
				sample.LastChecked = now
				sample.Preflight = healthState.Preflight
				sample.Restart = healthState.Restart
				sample.ConsecutivePollFailures = 0
				*healthState = *sample

				if cfg.Guardrails.MaxLagBytes > 0 && sample.LagBytes > cfg.Guardrails.MaxLagBytes {
					if lagBreachedAt.IsZero() {
						lagBreachedAt = now
					}
					if cfg.Guardrails.OnViolation == types.GuardrailActionStop &&
						(cfg.Guardrails.MaxLagDuration <= 0 || now.Sub(lagBreachedAt) >= cfg.Guardrails.MaxLagDuration) {
						healthErrCh.Send(gCtx, fmt.Sprintf("lag guardrail exceeded: lag_bytes=%d max_lag_bytes=%d", sample.LagBytes, cfg.Guardrails.MaxLagBytes))
						return
					}
				} else {
					lagBreachedAt = time.Time{}
				}

				if !sample.ReplicationSlotActive {
					if inactiveSlotAt.IsZero() {
						inactiveSlotAt = now
					}
					if cfg.Guardrails.MaxInactiveSlotDuration > 0 &&
						cfg.Guardrails.OnViolation == types.GuardrailActionStop &&
						now.Sub(inactiveSlotAt) >= cfg.Guardrails.MaxInactiveSlotDuration {
						healthErrCh.Send(gCtx, "replication slot remained inactive past configured guardrail")
						return
					}
				} else {
					inactiveSlotAt = time.Time{}
				}
			}
			pollTickCh.Send(gCtx, struct{}{})

			if err := workflow.NewTimer(gCtx, defaultHealthPoll).Get(gCtx, nil); err != nil {
				return
			}
		}
	})

	iterations := 0
	sel := workflow.NewSelector(ctx)

	var streamErr error
	var streamCompleted bool
	var stopped bool
	var restart bool
	var healthFailure string

	sel.AddFuture(streamFuture, func(f workflow.Future) {
		streamCompleted = true
		streamErr = f.Get(ctx, nil)
	})
	sel.AddReceive(stopCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
		stopped = true
	})
	sel.AddReceive(restartCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
		restart = true
	})
	sel.AddReceive(healthErrCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, &healthFailure)
	})
	sel.AddReceive(pollTickCh, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
	})

	for !stopped && streamErr == nil && !restart && !streamCompleted && healthFailure == "" {
		sel.Select(ctx)
		iterations++

		for {
			var s string
			if !stopCh.ReceiveAsync(&s) {
				break
			}
			stopped = true
		}
		for {
			var r struct{}
			if !restartCh.ReceiveAsync(&r) {
				break
			}
			restart = true
		}
		if iterations >= cfg.MaxIterations && !stopped && streamErr == nil && !restart && healthFailure == "" {
			cancelRun()
			healthDone.Receive(ctx, nil)
			logger.Info("CDCStreamWorkflow ContinueAsNew", slog.Int("iterations", iterations))
			return workflow.NewContinueAsNewError(ctx, CDCStreamWorkflow, cfg)
		}
	}

	cancelRun()
	healthDone.Receive(ctx, nil)

	if stopped {
		logger.Info("CDCStreamWorkflow stopped cleanly", slog.String("stream_id", cfg.StreamID))
		return nil
	}
	if healthFailure != "" {
		err := &types.StreamError{StreamID: cfg.StreamID, Wrapped: errors.New(healthFailure)}
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "health"}, err, "critical", "cdc-health")
		return err
	}
	if streamErr != nil {
		var sErr *types.StreamError
		if !errors.As(streamErr, &sErr) {
			streamErr = &types.StreamError{StreamID: cfg.StreamID, Wrapped: streamErr}
		}
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "stream"}, streamErr, "critical", "cdc-stream-died")
		return streamErr
	}
	if streamCompleted && cfg.Mode == types.StreamModeSnapshot {
		healthState.Phase = "completed"
		healthState.Status = "completed"
		return nil
	}
	if streamCompleted && cfg.Mode != types.StreamModeSnapshot {
		err := &types.StreamError{StreamID: cfg.StreamID, Wrapped: errors.New("pgstream exited unexpectedly")}
		_ = pageOperator(ctx, &types.ProgressResponse{Phase: "stream"}, err, "critical", "cdc-stream-exited")
		return err
	}

	logger.Info("CDCStreamWorkflow restarting to apply updated stream configuration",
		slog.String("stream_id", cfg.StreamID),
		slog.String("reason", cfg.RestartReason))
	return workflow.NewContinueAsNewError(ctx, CDCStreamWorkflow, cfg)
}

func rejectRestartByPolicy(cfg types.StreamConfig, now time.Time) error {
	if cfg.RestartPolicy.MaxRestarts <= 0 {
		return nil
	}
	if cfg.RestartCount < cfg.RestartPolicy.MaxRestarts {
		return nil
	}
	if cfg.RestartPolicy.Window <= 0 {
		return fmt.Errorf("restart policy rejected update: max_restarts=%d reached", cfg.RestartPolicy.MaxRestarts)
	}
	if cfg.LastRestartAt.IsZero() || now.Sub(cfg.LastRestartAt) < cfg.RestartPolicy.Window {
		return fmt.Errorf(
			"restart policy rejected update: max_restarts=%d within %s",
			cfg.RestartPolicy.MaxRestarts,
			cfg.RestartPolicy.Window,
		)
	}
	return nil
}
