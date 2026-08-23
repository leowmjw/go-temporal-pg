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

type StreamMode string

const (
	StreamModeReplication            StreamMode = "replication"
	StreamModeSnapshot               StreamMode = "snapshot"
	StreamModeSnapshotAndReplication StreamMode = "snapshot_and_replication"
)

type SnapshotMode string

const (
	SnapshotModeFull   SnapshotMode = "full"
	SnapshotModeSchema SnapshotMode = "schema"
	SnapshotModeData   SnapshotMode = "data"
)

type SchemaChangePolicy string

const (
	SchemaChangePolicyAllow           SchemaChangePolicy = "allow"
	SchemaChangePolicyBlock           SchemaChangePolicy = "block"
	SchemaChangePolicyAlertOnly       SchemaChangePolicy = "alert_only"
	SchemaChangePolicyRequireApproval SchemaChangePolicy = "require_approval"
)

type StreamSourceType string

const (
	StreamSourceTypePostgres StreamSourceType = "postgres"
	StreamSourceTypeKafka    StreamSourceType = "kafka"
)

type StreamTargetType string

const (
	StreamTargetTypePostgres      StreamTargetType = "postgres"
	StreamTargetTypeKafka         StreamTargetType = "kafka"
	StreamTargetTypeElasticsearch StreamTargetType = "elasticsearch"
	StreamTargetTypeOpenSearch    StreamTargetType = "opensearch"
	StreamTargetTypeWebhook       StreamTargetType = "webhook"
	StreamTargetTypeStdout        StreamTargetType = "stdout"
)

type GuardrailAction string

const (
	GuardrailActionAlert GuardrailAction = "alert"
	GuardrailActionStop  GuardrailAction = "stop"
)

// AnonymizationRule defines how to transform a column for PII removal.
type AnonymizationRule struct {
	Table       string `json:"table"`
	Column      string `json:"column"`
	Transformer string `json:"transformer"` // e.g. "email", "name", "phone", "null"
}

type StreamFilters struct {
	IncludedSchemas       []string `json:"included_schemas,omitempty"`
	ExcludedSchemas       []string `json:"excluded_schemas,omitempty"`
	IncludedTables        []string `json:"included_tables,omitempty"`
	ExcludedTables        []string `json:"excluded_tables,omitempty"`
	SchemaOnlyTables      []string `json:"schema_only_tables,omitempty"`
	IncludeDDLObjectTypes []string `json:"include_ddl_object_types,omitempty"`
	ExcludeDDLObjectTypes []string `json:"exclude_ddl_object_types,omitempty"`
}

type StreamSnapshotConfig struct {
	Mode                SnapshotMode `json:"mode,omitempty"`
	ResetTarget         bool         `json:"reset_target,omitempty"`
	Repeatable          bool         `json:"repeatable,omitempty"`
	SnapshotWorkers     int          `json:"snapshot_workers,omitempty"`
	SchemaWorkers       int          `json:"schema_workers,omitempty"`
	TableWorkers        int          `json:"table_workers,omitempty"`
	BatchBytes          int64        `json:"batch_bytes,omitempty"`
	MaxConnections      int          `json:"max_connections,omitempty"`
	DisableProgress     bool         `json:"disable_progress_tracking,omitempty"`
	DumpFile            string       `json:"dump_file,omitempty"`
	CleanTargetDatabase bool         `json:"clean_target_database,omitempty"`
	CreateTargetDB      bool         `json:"create_target_database,omitempty"`
}

type StreamRetryPolicy struct {
	DisableRetries     bool `json:"disable_retries,omitempty"`
	ConstantMaxRetries int  `json:"constant_max_retries,omitempty"`
	ConstantIntervalMS int  `json:"constant_interval_ms,omitempty"`
	InitialIntervalMS  int  `json:"initial_interval_ms,omitempty"`
	MaxIntervalMS      int  `json:"max_interval_ms,omitempty"`
}

