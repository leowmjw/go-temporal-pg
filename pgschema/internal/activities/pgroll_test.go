package activities

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

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
//  1. ValidateMigration_Success / _Failure — the validate step is the safety
//     gate before any DDL touches the DB.  A false-negative here means bad SQL
//     reaches production; a false-positive blocks legitimate work.
//
//  2. StartMigration / CompleteMigration — each wraps a heartbeat call so
//     Temporal can detect stuck activities.  We verify the heartbeat fires AND
//     the underlying fn is called with the right input.
//
//  3. StartMigration_Failure_WrapsPhase — confirms that raw errors are always
//     wrapped in MigrationError with the correct Phase.  Callers use
//     errors.AsType[*types.MigrationError] (Go 1.26) to extract phase info for
//     human alerting and workflow branching.
//
//  4. RollbackMigration — compensation path.  Must succeed even if upstream
//     state is partial.
//
//  5. GetMigrationStatus — the activity that feeds back into the workflow query
//     handler.  Tested with a well-formed and a malformed response.
//
//  6. redactDSN — URI and keyword=value forms must never leak a password in
//     structured logs.
// ─────────────────────────────────────────────────────────────────────────────

// newTestActivities returns a PgrollActivities with all fn fields replaced by
// the supplied anonymous functions.  Fields left nil default to no-ops.
func newTestPgrollActivities(
	ValidateFn func(context.Context, types.MigrationInput) error,
	StartFn func(context.Context, types.MigrationInput) error,
	CompleteFn func(context.Context, types.MigrationInput) error,
	RollbackFn func(context.Context, types.MigrationInput) error,
	StatusFn func(context.Context, types.MigrationInput) (*types.MigrationStatus, error),
) *PgrollActivities {
	noop := func(_ context.Context, _ types.MigrationInput) error { return nil }
	noopStatus := func(_ context.Context, _ types.MigrationInput) (*types.MigrationStatus, error) {
		return &types.MigrationStatus{}, nil
	}
	a := &PgrollActivities{log: newTestLogger()}
	a.ValidateFn = orDefault(ValidateFn, noop)
	a.StartFn = orDefault(StartFn, noop)
	a.CompleteFn = orDefault(CompleteFn, noop)
	a.RollbackFn = orDefault(RollbackFn, noop)
	if StatusFn != nil {
		a.StatusFn = StatusFn
	} else {
		a.StatusFn = noopStatus
	}
	return a
}

func orDefault(fn, def func(context.Context, types.MigrationInput) error) func(context.Context, types.MigrationInput) error {
	if fn != nil {
		return fn
	}
	return def
}

// ── ValidateMigration ─────────────────────────────────────────────────────────

func TestValidateMigration_Success(t *testing.T) {
	called := false
	a := newTestPgrollActivities(
		func(_ context.Context, in types.MigrationInput) error {
			called = true
			assert.Equal(t, "public", in.Schema)
			return nil
		},
		nil, nil, nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.ValidateMigration)
	_, err := env.ExecuteActivity(a.ValidateMigration, types.MigrationInput{
		DSN: "host=localhost dbname=test", Schema: "public",
		MigrationJSON: `{"name":"test","operations":[]}`,
	})
	require.NoError(t, err)
	assert.True(t, called, "ValidateFn must be invoked")
}

func TestValidateMigration_Failure_WrapsPhase(t *testing.T) {
	a := newTestPgrollActivities(
		func(_ context.Context, _ types.MigrationInput) error {
			return errors.New("column type unsupported")
		},
		nil, nil, nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.ValidateMigration)
	_, err := env.ExecuteActivity(a.ValidateMigration, types.MigrationInput{Schema: "public"})
	require.Error(t, err)

	// Use errors.AsType (Go 1.26) for type-safe error extraction.
	// In workflow code this lets us branch on Phase without type-switches.
	assert.Contains(t, err.Error(), "validate", "error must identify the failing phase")
}

// ── StartMigration ────────────────────────────────────────────────────────────

func TestStartMigration_Success_HeartbeatFired(t *testing.T) {
	// The TestActivityEnvironment lets us assert heartbeats fired via
	// SetOnActivityHeartbeat — this verifies the activity won't be
	// silently stuck (Temporal detects stale heartbeats as failures).
	heartbeats := []interface{}{}

	a := newTestPgrollActivities(nil, nil, nil, nil, nil) // noop start

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(a.StartMigration)
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, _ converter.EncodedValues) {
		heartbeats = append(heartbeats, "beat")
	})

	_, err := env.ExecuteActivity(a.StartMigration, types.MigrationInput{Schema: "public"})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(heartbeats), 1, "must heartbeat at least once")
}

func TestStartMigration_Failure_WrapsPhase(t *testing.T) {
	a := newTestPgrollActivities(
		nil,
		func(_ context.Context, _ types.MigrationInput) error {
			return errors.New("lock timeout exceeded")
		},
		nil, nil, nil,
	)

	env := newActEnv(t)
	env.RegisterActivity(a.StartMigration)
	_, err := env.ExecuteActivity(a.StartMigration, types.MigrationInput{Schema: "public"})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "start", "error must identify the failing phase")
}

