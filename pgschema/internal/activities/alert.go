// Package activities - human escalation / alerting activity.
//
// AlertActivities sends an alert to a configurable webhook endpoint when a
// workflow hits an unrecoverable failure.  The PageFn field can be replaced
// with any anonymous function in tests — no HTTP mocks needed.
package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// AlertActivities holds the human-escalation Temporal activity.
type AlertActivities struct {
	PageFn     func(ctx context.Context, msg types.AlertMessage) error
	httpClient *http.Client
	log        *slog.Logger
}

// NewAlertActivities returns an AlertActivities that POSTs to the webhook URL
// in the AlertMessage.
func NewAlertActivities(log *slog.Logger) *AlertActivities {
	a := &AlertActivities{
		httpClient: &http.Client{},
		log:        log,
	}
	a.PageFn = a.defaultPage
	return a
}
// logger returns the struct's logger, falling back to slog.Default() if nil.
func (a *AlertActivities) logger() *slog.Logger {
	if a.log == nil {
		return slog.Default()
	}
	return a.log
}


// Page sends an alert to the configured webhook.  The workflow calls this
// whenever a non-retryable failure occurs, so a human operator can investigate.
func (a *AlertActivities) Page(ctx context.Context, msg types.AlertMessage) error {
	a.logger().InfoContext(ctx, "paging operator",
		slog.String("workflow_id", msg.WorkflowID),
		slog.String("severity", msg.Severity),
		slog.String("title", msg.Title))

	if err := a.PageFn(ctx, msg); err != nil {
		return fmt.Errorf("alert page failed: %w", err)
	}
	return nil
}

// ─── Default (HTTP webhook) implementation ────────────────────────────────────

func (a *AlertActivities) defaultPage(ctx context.Context, msg types.AlertMessage) error {
	if msg.WebhookURL == "" {
		a.logger().WarnContext(ctx, "no webhook URL configured; alert dropped",
			slog.String("title", msg.Title))
		return nil
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, msg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