type StreamGuardrails struct {
	MaxLagBytes                int64           `json:"max_lag_bytes,omitempty"`
	MaxLagDuration             time.Duration   `json:"max_lag_duration,omitempty"`
	MaxInactiveSlotDuration    time.Duration   `json:"max_inactive_slot_duration,omitempty"`
	MaxConsecutivePollFailures int             `json:"max_consecutive_poll_failures,omitempty"`
	OnViolation                GuardrailAction `json:"on_violation,omitempty"`
}

type StreamPreflightConfig struct {
	ExpectedVersion string `json:"expected_version,omitempty"`
	StrictVersion   bool   `json:"strict_version,omitempty"`
}

type StreamRestartPolicy struct {
	MaxRestarts int           `json:"max_restarts,omitempty"`
	Window      time.Duration `json:"window,omitempty"`
}

type PostgresTargetConfig struct {
	URL                   string            `json:"url,omitempty"`
	MaxConnections        int               `json:"max_connections,omitempty"`
	DisableTriggers       bool              `json:"disable_triggers,omitempty"`
	OnConflictAction      string            `json:"on_conflict_action,omitempty"`
	StrictMode            bool              `json:"strict_mode,omitempty"`
	IgnoreDDL             bool              `json:"ignore_ddl,omitempty"`
	BatchTimeoutMS        int               `json:"batch_timeout_ms,omitempty"`
	BatchSize             int               `json:"batch_size,omitempty"`
	BatchMaxBytes         int64             `json:"batch_max_bytes,omitempty"`
	BatchMaxQueueBytes    int64             `json:"batch_max_queue_bytes,omitempty"`
	IgnoreSendErrors      bool              `json:"ignore_send_errors,omitempty"`
	AutoTune              bool              `json:"auto_tune,omitempty"`
	AutoTuneMinBatchBytes int64             `json:"auto_tune_min_batch_bytes,omitempty"`
	AutoTuneMaxBatchBytes int64             `json:"auto_tune_max_batch_bytes,omitempty"`
	CopyWorkers           int               `json:"copy_workers,omitempty"`
	BulkIngest            bool              `json:"bulk_ingest,omitempty"`
	RetryPolicy           StreamRetryPolicy `json:"retry_policy,omitempty"`
}

type KafkaTargetConfig struct {
	Servers           []string `json:"servers,omitempty"`
	TopicName         string   `json:"topic_name,omitempty"`
	Partitions        int      `json:"partitions,omitempty"`
	ReplicationFactor int      `json:"replication_factor,omitempty"`
	PartitionKey      string   `json:"partition_key,omitempty"`
	AutoCreate        bool     `json:"auto_create,omitempty"`
}

type SearchTargetConfig struct {
	Engine     StreamTargetType `json:"engine,omitempty"`
	URL        string           `json:"url,omitempty"`
	Index      string           `json:"index,omitempty"`
	HashDocIDs bool             `json:"hash_doc_ids,omitempty"`
}

type WebhookTargetConfig struct {
	StoreURL      string `json:"store_url,omitempty"`
	ServerAddress string `json:"server_address,omitempty"`
	ReadTimeoutS  int    `json:"read_timeout_s,omitempty"`
	WriteTimeoutS int    `json:"write_timeout_s,omitempty"`
	WorkerCount   int    `json:"worker_count,omitempty"`
	ClientTimeout int    `json:"client_timeout_ms,omitempty"`
}

type StreamTargetConfig struct {
	Type          StreamTargetType      `json:"type,omitempty"`
	Postgres      *PostgresTargetConfig `json:"postgres,omitempty"`
	Kafka         *KafkaTargetConfig    `json:"kafka,omitempty"`
	Elasticsearch *SearchTargetConfig   `json:"elasticsearch,omitempty"`
	OpenSearch    *SearchTargetConfig   `json:"opensearch,omitempty"`
	Webhook       *WebhookTargetConfig  `json:"webhook,omitempty"`
	Stdout        bool                  `json:"stdout,omitempty"`
}

