// Package types holds all shared input/output structs used across activities
// and workflows in the pgschema module.
package types

import "time"

// ─── Migration (pgroll) ──────────────────────────────────────────────────────

// MigrationInput carries parameters for a pgroll zero-downtime migration.
type MigrationInput struct {
	DSN                       string            `json:"dsn"`
	MigrationJSON             string            `json:"migration_json"` // pgroll migration file contents
	Schema                    string            `json:"schema"`
	Tags                      map[string]string `json:"tags,omitempty"`
	AllowInitialize           bool              `json:"allow_initialize,omitempty"`
	ExpectedPgrollVersion     string            `json:"expected_pgroll_version,omitempty"`
	RequireExactPgrollVersion bool              `json:"require_exact_pgroll_version,omitempty"`
	Policy                    MigrationPolicy   `json:"policy,omitempty"`
}

// MigrationPolicy controls migration preflight and risk gating behavior.
type MigrationPolicy struct {
	BlockRawSQL            bool     `json:"block_raw_sql,omitempty"`
	BlockRenames           bool     `json:"block_renames,omitempty"`
	BlockConstraints       bool     `json:"block_constraints,omitempty"`
	BlockDefaults          bool     `json:"block_defaults,omitempty"`
	BlockDestructive       bool     `json:"block_destructive,omitempty"`
	RequireApprovalForRisk string   `json:"require_approval_for_risk,omitempty"`
	Approved               bool     `json:"approved,omitempty"`
	ProtectedSchemas       []string `json:"protected_schemas,omitempty"`
	ProtectedTables        []string `json:"protected_tables,omitempty"`
}

// MigrationStatus reflects the current state of a pgroll migration.
type MigrationStatus struct {
	Name      string    `json:"name,omitempty"`
	Version   string    `json:"version,omitempty"`
	Status    string    `json:"status"` // "No migrations" | "In progress" | "Complete"
	Schema    string    `json:"schema"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

// EffectiveVersion returns the populated version field regardless of which JSON
// shape pgroll emitted.
func (s MigrationStatus) EffectiveVersion() string {
	if s.Version != "" {
		return s.Version
	}
	return s.Name
}

// PgrollReadiness describes whether pgroll metadata is ready for use.
type PgrollReadiness struct {
	Initialized     bool   `json:"initialized"`
	AutoInitialized bool   `json:"auto_initialized,omitempty"`
	Message         string `json:"message,omitempty"`
}

// MigrationRiskFinding captures one risky migration operation.
type MigrationRiskFinding struct {
	Operation string `json:"operation"`
	Category  string `json:"category"`
	Risk      string `json:"risk"`
	Target    string `json:"target,omitempty"`
	Reason    string `json:"reason"`
}

// MigrationRiskReport summarizes the migration risk profile.
type MigrationRiskReport struct {
	MigrationName    string                 `json:"migration_name,omitempty"`
	OverallRisk      string                 `json:"overall_risk"`
	Summary          string                 `json:"summary,omitempty"`
	Findings         []MigrationRiskFinding `json:"findings,omitempty"`
	RequiresApproval bool                   `json:"requires_approval,omitempty"`
	Blocked          bool                   `json:"blocked,omitempty"`
}

// ReconcileInput asks the activity layer to compare workflow intent with
// current pgroll state before a mutating step.
type ReconcileInput struct {
	Migration MigrationInput `json:"migration"`
	Phase     string         `json:"phase"`
}

// ReconciliationResult describes the action that should follow a state check.
type ReconciliationResult struct {
	Action string           `json:"action"`
	Reason string           `json:"reason,omitempty"`
	Status *MigrationStatus `json:"status,omitempty"`
}

// BaselineInput carries parameters for a pgroll brownfield baseline run.
type BaselineInput struct {
	DSN                       string            `json:"dsn"`
	Schema                    string            `json:"schema"`
	Version                   string            `json:"version"`
	Directory                 string            `json:"directory"`
	Format                    string            `json:"format,omitempty"`
	Operator                  string            `json:"operator,omitempty"`
	Tags                      map[string]string `json:"tags,omitempty"`
	AllowInitialize           bool              `json:"allow_initialize,omitempty"`
	ExpectedPgrollVersion     string            `json:"expected_pgroll_version,omitempty"`
	RequireExactPgrollVersion bool              `json:"require_exact_pgroll_version,omitempty"`
}

// BaselineResult captures the outcome of a baseline run.
type BaselineResult struct {
	Version       string    `json:"version"`
	Directory     string    `json:"directory"`
	Schema        string    `json:"schema"`
	Operator      string    `json:"operator,omitempty"`
	Status        string    `json:"status"`
	PgrollVersion string    `json:"pgroll_version,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
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
	Phase                string               `json:"phase"`
	Status               string               `json:"status"` // "running" | "completed" | "failed" | "rolled_back"
	Percent              int                  `json:"percent"`
	Message              string               `json:"message,omitempty"`
	LastUpdated          time.Time            `json:"last_updated"`
	PgrollVersion        string               `json:"pgroll_version,omitempty"`
	LatestSchema         string               `json:"latest_schema,omitempty"`
	PgrollStatus         *MigrationStatus     `json:"pgroll_status,omitempty"`
	RiskReport           *MigrationRiskReport `json:"risk_report,omitempty"`
	ReconciliationAction string               `json:"reconciliation_action,omitempty"`
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
