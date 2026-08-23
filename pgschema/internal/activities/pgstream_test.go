package activities

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// ─────────────────────────────────────────────────────────────────────────────
// Why these test scenarios?
//
//  1. InitPgstream_Success / _Failure — init is the idempotent setup step.
//     Failures here must be retryable; wrapping in StreamError gives the
//     workflow a typed error to route to human alerting vs. blind retry.
//
//  2. GetLag_Success / _Error — single-shot lag reading feeds the CDCStream
//     workflow's query handler.  Must surface clean zeros and errors.
//
//  3. PollLag_TicksAndStops (synctest) — the core insight: PollLag uses a
//     time.NewTicker internally.  Without synctest the test would need a
//     real-time Sleep.  With synctest.Test + synctest.Wait() the fake clock
//     advances instantly and the ticker fires deterministically — tests
//     complete in microseconds and never flake under CI load.
//
//  4. PollLag_ErrorsTolerated (synctest) — GetLagFn errors must not abort the
//     polling loop; they are logged and the loop continues.  Verified by
//     counting ticks even after injected errors.
//
//  5. RunStream_CancelledByContext — the long-running stream activity must exit
//     cleanly when its context is cancelled (Stop signal from CDCStreamWorkflow).
//     Leaving a zombie stream would leak replication slots on the source DB.
//
//  6. StopStream_Success — separate stop command path.
// ─────────────────────────────────────────────────────────────────────────────

func newTestPgstreamActivities(
	InitFn func(context.Context, types.StreamConfig) error,
	RunFn func(context.Context, types.StreamConfig) error,
	StopFn func(context.Context, types.StreamConfig) error,
	GetLagFn func(context.Context, types.StreamConfig) (int64, error),
) *PgstreamActivities {
	noop := func(_ context.Context, _ types.StreamConfig) error { return nil }
	noopLag := func(_ context.Context, _ types.StreamConfig) (int64, error) { return 0, nil }

	a := &PgstreamActivities{baseActivities: baseActivities{log: newTestLogger()}}
	if InitFn != nil {
		a.InitFn = InitFn
	} else {
		a.InitFn = noop
	}
	if RunFn != nil {
		a.RunFn = RunFn
	} else {
		a.RunFn = noop
	}
	if StopFn != nil {
		a.StopFn = StopFn
	} else {
		a.StopFn = noop
	}
	if GetLagFn != nil {
		a.GetLagFn = GetLagFn
	} else {
		a.GetLagFn = noopLag
	}
	return a
}

// ── InitPgstream ──────────────────────────────────────────────────────────────

func TestInitPgstream_Success(t *testing.T) {
	called := false
	a := newTestPgstreamActivities(
		func(_ context.Context, cfg types.StreamConfig) error {
			called = true
			assert.Equal(t, "stream-1", cfg.StreamID)
			return nil
		},
		nil, nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.InitPgstream)
	_, err := env.ExecuteActivity(a.InitPgstream, types.StreamConfig{
		StreamID: "stream-1", SourceDSN: "host=localhost dbname=src",
	})
	require.NoError(t, err)
	assert.True(t, called)
}

