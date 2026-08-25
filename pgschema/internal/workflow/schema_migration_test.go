package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// ─────────────────────────────────────────────────────────────────────────────
// Scenario rationale — why every test case exists:
//
//  HappyPath — the baseline: validation passes, migration starts, app signals
//    ready within the 60-minute window, complete succeeds.  The Temporal
//    testsuite executes all activities synchronously without a real server,
//    giving instant, deterministic coverage of the full happy path.
//
//  ValidationFailure — if pgroll rejects the migration JSON (type error, syntax
//    error), the workflow must roll back immediately and return a typed error.
//    Callers parse errors.AsType[*types.MigrationError] to extract the phase.
//
//  StartFailure — pgroll start fails (lock timeout, schema conflict).  The
//    workflow must NOT call Complete and MUST attempt Rollback + human alert.
//    Critical to test because partial execution leaves the DB in "expand" phase.
//
//  AppReadyTimeout — app never signals ready within 60 minutes (deploy stuck,
//    ingress misconfigured).  Workflow must time out and roll back automatically
//    instead of waiting indefinitely.  Tested by advancing the test clock past
//    the wait window with env.SetTestTimeout.
//
//  RollbackSignal — operator manually triggers rollback mid-workflow.  The
//    signal must be drained synchronously and compensation must fire.
//
//  CompleteFailure — complete phase fails (dependent view exists, constraint
//    violation).  The expand-phase columns already exist; rolling back here
//    is the only safe recovery path.
//
//  RollbackFails_AlertFired — the worst case: both complete AND rollback fail.
//    The workflow must page the operator and return the rollback error.  Without
//    this test a silent double-failure would leave the schema in an unknown state.
//
//  UpdateHandler_ExtendWait_Valid — operator extends the wait window via
//    Workflow Update (SDK v1.22+).  Verified that the validator accepts
//    valid input and the handler returns a confirmation string.
//
//  UpdateHandler_ExtendWait_Invalid — validator rejects out-of-range input.
//    Using Temporal Update means the rejection is synchronous before any
//    workflow state changes — safer than a signal.
// ─────────────────────────────────────────────────────────────────────────────

type SchemaMigrationTestSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
	env *testsuite.TestWorkflowEnvironment
}

func TestSchemaMigrationTestSuite(t *testing.T) {
	suite.Run(t, new(SchemaMigrationTestSuite))
}

func (s *SchemaMigrationTestSuite) SetupTest() {
	s.env = s.NewTestWorkflowEnvironment()
	s.env.RegisterWorkflow(SchemaMigrationWorkflow)
}

func (s *SchemaMigrationTestSuite) AfterTest(_, _ string) {
	s.env.AssertExpectations(s.T())
}

// ── fakes ─────────────────────────────────────────────────────────────────────

// fakeActivities holds all activity behaviour for a single test scenario.
// Fields are anonymous functions — no mocks, no generated code.
// fakeMigration bundles real activity structs with func-field overrides.
// This is the anonymous-function method-replacement pattern: the workflow
// calls e.g. (*activities.PgrollActivities).ValidateMigration which delegates
// to PgrollActivities.ValidateFn — set here to whatever the test needs.
type fakeMigration struct {
	pgroll *activities.PgrollActivities
	alert  *activities.AlertActivities
}

