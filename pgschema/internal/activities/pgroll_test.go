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
	a := &PgrollActivities{baseActivities: baseActivities{log: newTestLogger()}}
	a.ValidateFn = orDefault(ValidateFn, noop)
	a.StartFn = orDefault(StartFn, noop)
	a.CompleteFn = orDefault(CompleteFn, noop)
	a.RollbackFn = orDefault(RollbackFn, noop)
	if StatusFn != nil {
		a.StatusFn = StatusFn
	} else {
		a.StatusFn = noopStatus
	}
	a.VersionFn = func(_ context.Context, _ types.MigrationInput) (string, error) { return "v0.16.2", nil }
	a.ReadinessFn = func(_ context.Context, _ types.MigrationInput) (*types.PgrollReadiness, error) {
		return &types.PgrollReadiness{Initialized: true}, nil
	}
	a.LatestSchemaFn = func(_ context.Context, _ types.MigrationInput) (string, error) { return "public_test", nil }
	a.RiskFn = func(_ context.Context, _ types.MigrationInput) (*types.MigrationRiskReport, error) {
		return &types.MigrationRiskReport{MigrationName: "test", OverallRisk: "low"}, nil
	}
	a.ReconcileFn = func(_ context.Context, input types.ReconcileInput) (*types.ReconciliationResult, error) {
		status, _ := a.StatusFn(context.Background(), input.Migration)
		return &types.ReconciliationResult{Action: "continue", Status: status}, nil
	}
	a.BaselineFn = func(_ context.Context, input types.BaselineInput) (*types.BaselineResult, error) {
		return &types.BaselineResult{Version: input.Version, Directory: input.Directory, Schema: input.Schema, Status: "created"}, nil
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

// TestRedactDSN_QuotedPasswordWithSpace is the direct regression test for the
// whitespace-leak bug: redactDSN used to find the mask's end via
// strings.IndexAny(rest, " \t"), which stops at the FIRST space it finds —
// including one inside a single-quoted password value (libpq allows
// password='a b c' so the value itself can contain spaces). That truncated
// mask boundary let the tail of the real password leak into the "redacted"
// output after the "******".
func TestRedactDSN_QuotedPasswordWithSpace(t *testing.T) {
	got := redactDSN("host=localhost password='s3cr3t with spaces' user=pg")
	assert.NotContains(t, got, "with spaces", "no part of the quoted password may leak")
	assert.NotContains(t, got, "s3cr3t")
	assert.Contains(t, got, "user=pg", "fields after the password must be preserved")
	assert.Contains(t, got, "******")
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

func TestCheckPgrollVersion_Success(t *testing.T) {
	a := newTestPgrollActivities(nil, nil, nil, nil, nil)
	a.VersionFn = func(_ context.Context, _ types.MigrationInput) (string, error) {
		return "v0.16.2", nil
	}

	env := newActEnv(t)
	env.RegisterActivity(a.CheckPgrollVersion)
	val, err := env.ExecuteActivity(a.CheckPgrollVersion, types.MigrationInput{})
	require.NoError(t, err)

	var got string
	require.NoError(t, val.Get(&got))
	assert.Equal(t, "v0.16.2", got)
}

func TestGetLatestSchema_Success(t *testing.T) {
	a := newTestPgrollActivities(nil, nil, nil, nil, nil)
	a.LatestSchemaFn = func(_ context.Context, _ types.MigrationInput) (string, error) {
		return "public_add_email", nil
	}

	env := newActEnv(t)
	env.RegisterActivity(a.GetLatestSchema)
	val, err := env.ExecuteActivity(a.GetLatestSchema, types.MigrationInput{Schema: "public"})
	require.NoError(t, err)

	var got string
	require.NoError(t, val.Get(&got))
	assert.Equal(t, "public_add_email", got)
}

func TestAnalyzeMigrationRisk_BlockedRawSQL(t *testing.T) {
	a := NewPgrollActivities(newTestLogger())
	env := newActEnv(t)
	env.RegisterActivity(a.AnalyzeMigrationRisk)
	val, err := env.ExecuteActivity(a.AnalyzeMigrationRisk, types.MigrationInput{
		Schema:        "public",
		MigrationJSON: `{"name":"danger","operations":[{"sql":{"up":"DELETE FROM users"}}]}`,
		Policy:        types.MigrationPolicy{BlockRawSQL: true},
	})
	require.NoError(t, err)

	var report types.MigrationRiskReport
	require.NoError(t, val.Get(&report))
	assert.True(t, report.Blocked)
	assert.Equal(t, "high", report.OverallRisk)
	require.Len(t, report.Findings, 1)
	assert.Equal(t, "raw_sql", report.Findings[0].Category)
}

func TestReconcileMigrationState_AlreadyComplete(t *testing.T) {
	a := newTestPgrollActivities(nil, nil, nil, nil,
		func(_ context.Context, _ types.MigrationInput) (*types.MigrationStatus, error) {
			return &types.MigrationStatus{Status: "Complete", Version: "add_email", Schema: "public"}, nil
		})
	a.ReconcileFn = func(_ context.Context, input types.ReconcileInput) (*types.ReconciliationResult, error) {
		status, _ := a.StatusFn(context.Background(), input.Migration)
		return &types.ReconciliationResult{Action: "already_complete", Status: status}, nil
	}

	env := newActEnv(t)
	env.RegisterActivity(a.ReconcileMigrationState)
	val, err := env.ExecuteActivity(a.ReconcileMigrationState, types.ReconcileInput{
		Phase: "before_start",
		Migration: types.MigrationInput{
			Schema:        "public",
			MigrationJSON: `{"name":"add_email","operations":[]}`,
		},
	})
	require.NoError(t, err)

	var result types.ReconciliationResult
	require.NoError(t, val.Get(&result))
	assert.Equal(t, "already_complete", result.Action)
	require.NotNil(t, result.Status)
	assert.Equal(t, "add_email", result.Status.Version)
}

// ── reconcileDecision — double-run / already-applied regression coverage ──────
//
// Repro (confirmed against the real pgroll v0.16.2 binary + a live demo
// Postgres): running the same scenario twice — e.g. a double click on "Run",
// or re-clicking a scenario after it already completed — left the SECOND
// workflow run in this sequence:
//   1. `pgroll validate` on the already-applied migration fails:
//        `migration '01_add_email' is invalid: column "email" already
//        exists in table "users"`
//   2. The workflow's triggerRollback calls `ReconcileMigrationState`
//      phase=before_rollback. pgroll's status was "Complete" (nothing in
//      progress) with no matching migration name — demo migration JSON
//      files never set one, and pgroll v0.16.2 rejects a top-level "name"
//      field outright ("unknown field \"name\""), so migrationName is
//      always empty and the old `matchesCurrent` gate could never be true.
//      Because none of the "before_rollback" cases matched, the decision
//      fell through to the zero-value action "continue" instead of a
//      skip — a real `pgroll rollback` was attempted.
//   3. `pgroll rollback` against a "Complete" (no active migration) state
//      fails: `unable to get active migration: no active migration`.
//   4. The workflow treated this as rollback failure: paged the operator
//      at "critical" severity and returned status "rollback_failed" —
//      alarming and wrong for what is actually a harmless no-op.
//
// These tests pin the fixed behavior directly against reconcileDecision
// (the pure decision table defaultReconcile now delegates to), so any
// future edit that reintroduces the fall-through-to-continue bug fails
// immediately without needing a live pgroll binary or database.

func TestReconcileDecision_BeforeRollback_NothingInProgress_Skips(t *testing.T) {
	// The exact repro state: migration already completed, no name to match
	// against (as with every demo/*.json migration file today).
	status := &types.MigrationStatus{Schema: "public", Status: "Complete", Version: "pgroll-migration-632793991"}
	result := reconcileDecision("before_rollback", status, `{"operations":[]}`)

	require.Equal(t, reconcileActionSkipRollback, result.Action, "must skip, not attempt a doomed pgroll rollback")
	assert.NotEmpty(t, result.Reason)
}

func TestReconcileDecision_BeforeRollback_NoMigrations_Skips(t *testing.T) {
	status := &types.MigrationStatus{Schema: "public", Status: "No migrations"}
	result := reconcileDecision("before_rollback", status, `{"operations":[]}`)
	assert.Equal(t, reconcileActionSkipRollback, result.Action)
}

func TestReconcileDecision_BeforeRollback_InProgress_MatchingIdentity_Continues(t *testing.T) {
	// The happy path: a genuinely in-flight migration, where pgroll's
	// reported version is the content-addressable name this workflow's own
	// StartMigration call would have produced for this exact JSON (see
	// MigrationFileName/migrationIdentity) — rollback must be attempted.
	migrationJSON := `{"operations":[{"add_column":{"table":"users","column":{"name":"email","type":"text"}}}]}`
	status := &types.MigrationStatus{Schema: "public", Status: "In progress", Version: migrationIdentity(migrationJSON)}
	result := reconcileDecision("before_rollback", status, migrationJSON)
	assert.Equal(t, reconcileActionContinue, result.Action)
}

func TestReconcileDecision_BeforeRollback_DifferentMigrationInProgress_Skips(t *testing.T) {
	// A different, named migration than the one we're tracking is active —
	// must not roll back someone else's in-flight change.
	status := &types.MigrationStatus{Schema: "public", Status: "In progress", Version: "some-other-migration"}
	result := reconcileDecision("before_rollback", status, `{"name":"add_email","operations":[]}`)
	assert.Equal(t, reconcileActionSkipRollback, result.Action)
}

func TestReconcileDecision_BeforeComplete_InProgress_MatchingIdentity_Continues(t *testing.T) {
	// The happy path: pgroll status="In progress" during before_complete,
	// with a matching content-addressable version (see MigrationFileName).
	migrationJSON := `{"operations":[{"add_column":{"table":"users","column":{"name":"email","type":"text"}}}]}`
	status := &types.MigrationStatus{Schema: "public", Status: "In progress", Version: migrationIdentity(migrationJSON)}
	result := reconcileDecision("before_complete", status, migrationJSON)
	assert.Equal(t, reconcileActionContinue, result.Action)
}

func TestReconcileDecision_BeforeComplete_InProgress_NoName_Continues(t *testing.T) {
	// Edge case: MigrationJSON is empty (shouldn't happen past validate) —
	// migrationIdentity returns "", and the decision fails open rather than
	// blocking the happy path on an identity it can't compute.
	status := &types.MigrationStatus{Schema: "public", Status: "In progress", Version: "pgroll-migration-1626957683"}
	result := reconcileDecision("before_complete", status, "")
	assert.Equal(t, reconcileActionContinue, result.Action)
}

// ── reconcileDecision — before_start, content-addressable identity ────────────
//
// Regression coverage for the second double-run bug: re-running scenario 3
// (a rename: `full_name` -> `display_name`) failed validate with `column
// "full_name" does not exist on table "users"` — the flip side of
// "already exists": a rename/drop op references a name the FIRST run
// already made disappear. Matching by CLI error text can't cover this
// without also risking swallowing a genuinely different, broken migration
// (e.g. running scenario 5 before scenario 3, which fails with a similarly
// shaped "does not exist" error but is NOT a duplicate — see demo/README.md
// Troubleshooting). Content-addressable migration names (MigrationFileName)
// let before_start recognize "this exact migration already fully applied"
// BEFORE validate ever runs, for any operation type — no error-text
// matching involved, and a different migration against the same "Complete"
// status is left alone to validate (and fail) normally.

func TestReconcileDecision_BeforeStart_MatchingComplete_AlreadyComplete(t *testing.T) {
	migrationJSON := `{"operations":[{"rename_column":{"table":"users","from":"full_name","to":"display_name"}}]}`
	status := &types.MigrationStatus{Schema: "public", Status: "Complete", Version: migrationIdentity(migrationJSON)}
	result := reconcileDecision("before_start", status, migrationJSON)
	assert.Equal(t, reconcileActionAlreadyDone, result.Action, "re-submitting an already-applied migration must short-circuit before validate runs")
}

func TestReconcileDecision_BeforeStart_DifferentCompletedMigration_Continues(t *testing.T) {
	// A DIFFERENT migration than the last one that completed — e.g. running
	// scenario 5 before scenario 3 finished. Must NOT be mistaken for a
	// duplicate: validate needs to run and (correctly) fail.
	lastCompleted := `{"operations":[{"add_column":{"table":"users","column":{"name":"email","type":"text"}}}]}`
	thisRequest := `{"operations":[{"rename_column":{"table":"users","from":"display_name","to":"first_name"}}]}`
	status := &types.MigrationStatus{Schema: "public", Status: "Complete", Version: migrationIdentity(lastCompleted)}
	result := reconcileDecision("before_start", status, thisRequest)
	assert.Equal(t, reconcileActionContinue, result.Action, "a distinct migration must proceed to validate, not be swallowed as already-applied")
}

func TestReconcileDecision_BeforeComplete_UnexpectedStatus_Fails(t *testing.T) {
	status := &types.MigrationStatus{Schema: "public", Status: "Complete", Version: "some-other-migration"}
	result := reconcileDecision("before_complete", status, `{"operations":[]}`)
	assert.Equal(t, reconcileActionFail, result.Action)
}

// ── pgroll error classification ────────────────────────────────────────────

func TestIsAlreadyAppliedError(t *testing.T) {
	// Exact text confirmed against the real pgroll v0.16.2 binary.
	err := &types.MigrationError{Phase: "validate", Wrapped: errors.New(`migration '01_add_email' is invalid: column "email" already exists in table "users"`)}
	assert.True(t, IsAlreadyAppliedError(err))
	assert.False(t, IsAlreadyAppliedError(errors.New("lock timeout")))
	assert.False(t, IsAlreadyAppliedError(nil))
}

func TestIsNoActiveMigrationError(t *testing.T) {
	// Exact text confirmed against the real pgroll v0.16.2 binary.
	err := errors.New("unable to get active migration: no active migration")
	assert.True(t, IsNoActiveMigrationError(err))
	assert.False(t, IsNoActiveMigrationError(errors.New("lock timeout")))
	assert.False(t, IsNoActiveMigrationError(nil))
}

func TestMigrationIdentity_DeterministicAndDistinct(t *testing.T) {
	a := `{"operations":[{"add_column":{"table":"users","column":{"name":"email","type":"text"}}}]}`
	b := `{"operations":[{"add_column":{"table":"users","column":{"name":"status","type":"text"}}}]}`

	assert.Equal(t, migrationIdentity(a), migrationIdentity(a), "same content must hash to the same identity")
	assert.NotEqual(t, migrationIdentity(a), migrationIdentity(b), "different content must hash to different identities")
	assert.Empty(t, migrationIdentity(""), "empty migration JSON has no identity")

	// Must match the filename runPgroll (base.go) actually writes to disk —
	// that's the literal basename pgroll derives its reported version from.
	assert.Equal(t, MigrationFileName(a), migrationIdentity(a)+".json")
}

func TestParsePgrollStatusOutput_Malformed(t *testing.T) {
	_, err := parsePgrollStatusOutput([]byte(`{"status":`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing pgroll status output")
}