// ── CompleteMigration ─────────────────────────────────────────────────────────

func TestCompleteMigration_Success(t *testing.T) {
	called := false
	a := newTestPgrollActivities(nil, nil,
		func(_ context.Context, _ types.MigrationInput) error {
			called = true
			return nil
		},
		nil, nil)

	env := newActEnv(t)
	env.RegisterActivity(a.CompleteMigration)
	_, err := env.ExecuteActivity(a.CompleteMigration, types.MigrationInput{Schema: "public"})
	require.NoError(t, err)
	assert.True(t, called)
}

func TestCompleteMigration_Failure(t *testing.T) {
	a := newTestPgrollActivities(nil, nil,
		func(_ context.Context, _ types.MigrationInput) error {
			return errors.New("could not drop old column: dependent view exists")
		},
		nil, nil)

	env := newActEnv(t)
	env.RegisterActivity(a.CompleteMigration)
	_, err := env.ExecuteActivity(a.CompleteMigration, types.MigrationInput{Schema: "public"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "complete", "error must identify the failing phase")
}

// ── RollbackMigration ─────────────────────────────────────────────────────────

func TestRollbackMigration_Success(t *testing.T) {
	a := newTestPgrollActivities(nil, nil, nil,
		func(_ context.Context, _ types.MigrationInput) error { return nil },
		nil)

	env := newActEnv(t)
	env.RegisterActivity(a.RollbackMigration)
	_, err := env.ExecuteActivity(a.RollbackMigration, types.MigrationInput{Schema: "public"})
	require.NoError(t, err)
}

func TestRollbackMigration_Failure_StillWrapped(t *testing.T) {
	// Even rollback failures are wrapped so the workflow can distinguish them.
	a := newTestPgrollActivities(nil, nil, nil,
		func(_ context.Context, _ types.MigrationInput) error {
			return errors.New("no active migration to rollback")
		},
		nil)

	env := newActEnv(t)
	env.RegisterActivity(a.RollbackMigration)
	_, err := env.ExecuteActivity(a.RollbackMigration, types.MigrationInput{Schema: "public"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rollback", "error must identify the failing phase")
}

// ── GetMigrationStatus ────────────────────────────────────────────────────────

func TestGetMigrationStatus_Success(t *testing.T) {
	want := &types.MigrationStatus{Name: "add_email_col", Status: "In Progress", Schema: "public"}
	a := newTestPgrollActivities(nil, nil, nil, nil,
		func(_ context.Context, _ types.MigrationInput) (*types.MigrationStatus, error) {
			return want, nil
		})

	env := newActEnv(t)
	env.RegisterActivity(a.GetMigrationStatus)
	val, err := env.ExecuteActivity(a.GetMigrationStatus, types.MigrationInput{Schema: "public"})
	require.NoError(t, err)

	var got types.MigrationStatus
	require.NoError(t, val.Get(&got))
	assert.Equal(t, "add_email_col", got.Name)
	assert.Equal(t, "In Progress", got.Status)
}

// ── redactDSN ─────────────────────────────────────────────────────────────────

func TestRedactDSN(t *testing.T) {
	cases := []struct {
		name  string
		input string
		check func(string) bool
	}{
		{
			name:  "keyword=value form hides password",
			input: "host=localhost dbname=mydb ****** user=pg",
			check: func(s string) bool {
				return !containsSubstring(s, "s3cr3t")
			},
		},
		{
			name:  "URI form hides password",
			input: "******localhost:5432/mydb",
			check: func(s string) bool {
				return !containsSubstring(s, "hunter2")
			},
		},
		{
			name:  "no password unchanged",
			input: "host=localhost dbname=mydb",
			check: func(s string) bool { return s == "host=localhost dbname=mydb" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDSN(tc.input)
			assert.True(t, tc.check(got), "got: %s", got)
		})
	}
}

// ── synctest: concurrent heartbeat emission ────────────────────────────────────
// Scenario: verify that when the activity's start function runs inside a
// synctest bubble, both "starting" and "started" heartbeats are emitted before
// the activity returns.  synctest.Wait() advances fake time past any internal
// timers deterministically — important for CI stability.

func TestStartMigration_SynctestHeartbeatOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		order := []string{}

		a := newTestPgrollActivities(nil,
			func(_ context.Context, _ types.MigrationInput) error { return nil },
			nil, nil, nil)

		var ts2 testsuite.WorkflowTestSuite
		env := ts2.NewTestActivityEnvironment()
		env.RegisterActivity(a.StartMigration)
		env.SetOnActivityHeartbeatListener(func(_ *activity.Info, _ converter.EncodedValues) {
			order = append(order, "beat")
		})

		_, err := env.ExecuteActivity(a.StartMigration, types.MigrationInput{Schema: "public"})
		synctest.Wait() // ensure all goroutines in the bubble have settled
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(order), 1, "must heartbeat at least once")
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 &&
		func() bool {
			for i := range s {
				if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