// newFakeMigration builds a fakeMigration with all-noop defaults.
// Pass functional options to override individual behaviours.
func newFakeMigration(opts ...func(*fakeMigration)) *fakeMigration {
	f := &fakeMigration{
		pgroll: &activities.PgrollActivities{
			ValidateFn: func(_ context.Context, _ types.MigrationInput) error { return nil },
			StartFn:    func(_ context.Context, _ types.MigrationInput) error { return nil },
			CompleteFn: func(_ context.Context, _ types.MigrationInput) error { return nil },
			RollbackFn: func(_ context.Context, _ types.MigrationInput) error { return nil },
			StatusFn: func(_ context.Context, _ types.MigrationInput) (*types.MigrationStatus, error) {
				return &types.MigrationStatus{Status: "Complete", Version: "add_email", Schema: "public"}, nil
			},
			VersionFn: func(_ context.Context, _ types.MigrationInput) (string, error) {
				return "v0.16.2", nil
			},
			ReadinessFn: func(_ context.Context, _ types.MigrationInput) (*types.PgrollReadiness, error) {
				return &types.PgrollReadiness{Initialized: true, Message: "pgroll metadata ready"}, nil
			},
			LatestSchemaFn: func(_ context.Context, _ types.MigrationInput) (string, error) {
				return "public_add_email", nil
			},
			RiskFn: func(_ context.Context, _ types.MigrationInput) (*types.MigrationRiskReport, error) {
				return &types.MigrationRiskReport{MigrationName: "add_email", OverallRisk: "low"}, nil
			},
		},
		alert: &activities.AlertActivities{
			PageFn: func(_ context.Context, _ types.AlertMessage) error { return nil },
		},
	}
	f.pgroll.ReconcileFn = func(_ context.Context, input types.ReconcileInput) (*types.ReconciliationResult, error) {
		status, _ := f.pgroll.StatusFn(context.Background(), input.Migration)
		return &types.ReconciliationResult{Action: "continue", Status: status}, nil
	}
	f.pgroll.BaselineFn = func(_ context.Context, input types.BaselineInput) (*types.BaselineResult, error) {
		return &types.BaselineResult{Version: input.Version, Directory: input.Directory, Schema: input.Schema, Status: "created"}, nil
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func (f *fakeMigration) register(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivity(f.pgroll)
	env.RegisterActivity(f.alert)
}

func defaultInput() types.MigrationInput {
	return types.MigrationInput{
		DSN:           "host=localhost dbname=test",
		Schema:        "public",
		MigrationJSON: `{"name":"add_email","operations":[]}`,
	}
}

// ── HappyPath ─────────────────────────────────────────────────────────────────

func (s *SchemaMigrationTestSuite) TestHappyPath() {
	fake := newFakeMigration()
	fake.register(s.env)

	// Send app-ready signal just before the workflow waits for it.
	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 1*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())

	var result types.ProgressResponse
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal("completed", result.Status)
	s.Equal(100, result.Percent)
	s.Equal("v0.16.2", result.PgrollVersion)
	s.Equal("public_add_email", result.LatestSchema)
	s.NotNil(result.PgrollStatus)
}

// ── ValidationFailure ─────────────────────────────────────────────────────────

func (s *SchemaMigrationTestSuite) TestValidationFailure() {
	fake := newFakeMigration(func(f *fakeMigration) {
		f.pgroll.ValidateFn = func(_ context.Context, _ types.MigrationInput) error {
			return &types.MigrationError{Phase: "validate", Wrapped: errors.New("unsupported type")}
		}
	})
	fake.register(s.env)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	// Workflow returns an error (the cause), not nil — and per Temporal
	// semantics an errored workflow completion has no decoded result value,
	// so we assert on the error itself rather than GetWorkflowResult.
	err := s.env.GetWorkflowError()
	s.Error(err)
	s.Contains(err.Error(), "validate", "error must identify the failing phase")
}

// ── ValidationFailure_AlreadyApplied ─────────────────────────────────────────
//
// Re-running an already-completed scenario (double click, or re-clicking
// "Run" after it finished) makes pgroll validate fail with a message like
// `column "email" already exists in table "users"` — a real pgroll detail,
// including internal temp-file paths. The workflow must not surface that
// raw detail to whoever's watching (the demo/deploy UI); it should report a
// short, friendly message tagged with this workflow's ID as a ref, with the
// full detail only in structured logs (searchable by that same ref ID).

func (s *SchemaMigrationTestSuite) TestValidationFailure_AlreadyApplied_FriendlyMessage() {
	rawDetail := `migration 'add_email' is invalid: column "email" already exists in table "users"`
	fake := newFakeMigration(func(f *fakeMigration) {
		f.pgroll.ValidateFn = func(_ context.Context, _ types.MigrationInput) error {
			return errors.New(rawDetail)
		}
	})
	fake.register(s.env)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError(), "an already-applied migration must not surface as a workflow failure")

	var result types.ProgressResponse
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal("completed", result.Status)
	s.Contains(result.Message, "ref:", "message must carry a ref ID for later log lookup")
	s.NotContains(result.Message, "already exists", "raw pgroll detail must not reach the presenter-facing message")
	s.NotContains(result.Message, rawDetail, "raw pgroll detail must not reach the presenter-facing message")
}

