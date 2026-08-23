// Package activities - pgstream CDC / anonymization activities.
//
// PgstreamActivities wraps the pgstream binary for Temporal activities.
// All external calls are controlled by function fields for easy test injection.
package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// PgstreamActivities holds CDC-related Temporal activities backed by pgstream.
type PgstreamActivities struct {
	baseActivities
	InitFn   func(ctx context.Context, cfg types.StreamConfig) error
	RunFn    func(ctx context.Context, cfg types.StreamConfig) error
	StopFn   func(ctx context.Context, cfg types.StreamConfig) error
	GetLagFn func(ctx context.Context, cfg types.StreamConfig) (int64, error)
	// PollLagFn optionally overrides the entire PollLag loop for testing.
	// When nil, the default ticker-based implementation is used.
	PollLagFn func(ctx context.Context, cfg types.StreamConfig, interval time.Duration) (int64, error)
}

// NewPgstreamActivities returns a PgstreamActivities wired to the real
// `pgstream` binary.  Any field can be replaced in tests.
func NewPgstreamActivities(log *slog.Logger) *PgstreamActivities {
	a := &PgstreamActivities{baseActivities: baseActivities{log: log}}
	a.InitFn = a.defaultInit
	a.RunFn = a.defaultRun
	a.StopFn = a.defaultStop
	a.GetLagFn = a.defaultGetLag
	return a
}

// ─── Temporal activity methods ────────────────────────────────────────────────

// InitPgstream initialises the pgstream metadata schema and replication slot
// in the source Postgres database.  Idempotent — safe to retry.
func (a *PgstreamActivities) InitPgstream(ctx context.Context, cfg types.StreamConfig) error {
	end := a.startTrace(ctx, "pgstream.init",
		slog.String("stream_id", cfg.StreamID),
		slog.String("slot", cfg.ReplicationSlotName))
	err := a.InitFn(ctx, cfg)
	end(err)
	if err != nil {
		return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return nil
}

// RunStream is a long-running Temporal activity that drives the pgstream
// replication loop.  It emits heartbeats every 30 s so Temporal can detect
// stuck workers.  The activity exits cleanly when ctx is cancelled (Stop
// signal from the workflow).
func (a *PgstreamActivities) RunStream(ctx context.Context, cfg types.StreamConfig) error {
	end := a.startTrace(ctx, "pgstream.run", slog.String("stream_id", cfg.StreamID))
	safeHeartbeat(ctx, "stream_starting")

	// RunFn (the real implementation shells out and blocks for the entire
	// stream lifetime, up to days). Run it in its own goroutine and heartbeat
	// on a ticker while we wait, so Temporal's HeartbeatTimeout doesn't fire
	// and force spurious retries of an otherwise-healthy stream.
	resultCh := make(chan error, 1)
	go func() { resultCh <- a.RunFn(ctx, cfg) }()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-resultCh:
			end(err)
			if err != nil {
				return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
			}
			return nil
		case <-ticker.C:
			safeHeartbeat(ctx, "stream_running")
		}
	}
}

// PollLag polls replication lag on a fixed interval until ctx is cancelled.
// Returns the last observed lag in bytes.  Emits heartbeats so Temporal knows
// the activity is alive.  This design — a regular ticker inside an activity —
// is tested with testing/synctest for deterministic timer control.
func (a *PgstreamActivities) PollLag(ctx context.Context, cfg types.StreamConfig, interval time.Duration) (int64, error) {
	if a.PollLagFn != nil {
		return a.PollLagFn(ctx, cfg, interval)
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastLag int64
	for {
		select {
		case <-ctx.Done():
			a.logger().InfoContext(ctx, "lag polling stopped",
				slog.String("flow", "end"),
				slog.String("op", "pgstream.poll_lag"),
				slog.String("stream_id", cfg.StreamID),
				slog.Int64("last_lag_bytes", lastLag))
			return lastLag, nil
		case <-ticker.C:
			lag, err := a.GetLagFn(ctx, cfg)
			if err != nil {
				a.logger().WarnContext(ctx, "failed to get lag",
					slog.String("op", "pgstream.poll_lag"),
					slog.String("stream_id", cfg.StreamID),
					slog.String("error", err.Error()))
				safeHeartbeat(ctx, "lag_poll_error")
				continue
			}
			lastLag = lag
			a.logger().InfoContext(ctx, "replication lag",
				slog.String("op", "pgstream.poll_lag"),
				slog.String("stream_id", cfg.StreamID),
				slog.Int64("lag_bytes", lag))
			safeHeartbeat(ctx, fmt.Sprintf("lag_bytes=%d", lastLag))
		}
	}
}

// StopStream gracefully signals pgstream to stop.
func (a *PgstreamActivities) StopStream(ctx context.Context, cfg types.StreamConfig) error {
	end := a.startTrace(ctx, "pgstream.stop", slog.String("stream_id", cfg.StreamID))
	err := a.StopFn(ctx, cfg)
	end(err)
	if err != nil {
		return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return nil
}

// GetLag returns the current replication lag once.
func (a *PgstreamActivities) GetLag(ctx context.Context, cfg types.StreamConfig) (int64, error) {
	lag, err := a.GetLagFn(ctx, cfg)
	if err != nil {
		return 0, &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return lag, nil
}

// ─── Default (binary) implementations ────────────────────────────────────────

func (a *PgstreamActivities) defaultInit(ctx context.Context, cfg types.StreamConfig) error {
	args := []string{
		"init",
		"--pgstream-pgurl", cfg.SourceDSN,
	}
	return a.runPgstream(ctx, args)
}

func (a *PgstreamActivities) defaultRun(ctx context.Context, cfg types.StreamConfig) error {
	args := []string{
		"run",
		"--source", "postgres",
		"--source-url", cfg.SourceDSN,
		"--target", "postgres",
		"--target-url", cfg.TargetDSN,
	}
	return a.runPgstreamHeartbeating(ctx, args, cfg.StreamID)
}

func (a *PgstreamActivities) defaultStop(_ context.Context, _ types.StreamConfig) error {
	// pgstream does not have a standalone stop command; cancelling the Run
	// context terminates it.  This is a no-op for the binary implementation.
	return nil
}

func (a *PgstreamActivities) defaultGetLag(ctx context.Context, cfg types.StreamConfig) (int64, error) {
	// pgstream exposes lag via its status command (implementation-specific).
	args := []string{"status", "--pgstream-pgurl", cfg.SourceDSN, "--output", "json"}
	out, err := a.runPgstreamOutput(ctx, args)
	if err != nil {
		return 0, err
	}
	return parseLagBytes(out)
}

// parseLagBytes extracts the replication lag (in bytes) from `pgstream status
// --output json` output.  Split out from defaultGetLag so it can be unit
// tested without shelling out to the real pgstream binary.
func parseLagBytes(out []byte) (int64, error) {
	var status struct {
		LagBytes int64 `json:"lag_bytes"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return 0, fmt.Errorf("parsing pgstream status output: %w", err)
	}
	return status.LagBytes, nil
}
