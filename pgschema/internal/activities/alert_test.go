package activities

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// ─────────────────────────────────────────────────────────────────────────────
// Why these test scenarios?
//
//  1. Page_Success — confirms the PageFn is called with the full AlertMessage.
//     If we don't assert the field values, a bug that silently drops the
//     WorkflowID or severity would go undetected.
//
//  2. Page_NoWebhook_Drops — when WebhookURL is empty the activity must succeed
//     (not error).  Alerting misconfiguration should NOT crash the workflow
//     outright; the operator can fix the config and resend.
//
//  3. Page_Failure_ReturnsError — webhook POST failures are real errors that
//     Temporal should retry (with the configured RetryPolicy).  Confirmed that
//     the error string is surfaced, not swallowed.
//
//  4. Page_CriticalSeverity — "critical" messages must be distinguishable from
//     "warning" so PagerDuty/Slack handlers can route them differently.  We
//     verify the AlertMessage severity field reaches the fn unchanged.
// ─────────────────────────────────────────────────────────────────────────────

func TestPage_Success(t *testing.T) {
	var received types.AlertMessage
	a := &AlertActivities{
		log: newTestLogger(),
		PageFn: func(_ context.Context, msg types.AlertMessage) error {
			received = msg
			return nil
		},
	}

	env := newActEnv(t)
	env.RegisterActivity(a.Page)
	msg := types.AlertMessage{
		WorkflowID: "wf-123",
		RunID:      "run-456",
		Severity:   "warning",
		Title:      "Migration stalled",
		Detail:     "pgroll start timed out after 15 min",
	}
	_, err := env.ExecuteActivity(a.Page, msg)
	require.NoError(t, err)
	assert.Equal(t, msg.WorkflowID, received.WorkflowID)
	assert.Equal(t, msg.Title, received.Title)
	assert.Equal(t, "warning", received.Severity)
}

func TestPage_NoWebhook_Drops(t *testing.T) {
	// PageFn should NOT be called when WebhookURL is empty in the default impl.
	// Here we confirm the activity returns nil even with an empty URL.
	a := &AlertActivities{
		log: newTestLogger(),
		PageFn: func(_ context.Context, _ types.AlertMessage) error {
			return nil // default noop when no URL
		},
	}

	env := newActEnv(t)
	env.RegisterActivity(a.Page)
	_, err := env.ExecuteActivity(a.Page, types.AlertMessage{
		WorkflowID: "wf-1",
		WebhookURL: "", // deliberately empty
		Title:      "test",
	})
	require.NoError(t, err)
}

func TestPage_Failure_ReturnsError(t *testing.T) {
	a := &AlertActivities{
		log: newTestLogger(),
		PageFn: func(_ context.Context, _ types.AlertMessage) error {
			return errors.New("webhook returned HTTP 503")
		},
	}

	env := newActEnv(t)
	env.RegisterActivity(a.Page)
	_, err := env.ExecuteActivity(a.Page, types.AlertMessage{
		WorkflowID: "wf-2", Title: "test",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alert page failed")
}

func TestPage_CriticalSeverity(t *testing.T) {
	var gotSeverity string
	a := &AlertActivities{
		log: newTestLogger(),
		PageFn: func(_ context.Context, msg types.AlertMessage) error {
			gotSeverity = msg.Severity
			return nil
		},
	}

	env := newActEnv(t)
	env.RegisterActivity(a.Page)
	_, err := env.ExecuteActivity(a.Page, types.AlertMessage{
		WorkflowID: "wf-3",
		Severity:   "critical",
		Title:      "CDC stream died",
	})
	require.NoError(t, err)
	assert.Equal(t, "critical", gotSeverity)
}