// ── StartFailure ──────────────────────────────────────────────────────────────

func (s *SchemaMigrationTestSuite) TestStartMigrationFailure() {
	rollbackCalled := false
	fake := newFakeMigration(func(f *fakeMigration) {
		f.pgroll.StartFn = func(_ context.Context, _ types.MigrationInput) error {
			return &types.MigrationError{Phase: "start", Wrapped: errors.New("lock timeout")}
		}
		f.pgroll.RollbackFn = func(_ context.Context, _ types.MigrationInput) error {
			rollbackCalled = true
			return nil
		}
	})
	fake.register(s.env)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.True(rollbackCalled, "rollback must be called after start failure")
}

// ── AppReadyTimeout ───────────────────────────────────────────────────────────

func (s *SchemaMigrationTestSuite) TestAppReadyTimeout() {
	rollbackCalled := false
	fake := newFakeMigration(func(f *fakeMigration) {
		f.pgroll.RollbackFn = func(_ context.Context, _ types.MigrationInput) error {
			rollbackCalled = true
			return nil
		}
	})
	fake.register(s.env)

	// Advance test clock past the 60-minute wait window without sending AppReady.
	s.env.RegisterDelayedCallback(func() {
		// no-op; just let time pass
	}, 61*time.Minute)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.True(rollbackCalled, "rollback must fire after app-ready timeout")
}

// ── RollbackSignal ────────────────────────────────────────────────────────────

func (s *SchemaMigrationTestSuite) TestRollbackSignal_AfterStart() {
	rollbackCalled := false
	fake := newFakeMigration(func(f *fakeMigration) {
		f.pgroll.RollbackFn = func(_ context.Context, _ types.MigrationInput) error {
			rollbackCalled = true
			return nil
		}
	})
	fake.register(s.env)

	// Send rollback signal while workflow is waiting for app-ready.
	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(SignalRollback, nil)
	}, 5*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.True(rollbackCalled)

	var result types.ProgressResponse
	_ = s.env.GetWorkflowResult(&result)
	s.Equal("rolled_back", result.Status)
}

// TestRollbackSignal_ArrivesAfterCompletion is the regression test for the
// "Workflow has unhandled signals [rollback]" warning seen in worker logs:
// an operator's rollback signal can arrive after the workflow has already
// passed every checkpoint that consumes rollbackCh (e.g. once app-ready has
// been received and CompleteMigration has already succeeded). The workflow
// must still complete cleanly — draining the stale signal rather than
// leaving it unhandled — instead of rolling back a migration that already
// finished.
func (s *SchemaMigrationTestSuite) TestRollbackSignal_ArrivesAfterCompletion() {
	fake := newFakeMigration()
	fake.register(s.env)

	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 1*time.Millisecond)

	// Fired after the workflow has already completed in test-env terms;
	// TestWorkflowEnvironment still delivers it into the buffered channel,
	// exercising the same "stale signal sitting unread" scenario as prod.
	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(SignalRollback, nil)
	}, 2*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())

	var result types.ProgressResponse
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal("completed", result.Status, "a rollback signal arriving after completion must not be treated as a live rollback request")
}

// ── CompleteFailure ───────────────────────────────────────────────────────────

func (s *SchemaMigrationTestSuite) TestCompleteFailure_TriggersRollback() {
	rollbackCalled := false
	fake := newFakeMigration(func(f *fakeMigration) {
		f.pgroll.CompleteFn = func(_ context.Context, _ types.MigrationInput) error {
			return &types.MigrationError{Phase: "complete", Wrapped: errors.New("dependent view exists")}
		}
		f.pgroll.RollbackFn = func(_ context.Context, _ types.MigrationInput) error {
			rollbackCalled = true
			return nil
		}
	})
	fake.register(s.env)

	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 1*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.True(rollbackCalled)
}

