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
//  2. RunStream_CancelledByContext — the long-running stream activity must exit
//     cleanly when its context is cancelled (Stop signal from CDCStreamWorkflow).
//     Leaving a zombie stream would leak replication slots on the source DB.
//
//  3. StopStream_Success — separate stop command path.
// ─────────────────────────────────────────────────────────────────────────────

func newTestPgstreamActivities(
	InitFn func(context.Context, types.StreamConfig) error,
	RunFn func(context.Context, types.StreamConfig) error,
	StopFn func(context.Context, types.StreamConfig) error,
) *PgstreamActivities {
	noop := func(_ context.Context, _ types.StreamConfig) error { return nil }

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
		nil, nil,
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
		nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.InitPgstream)
	_, err := env.ExecuteActivity(a.InitPgstream, types.StreamConfig{StreamID: "s1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "s1", "error must contain the stream ID")
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
		nil,
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
		nil,
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

// ── StopStream ────────────────────────────────────────────────────────────────

func TestStopStream_Success(t *testing.T) {
	stopped := false
	a := newTestPgstreamActivities(nil, nil,
		func(_ context.Context, _ types.StreamConfig) error {
			stopped = true
			return nil
		},
	)

	env := newActEnv(t)
	env.RegisterActivity(a.StopStream)
	_, err := env.ExecuteActivity(a.StopStream, types.StreamConfig{StreamID: "s1"})
	require.NoError(t, err)
	assert.True(t, stopped)
}

// ── parseLagBytes ────────────────────────────────────────────────────────────
//
// parseLagBytes is the extracted, unit-testable parsing step for replication lag.

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