// StreamConfig carries all parameters for a pgstream CDC pipeline.
type StreamConfig struct {
	SourceDSN           string                `json:"source_dsn"`
	TargetDSN           string                `json:"target_dsn"`
	ReplicationSlotName string                `json:"replication_slot_name"`
	AnonymizationRules  []AnonymizationRule   `json:"anonymization_rules,omitempty"`
	StreamID            string                `json:"stream_id"`
	Mode                StreamMode            `json:"mode,omitempty"`
	SourceType          StreamSourceType      `json:"source_type,omitempty"`
	Target              StreamTargetConfig    `json:"target,omitempty"`
	Filters             StreamFilters         `json:"filters,omitempty"`
	Snapshot            StreamSnapshotConfig  `json:"snapshot,omitempty"`
	SchemaChangePolicy  SchemaChangePolicy    `json:"schema_change_policy,omitempty"`
	Guardrails          StreamGuardrails      `json:"guardrails,omitempty"`
	Preflight           StreamPreflightConfig `json:"preflight,omitempty"`
	RestartPolicy       StreamRestartPolicy   `json:"restart_policy,omitempty"`
	RestartCount        int                   `json:"restart_count,omitempty"`
	LastRestartAt       time.Time             `json:"last_restart_at,omitempty"`
	RestartReason       string                `json:"restart_reason,omitempty"`
	RestartInitiator    string                `json:"restart_initiator,omitempty"`
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

type SnapshotStatus struct {
	Phase           string    `json:"phase,omitempty"`
	TablesCompleted int       `json:"tables_completed,omitempty"`
	RowsCopied      int64     `json:"rows_copied,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
	FailedAt        time.Time `json:"failed_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
}

type RestartMetadata struct {
	Count      int       `json:"count"`
	Reason     string    `json:"reason,omitempty"`
	Initiator  string    `json:"initiator,omitempty"`
	LastAt     time.Time `json:"last_at,omitempty"`
	Rejected   bool      `json:"rejected,omitempty"`
	RejectNote string    `json:"reject_note,omitempty"`
}

type PreflightStatus struct {
	PgstreamVersion        string    `json:"pgstream_version,omitempty"`
	VersionMatchesExpected bool      `json:"version_matches_expected,omitempty"`
	SourceReachable        bool      `json:"source_reachable,omitempty"`
	TargetReachable        bool      `json:"target_reachable,omitempty"`
	MetadataExists         bool      `json:"metadata_exists,omitempty"`
	SlotExists             bool      `json:"slot_exists,omitempty"`
	SlotActive             bool      `json:"slot_active,omitempty"`
	SlotPlugin             string    `json:"slot_plugin,omitempty"`
	SuggestedInitMode      string    `json:"suggested_init_mode,omitempty"`
	CheckedAt              time.Time `json:"checked_at,omitempty"`
	Warning                string    `json:"warning,omitempty"`
}

type StreamHealthResponse struct {
	Phase                   string             `json:"phase,omitempty"`
	Status                  string             `json:"status,omitempty"`
	Mode                    StreamMode         `json:"mode,omitempty"`
	TargetType              StreamTargetType   `json:"target_type,omitempty"`
	SchemaChangePolicy      SchemaChangePolicy `json:"schema_change_policy,omitempty"`
	ReplicationSlotName     string             `json:"replication_slot_name,omitempty"`
	ReplicationSlotActive   bool               `json:"replication_slot_active,omitempty"`
	CurrentLSN              string             `json:"current_lsn,omitempty"`
	LastEventAt             time.Time          `json:"last_event_at,omitempty"`
	LagBytes                int64              `json:"lag_bytes,omitempty"`
	LastChecked             time.Time          `json:"last_checked,omitempty"`
	SourceReachable         bool               `json:"source_reachable,omitempty"`
	TargetReachable         bool               `json:"target_reachable,omitempty"`
	ConsecutivePollFailures int                `json:"consecutive_poll_failures,omitempty"`
	LastError               string             `json:"last_error,omitempty"`
	Snapshot                SnapshotStatus     `json:"snapshot,omitempty"`
	Restart                 RestartMetadata    `json:"restart,omitempty"`
	Preflight               PreflightStatus    `json:"preflight,omitempty"`
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