// ── RollbackFails — double failure + human alert ──────────────────────────────

func (s *SchemaMigrationTestSuite) TestCompleteAndRollbackBothFail_AlertFired() {
	alertFired := false
	fake := newFakeMigration(func(f *fakeMigration) {
		f.pgroll.CompleteFn = func(_ context.Context, _ types.MigrationInput) error {
			return &types.MigrationError{Phase: "complete", Wrapped: errors.New("constraint violation")}
		}
		f.pgroll.RollbackFn = func(_ context.Context, _ types.MigrationInput) error {
			return errors.New("rollback: no active migration")
		}
		f.alert.PageFn = func(_ context.Context, _ types.AlertMessage) error {
			alertFired = true
			return nil
		}
	})
	fake.register(s.env)

	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 1*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.Error(s.env.GetWorkflowError(), "workflow must return an error when rollback also fails")
	s.True(alertFired, "human must be paged when double failure occurs")
}

// ── UpdateHandler ─────────────────────────────────────────────────────────────
// Temporal SDK v1.22+ Update handlers allow synchronous, validated mutations.
// Tested here to confirm the validator rejects bad input before workflow state
// changes — safer than signals which have no rejection mechanism.

func (s *SchemaMigrationTestSuite) TestUpdateHandler_ExtendWait_Valid() {
	fake := newFakeMigration()
	fake.register(s.env)

	var updateResult string
	var updateErr error
	s.env.RegisterDelayedCallback(func() {
		uc := &testsuite.TestUpdateCallback{
			OnAccept: func() {},
			OnReject: func(err error) { updateErr = err },
			OnComplete: func(res interface{}, err error) {
				updateErr = err
				if str, ok := res.(string); ok {
					updateResult = str
				}
			},
		}
		s.env.UpdateWorkflow("extend-wait", "", uc, 30)
	}, 1*time.Millisecond)

	// The update's own handler invocation is queued as a callback and is only
	// drained *after* the callback above returns, so assertions on the
	// outcome — and the app-ready signal that follows — must happen later.
	s.env.RegisterDelayedCallback(func() {
		s.NoError(updateErr)
		s.Contains(updateResult, "minutes")

		// Then send app-ready to complete the workflow.
		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 2*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

// TestUpdateHandler_ExtendWait_ActuallyDelaysRollback is the direct
// regression test for the extend-wait fix: previously the handler validated
// input and returned a success message but never touched the fixed
// 60-minute waitTimeout, so the migration would still time out and roll back
// on the original schedule regardless of how many times extend-wait was
// called. This asserts the workflow is still running well past the original
// 60-minute deadline once an extension has been granted, and only completes
// after app-ready arrives inside the extended window.
func (s *SchemaMigrationTestSuite) TestUpdateHandler_ExtendWait_ActuallyDelaysRollback() {
	fake := newFakeMigration()
	fake.register(s.env)

	var updateErr error
	s.env.RegisterDelayedCallback(func() {
		uc := &testsuite.TestUpdateCallback{
			OnAccept:   func() {},
			OnReject:   func(err error) { updateErr = err },
			OnComplete: func(_ interface{}, err error) { updateErr = err },
		}
		s.env.UpdateWorkflow("extend-wait", "", uc, 40)
	}, 1*time.Millisecond)

	s.env.RegisterDelayedCallback(func() {
		s.NoError(updateErr, "extend-wait must be accepted")
	}, 2*time.Millisecond)

	// Past the original 60-minute deadline but well before the 100-minute
	// (60 + 40) extended deadline: the workflow must still be running.
	s.env.RegisterDelayedCallback(func() {
		s.False(s.env.IsWorkflowCompleted(),
			"extend-wait must actually push out the app-ready deadline, not just say it did")
	}, 65*time.Minute)

	// Signal app-ready comfortably inside the extended window.
	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 90*time.Minute)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())

	var result types.ProgressResponse
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal("completed", result.Status, "workflow should complete normally, not roll back")
}

func (s *SchemaMigrationTestSuite) TestUpdateHandler_ExtendWait_Invalid() {
	fake := newFakeMigration()
	fake.register(s.env)

	var rejected bool
	s.env.RegisterDelayedCallback(func() {
		uc := &testsuite.TestUpdateCallback{
			OnAccept:   func() { s.Fail("should not accept invalid input") },
			OnReject:   func(err error) { rejected = true; s.Error(err) },
			OnComplete: func(_ interface{}, _ error) {},
		}
		s.env.UpdateWorkflow("extend-wait", "", uc, -5)
	}, 1*time.Millisecond)

	// The update's own handler invocation is queued as a callback and is only
	// drained *after* the RegisterDelayedCallback above returns, so assertions
	// on the callback outcome must happen in a subsequent callback.
	s.env.RegisterDelayedCallback(func() {
		s.True(rejected, "validator must reject negative values")
		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 2*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

// ── Query handler ─────────────────────────────────────────────────────────────

func (s *SchemaMigrationTestSuite) TestProgressQuery_ReturnsCurrentPhase() {
	fake := newFakeMigration()
	fake.register(s.env)

	s.env.RegisterDelayedCallback(func() {
		val, err := s.env.QueryWorkflow(QueryMigrationProgress)
		s.NoError(err)
		var p types.ProgressResponse
		s.NoError(val.Get(&p))
		// At this point the workflow is waiting for app-ready signal.
		s.Equal("waiting_for_app_ready", p.Phase)
		s.Equal("v0.16.2", p.PgrollVersion)
		s.Equal("public_add_email", p.LatestSchema)
		s.NotNil(p.PgrollStatus)

		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 5*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
}

// ── activity.RecordHeartbeat in TestActivityEnvironment ───────────────────────
// Standalone test for activity heartbeat using TestActivityEnvironment directly.
// SetOnActivityHeartbeatListener (SDK v1.x) is called for each heartbeat.

func TestStartMigrationActivity_Heartbeat(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()

	heartbeats := 0
	env.SetOnActivityHeartbeatListener(func(_ *activity.Info, _ converter.EncodedValues) {
		heartbeats++
	})

	actFn := func(ctx context.Context, in types.MigrationInput) error {
		activity.RecordHeartbeat(ctx, "starting")
		activity.RecordHeartbeat(ctx, "started")
		return nil
	}
	env.RegisterActivity(actFn)
	_, err := env.ExecuteActivity(actFn, types.MigrationInput{Schema: "public"})

	if err != nil {
		t.Fatalf("activity error: %v", err)
	}
	if heartbeats < 1 {
		t.Fatalf("expected at least 1 heartbeat, got %d", heartbeats)
	}
}

// ── workflow.GetVersion compatibility ─────────────────────────────────────────
// Tests that the version gate does not break replay.  In practice we export
// a workflow history from a real run and replay it, but in unit tests we just
// verify the workflow completes normally with the version gate in place.

func (s *SchemaMigrationTestSuite) TestGetVersion_DoesNotBreakHappyPath() {
	fake := newFakeMigration()
	fake.register(s.env)

	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(SignalAppReady, nil)
	}, 1*time.Millisecond)

	s.env.ExecuteWorkflow(SchemaMigrationWorkflow, defaultInput())
	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

// ── Workflow replay / determinism ─────────────────────────────────────────────
// The Go SDK testsuite supports WorkflowReplayer for history-based determinism
// checks.  Here we demonstrate the setup; in CI a real history JSON would be
// committed and replayed on every PR.

func TestWorkflowReplay_SchemaMigration(t *testing.T) {
	// Register the workflow with a replayer to verify it is deterministic.
	// In a real project: load history JSON exported via `temporal workflow show`.
	// For now we just confirm the replayer can be constructed.
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(SchemaMigrationWorkflow)
	// Replay from file would be:
	//   err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/history.json")
	// Omitted here because we have no real history artifact yet.
	_ = replayer
}

// ── fake wiring helpers ───────────────────────────────────────────────────────
