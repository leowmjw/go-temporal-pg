// Package types holds all shared input/output structs used across activities
// and workflows in the pgschema module.
package types

import "time"

// ─── Migration (pgroll) ──────────────────────────────────────────────────────

// MigrationInput carries parameters for a pgroll zero-downtime migration.
type MigrationInput struct {
	DSN           string            `json:"dsn"`
	MigrationJSON string            `json:"migration_json"` // pgroll migration file contents
	Schema        string            `json:"schema"`
	Tags          map[string]string `json:"tags,omitempty"`
}

// MigrationStatus reflects the current state of a pgroll migration.
type MigrationStatus struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"` // "In Progress" | "Complete" | "Rolled Back"
	Schema    string    `json:"schema"`
	StartedAt time.Time `json:"started_at"`
}

// ─── CDC / Streaming (pgstream) ──────────────────────────────────────────────

// AnonymizationRule defines how to transform a column for PII removal.
type AnonymizationRule struct {
	Table       string `json:"table"`
	Column      string `json:"column"`
	Transformer string `json:"transformer"` // e.g. "email", "name", "phone", "null"
}

// StreamConfig carries all parameters for a pgstream CDC pipeline.
type StreamConfig struct {
	SourceDSN           string              `json:"source_dsn"`
	TargetDSN           string              `json:"target_dsn"`
	ReplicationSlotName string              `json:"replication_slot_name"`
	AnonymizationRules  []AnonymizationRule `json:"anonymization_rules,omitempty"`
	StreamID            string              `json:"stream_id"`
	// MaxIterations, when > 0, causes the workflow to ContinueAsNew after this
	// many heartbeat cycles to prevent history from growing unbounded.
	MaxIterations int `json:"max_iterations,omitempty"`
}

// AnonymizationInput wraps a target DSN and a set of anonymization rules
// for the preview-clone anonymization activity.
type AnonymizationInput struct {
	TargetDSN string              `json:"target_dsn"`
	Rules     []AnonymizationRule `json:"rules"`
}

// ─── Preview Clone ────────────────────────────────────────────────────────────

// PreviewCloneInput carries parameters for creating a copy-on-write preview DB.
type PreviewCloneInput struct {
	SourceDSN          string              `json:"source_dsn"`
	PreviewID          string              `json:"preview_id"`
	MigrationJSON      string              `json:"migration_json,omitempty"`
	AnonymizationRules []AnonymizationRule `json:"anonymization_rules,omitempty"`
	TTL                time.Duration       `json:"ttl"` // How long the preview lives before auto-cleanup
}

// PreviewEndpoint holds the connection details and expiry for a preview clone.
type PreviewEndpoint struct {
	DSN       string    `json:"dsn"`
	Schema    string    `json:"schema"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ─── Alerting ─────────────────────────────────────────────────────────────────

// AlertMessage is sent to the human operator when escalation is needed.
type AlertMessage struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	Severity   string `json:"severity"` // "warning" | "critical"
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	WebhookURL string `json:"webhook_url,omitempty"` // optional override
}

// ─── Workflow progress / queries ──────────────────────────────────────────────

// ProgressResponse is returned by workflow progress queries.
type ProgressResponse struct {
	Phase       string    `json:"phase"`
	Status      string    `json:"status"` // "running" | "completed" | "failed" | "rolled_back"
	Percent     int       `json:"percent"`
	Message     string    `json:"message,omitempty"`
	LastUpdated time.Time `json:"last_updated"`
}

// LagResponse is returned by the CDC lag query.
type LagResponse struct {
	LagBytes    int64     `json:"lag_bytes"`
	LastChecked time.Time `json:"last_checked"`
}

// ─── Typed errors ─────────────────────────────────────────────────────────────

// MigrationError wraps a migration-specific error with the phase it occurred
// in.  Use errors.AsType[*MigrationError] (Go 1.26+) to unpack it.
type MigrationError struct {
	Phase   string
	Wrapped error
}

func (e *MigrationError) Error() string {
	return "migration error in phase " + e.Phase + ": " + e.Wrapped.Error()
}

func (e *MigrationError) Unwrap() error { return e.Wrapped }

// StreamError wraps a streaming-specific error with the stream ID.
// Use errors.AsType[*StreamError] (Go 1.26+) to unpack it.
type StreamError struct {
	StreamID string
	Wrapped  error
}

func (e *StreamError) Error() string {
	return "stream error [" + e.StreamID + "]: " + e.Wrapped.Error()
}

func (e *StreamError) Unwrap() error { return e.Wrapped }

// PreviewError wraps a preview-clone error with the preview ID.
type PreviewError struct {
	PreviewID string
	Wrapped   error
}

func (e *PreviewError) Error() string {
	return "preview error [" + e.PreviewID + "]: " + e.Wrapped.Error()
}

func (e *PreviewError) Unwrap() error { return e.Wrapped }