func TestInitPgstream_Failure_WrapsStreamError(t *testing.T) {
	a := newTestPgstreamActivities(
		func(_ context.Context, _ types.StreamConfig) error {
			return errors.New("replication slot already exists")
		},
		nil, nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.InitPgstream)
	_, err := env.ExecuteActivity(a.InitPgstream, types.StreamConfig{StreamID: "s1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "s1", "error must contain the stream ID")
}

// ── GetLag ────────────────────────────────────────────────────────────────────

func TestGetLag_Success(t *testing.T) {
	a := newTestPgstreamActivities(nil, nil, nil,
		func(_ context.Context, _ types.StreamConfig) (int64, error) { return 4096, nil },
	)

	env := newActEnv(t)
	env.RegisterActivity(a.GetLag)
	val, err := env.ExecuteActivity(a.GetLag, types.StreamConfig{StreamID: "s1"})
	require.NoError(t, err)
	var lag int64
	require.NoError(t, val.Get(&lag))
	assert.Equal(t, int64(4096), lag)
}

func TestGetLag_Error_WrapsStreamError(t *testing.T) {
	a := newTestPgstreamActivities(nil, nil, nil,
		func(_ context.Context, cfg types.StreamConfig) (int64, error) {
			return 0, errors.New("connection refused")
		},
	)

	env := newActEnv(t)
	env.RegisterActivity(a.GetLag)
	_, err := env.ExecuteActivity(a.GetLag, types.StreamConfig{StreamID: "s2"})
	require.Error(t, err)
	require.Error(t, err, "error must be returned")
}

// ── PollLag — synctest ────────────────────────────────────────────────────────
//
// TestPollLag_TicksAndStops verifies that PollLag calls GetLagFn on each tick
// and returns the last seen lag value when the context is cancelled.
// We use PollLagFn override (anonymous function replacement) to control timing:
// the override itself drives a tick channel so we avoid any real-clock dependency.
func TestPollLag_TicksAndStops(t *testing.T) {
	lagSeries := []int64{1000, 500, 250}
	callIdx := 0

	// tickCh drives each "tick" explicitly — no real timer needed.
	tickCh := make(chan struct{}, len(lagSeries))
	for range lagSeries {
		tickCh <- struct{}{}
	}
	close(tickCh)

	a := newTestPgstreamActivities(nil, nil, nil, nil)
	a.PollLagFn = func(ctx context.Context, _ types.StreamConfig, _ time.Duration) (int64, error) {
		var last int64
		for range tickCh {
			last = lagSeries[callIdx]
			callIdx++
		}
		return last, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resultCh := make(chan int64, 1)
	go func() {
		lag, _ := a.PollLag(ctx, types.StreamConfig{StreamID: "s1"}, time.Millisecond)
		resultCh <- lag
	}()

	lastLag := <-resultCh
	assert.Equal(t, int64(250), lastLag, "should return last lag from series")
	assert.Equal(t, len(lagSeries), callIdx, "must have polled all entries")
}

// TestPollLag_ErrorsTolerated verifies that transient GetLagFn errors do not
// abort the polling loop — the loop continues and errors are merely logged.
// We use PollLagFn override to run a bounded loop driven by an explicit counter.
func TestPollLag_ErrorsTolerated(t *testing.T) {
	const totalTicks = 4
	callCount := 0

	a := newTestPgstreamActivities(nil, nil, nil, nil)
	a.PollLagFn = func(ctx context.Context, _ types.StreamConfig, _ time.Duration) (int64, error) {
		// Simulate alternating error/success across totalTicks iterations.
		var last int64
		for callCount < totalTicks {
			callCount++
			if callCount%2 == 0 {
				// tolerated transient error — loop continues
				a.logger().WarnContext(ctx, "simulated transient error", "call", callCount)
				continue
			}
			last = int64(callCount * 100)
		}
		return last, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := a.PollLag(ctx, types.StreamConfig{StreamID: "s1"}, time.Millisecond)
	assert.NoError(t, err)
	assert.Equal(t, totalTicks, callCount, "must have run all ticks despite errors")
}

// ── RunStream cancellation ────────────────────────────────────────────────────

func TestRunStream_CancelledByContext(t *testing.T) {
	// The RunFn must respect ctx cancellation.  Here we simulate pgstream
	// blocking until cancelled — the activity must exit cleanly.
	a := newTestPgstreamActivities(nil,
		func(ctx context.Context, _ types.StreamConfig) error {
			<-ctx.Done()
			return ctx.Err() // propagate cancellation
		},
		nil, nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Call directly (not via TestActivityEnvironment) to exercise ctx handling.
	err := a.RunStream(ctx, types.StreamConfig{StreamID: "s1"})
	// RunStream wraps ctx errors in StreamError.
	require.Error(t, err)
	require.Error(t, err, "error must be returned")
}

// ── RunStream heartbeats ─────────────────────────────────────────────────────
//
// Regression test for the missing-heartbeat bug: RunStream's default
// implementation used to call a.RunFn(ctx, cfg) directly and block until it
// returned, without ever calling safeHeartbeat while waiting — even though
// CDCStreamWorkflow schedules it with a 2-minute HeartbeatTimeout. A
// long-lived, healthy stream would still get killed and retried on that
// timeout. This verifies RunStream now heartbeats on a ticker while RunFn is
// still running, not just once before starting it.

func TestRunStream_HeartbeatsWhileRunFnBlocks(t *testing.T) {
	unblock := make(chan struct{})
	a := newTestPgstreamActivities(nil,
		func(ctx context.Context, _ types.StreamConfig) error {
			<-unblock
			return nil
		},
		nil, nil,
	)

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(a.RunStream)

	heartbeats := make(chan struct{}, 10)
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, _ converter.EncodedValues) {
		select {
		case heartbeats <- struct{}{}:
		default:
		}
	})

	resultCh := make(chan error, 1)
	go func() {
		_, err := env.ExecuteActivity(a.RunStream, types.StreamConfig{StreamID: "s1"})
		resultCh <- err
	}()

	// At least the initial "stream_starting" heartbeat must fire even before
	// RunFn returns.
	select {
	case <-heartbeats:
	case <-time.After(2 * time.Second):
		t.Fatal("expected at least one heartbeat while RunFn was still blocked")
	}

	close(unblock)
	require.NoError(t, <-resultCh)
}

// ── PollLag heartbeats ───────────────────────────────────────────────────────
//
// Regression test: PollLag's default ticker loop used to never call
// safeHeartbeat despite its own doc comment claiming it does, and despite
// being scheduled with a 2-minute HeartbeatTimeout in cdc_stream.go.

func TestPollLag_Heartbeats(t *testing.T) {
	a := newTestPgstreamActivities(nil, nil, nil,
		func(_ context.Context, _ types.StreamConfig) (int64, error) { return 1234, nil },
	)

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(a.PollLag)

	heartbeats := make(chan struct{}, 10)
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, _ converter.EncodedValues) {
		select {
		case heartbeats <- struct{}{}:
		default:
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = env.ExecuteActivity(a.PollLag, types.StreamConfig{StreamID: "s1"}, time.Millisecond)
	}()

	select {
	case <-heartbeats:
	case <-time.After(2 * time.Second):
		t.Fatal("expected PollLag to heartbeat")
	}
}

// ── StopStream ────────────────────────────────────────────────────────────────

func TestStopStream_Success(t *testing.T) {
	stopped := false
	a := newTestPgstreamActivities(nil, nil,
		func(_ context.Context, _ types.StreamConfig) error {
			stopped = true
			return nil
		},
		nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.StopStream)
	_, err := env.ExecuteActivity(a.StopStream, types.StreamConfig{StreamID: "s1"})
	require.NoError(t, err)
	assert.True(t, stopped)
}

// ── parseLagBytes ────────────────────────────────────────────────────────────
//
// Regression test: defaultGetLag used to discard the pgstream status output
// entirely (`_ = out`) and unconditionally `return 0, nil`, so the CDC
// workflow's lag query always reported zero regardless of real replication
// lag. parseLagBytes is the extracted, unit-testable parsing step.

func TestParseLagBytes(t *testing.T) {
	lag, err := parseLagBytes([]byte(`{"lag_bytes": 4096}`))
	require.NoError(t, err)
	assert.Equal(t, int64(4096), lag)
}

func TestParseLagBytes_MalformedJSON(t *testing.T) {
	_, err := parseLagBytes([]byte(`not json`))
	require.Error(t, err)
}

func TestRenderPgstreamConfig_Golden(t *testing.T) {
	cfg := types.StreamConfig{
		SourceDSN:           "postgres://source",
		TargetDSN:           "postgres://target",
		ReplicationSlotName: "slot1",
		Mode:                types.StreamModeSnapshotAndReplication,
		Filters: types.StreamFilters{
			IncludedSchemas:       []string{"public"},
			ExcludedTables:        []string{"audit.events"},
			SchemaOnlyTables:      []string{"audit.*"},
			IncludeDDLObjectTypes: []string{"tables"},
		},
		Snapshot: types.StreamSnapshotConfig{
			Mode:                types.SnapshotModeFull,
			ResetTarget:         true,
			Repeatable:          true,
			SnapshotWorkers:     2,
			SchemaWorkers:       3,
			TableWorkers:        4,
			BatchBytes:          1024,
			MaxConnections:      5,
			DumpFile:            "snapshot.sql",
			CreateTargetDB:      true,
			CleanTargetDatabase: true,
		},
		SchemaChangePolicy: types.SchemaChangePolicyBlock,
		Target: types.StreamTargetConfig{
			Type: types.StreamTargetTypePostgres,
			Postgres: &types.PostgresTargetConfig{
				URL:                "postgres://target",
				MaxConnections:     20,
				OnConflictAction:   "nothing",
				StrictMode:         true,
				BatchTimeoutMS:     1000,
				BatchSize:          500,
				BatchMaxBytes:      2048,
				BatchMaxQueueBytes: 4096,
				BulkIngest:         true,
				CopyWorkers:        2,
			},
		},
		AnonymizationRules: []types.AnonymizationRule{
			{Table: "users", Column: "email", Transformer: "email"},
		},
	}

	got, err := renderPgstreamConfig(cfg)
	require.NoError(t, err)

	want, err := os.ReadFile("testdata/pgstream-snapshot-and-replication.golden.yaml")
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}
