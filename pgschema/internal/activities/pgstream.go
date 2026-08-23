// Package activities - pgstream CDC / anonymization activities.
//
// PgstreamActivities wraps the pgstream binary for Temporal activities.
// All external calls are controlled by function fields for easy test injection.
package activities

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"gopkg.in/yaml.v3"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

const defaultExpectedPgstreamVersion = "v1.4.1"

var allowedTransformers = map[string]struct{}{
	"email":               {},
	"name":                {},
	"phone":               {},
	"null":                {},
	"mask":                {},
	"uuid":                {},
	"noop":                {},
	"greenmask_email":     {},
	"greenmask_name":      {},
	"greenmask_phone":     {},
	"greenmask_null":      {},
	"greenmask_uuid":      {},
	"greenmask_company":   {},
	"greenmask_firstname": {},
	"greenmask_lastname":  {},
}

// PgstreamActivities holds CDC-related Temporal activities backed by pgstream.
type PgstreamActivities struct {
	baseActivities
	InitFn          func(ctx context.Context, cfg types.StreamConfig) error
	RunFn           func(ctx context.Context, cfg types.StreamConfig) error
	StopFn          func(ctx context.Context, cfg types.StreamConfig) error
	GetLagFn        func(ctx context.Context, cfg types.StreamConfig) (int64, error)
	GetHealthFn     func(ctx context.Context, cfg types.StreamConfig) (*types.StreamHealthResponse, error)
	PreflightFn     func(ctx context.Context, cfg types.StreamConfig) (*types.PreflightStatus, error)
	ValidateRulesFn func(ctx context.Context, cfg types.StreamConfig, rules []types.AnonymizationRule) error
	// PollLagFn optionally overrides the entire PollLag loop for testing.
	// When nil, the default ticker-based implementation is used.
	PollLagFn func(ctx context.Context, cfg types.StreamConfig, interval time.Duration) (int64, error)
}

// NewPgstreamActivities returns a PgstreamActivities wired to the real
// `pgstream` binary. Any field can be replaced in tests.
func NewPgstreamActivities(log *slog.Logger) *PgstreamActivities {
	a := &PgstreamActivities{baseActivities: baseActivities{log: log}}
	a.InitFn = a.defaultInit
	a.RunFn = a.defaultRun
	a.StopFn = a.defaultStop
	a.GetLagFn = a.defaultGetLag
	a.GetHealthFn = a.defaultGetHealth
	a.PreflightFn = a.defaultPreflight
	a.ValidateRulesFn = a.defaultValidateRules
	return a
}

// CheckPreflight validates the local pgstream installation and source/target
// connectivity before the workflow attempts init/run.
func (a *PgstreamActivities) CheckPreflight(ctx context.Context, cfg types.StreamConfig) (*types.PreflightStatus, error) {
	cfg = normalizeStreamConfig(cfg)
	end := a.startTrace(ctx, "pgstream.preflight", slog.String("stream_id", cfg.StreamID))
	status, err := a.PreflightFn(ctx, cfg)
	end(err)
	if err != nil {
		return nil, &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return status, nil
}

// InitPgstream initialises the pgstream metadata schema and replication slot
// in the source Postgres database. Idempotent — safe to retry.
func (a *PgstreamActivities) InitPgstream(ctx context.Context, cfg types.StreamConfig) error {
	cfg = normalizeStreamConfig(cfg)
	end := a.startTrace(ctx, "pgstream.init",
		slog.String("stream_id", cfg.StreamID),
		slog.String("slot", cfg.ReplicationSlotName),
		slog.String("source", redactDSN(cfg.SourceDSN)))
	err := a.InitFn(ctx, cfg)
	end(err)
	if err != nil {
		return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return nil
}

// RunStream is a long-running Temporal activity that drives the pgstream
// replication loop. It emits heartbeats every 30s so Temporal can detect
// stuck workers. The activity exits cleanly when ctx is cancelled.
func (a *PgstreamActivities) RunStream(ctx context.Context, cfg types.StreamConfig) error {
	cfg = normalizeStreamConfig(cfg)
	end := a.startTrace(ctx, "pgstream.run",
		slog.String("stream_id", cfg.StreamID),
		slog.String("mode", string(cfg.Mode)),
		slog.String("target_type", string(cfg.Target.Type)))
	safeHeartbeat(ctx, "stream_starting")

	resultCh := make(chan error, 1)
	go func() { resultCh <- a.RunFn(ctx, cfg) }()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-resultCh:
			end(err)
			if err != nil {
				return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
			}
			return nil
		case <-ticker.C:
			safeHeartbeat(ctx, "stream_running")
		}
	}
}

// PollLag polls replication lag on a fixed interval until ctx is cancelled.
// Returns the last observed lag in bytes. Emits heartbeats so Temporal knows
// the activity is alive.
func (a *PgstreamActivities) PollLag(ctx context.Context, cfg types.StreamConfig, interval time.Duration) (int64, error) {
	cfg = normalizeStreamConfig(cfg)
	if a.PollLagFn != nil {
		return a.PollLagFn(ctx, cfg, interval)
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastLag int64
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			a.logger().InfoContext(ctx, "lag polling stopped",
				slog.String("flow", "end"),
				slog.String("op", "pgstream.poll_lag"),
				slog.String("stream_id", cfg.StreamID),
				slog.Int64("last_lag_bytes", lastLag))
			return lastLag, nil
		case <-ticker.C:
			lag, err := a.GetLagFn(ctx, cfg)
			if err != nil {
				consecutiveFailures++
				a.logger().WarnContext(ctx, "failed to get lag",
					slog.String("op", "pgstream.poll_lag"),
					slog.String("stream_id", cfg.StreamID),
					slog.Int("consecutive_failures", consecutiveFailures),
					slog.String("error", err.Error()))
				if max := cfg.Guardrails.MaxConsecutivePollFailures; max > 0 && consecutiveFailures >= max {
					return lastLag, fmt.Errorf("lag polling failed %d consecutive times: %w", consecutiveFailures, err)
				}
				safeHeartbeat(ctx, "lag_poll_error")
				continue
			}
			consecutiveFailures = 0
			lastLag = lag
			if max := cfg.Guardrails.MaxLagBytes; max > 0 && lag > max && cfg.Guardrails.OnViolation == types.GuardrailActionStop {
				return lastLag, fmt.Errorf("lag guardrail exceeded: lag_bytes=%d max_lag_bytes=%d", lag, max)
			}
			a.logger().InfoContext(ctx, "replication lag",
				slog.String("op", "pgstream.poll_lag"),
				slog.String("stream_id", cfg.StreamID),
				slog.Int64("lag_bytes", lag))
			safeHeartbeat(ctx, fmt.Sprintf("lag_bytes=%d", lastLag))
		}
	}
}

// StopStream gracefully signals pgstream to stop.
func (a *PgstreamActivities) StopStream(ctx context.Context, cfg types.StreamConfig) error {
	cfg = normalizeStreamConfig(cfg)
	end := a.startTrace(ctx, "pgstream.stop", slog.String("stream_id", cfg.StreamID))
	err := a.StopFn(ctx, cfg)
	end(err)
	if err != nil {
		return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return nil
}

// GetLag returns the current replication lag once.
func (a *PgstreamActivities) GetLag(ctx context.Context, cfg types.StreamConfig) (int64, error) {
	cfg = normalizeStreamConfig(cfg)
	end := a.startTrace(ctx, "pgstream.get_lag", slog.String("stream_id", cfg.StreamID))
	lag, err := a.GetLagFn(ctx, cfg)
	end(err)
	if err != nil {
		return 0, &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return lag, nil
}

// GetStreamHealth returns rich stream health information for workflow queries.
func (a *PgstreamActivities) GetStreamHealth(ctx context.Context, cfg types.StreamConfig) (*types.StreamHealthResponse, error) {
	cfg = normalizeStreamConfig(cfg)
	end := a.startTrace(ctx, "pgstream.health", slog.String("stream_id", cfg.StreamID))
	health, err := a.GetHealthFn(ctx, cfg)
	end(err)
	if err != nil {
		return nil, &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return health, nil
}

// ValidateAnonymizationRules validates a new rule set against the source
// schema and supported transformer names before a workflow restart.
func (a *PgstreamActivities) ValidateAnonymizationRules(ctx context.Context, cfg types.StreamConfig, rules []types.AnonymizationRule) error {
	cfg = normalizeStreamConfig(cfg)
	end := a.startTrace(ctx, "pgstream.validate_rules",
		slog.String("stream_id", cfg.StreamID),
		slog.Int("rule_count", len(rules)))
	err := a.ValidateRulesFn(ctx, cfg, rules)
	end(err)
	if err != nil {
		return &types.StreamError{StreamID: cfg.StreamID, Wrapped: err}
	}
	return nil
}

func (a *PgstreamActivities) defaultPreflight(ctx context.Context, cfg types.StreamConfig) (*types.PreflightStatus, error) {
	version, err := getPgstreamVersion(ctx, a.baseActivities)
	if err != nil {
		return nil, err
	}
	state, err := inspectPostgresState(ctx, cfg)
	if err != nil {
		return nil, err
	}
	status := &types.PreflightStatus{
		PgstreamVersion:        version,
		VersionMatchesExpected: true,
		SourceReachable:        state.SourceReachable,
		TargetReachable:        state.TargetReachable,
		MetadataExists:         state.MetadataExists,
		SlotExists:             state.SlotExists,
		SlotActive:             state.SlotActive,
		SlotPlugin:             state.SlotPlugin,
		SuggestedInitMode:      state.SuggestedInitMode(),
		CheckedAt:              time.Now().UTC(),
	}
	expected := cfg.Preflight.ExpectedVersion
	if expected == "" {
		expected = defaultExpectedPgstreamVersion
	}
	status.VersionMatchesExpected = versionMatches(version, expected)
	if !status.VersionMatchesExpected {
		status.Warning = fmt.Sprintf("expected pgstream %s, found %s", expected, version)
		if cfg.Preflight.StrictVersion {
			return nil, errors.New(status.Warning)
		}
		a.logger().WarnContext(ctx, "pgstream version mismatch",
			slog.String("expected", expected),
			slog.String("found", version))
	}
	return status, nil
}

func (a *PgstreamActivities) defaultInit(ctx context.Context, cfg types.StreamConfig) error {
	if cfg.Mode == types.StreamModeSnapshot {
		return nil
	}
	state, err := inspectPostgresState(ctx, cfg)
	if err != nil {
		return err
	}
	if state.SlotExists && state.SlotPlugin != "" && state.SlotPlugin != "wal2json" {
		return fmt.Errorf("replication slot %q already exists with unsupported plugin %q", cfg.ReplicationSlotName, state.SlotPlugin)
	}
	if state.MetadataExists && state.SlotExists {
		return nil
	}
	return withPgstreamConfigFile(cfg, func(path string) error {
		args := []string{"init", "--config", path}
		switch state.SuggestedInitMode() {
		case "migrations-only":
			args = append(args, "--migrations-only")
		case "slot-only":
			args = append(args, "--slot-only")
		}
		if needsInjector(cfg) {
			args = append(args, "--with-injector")
		}
		return a.runPgstream(ctx, args)
	})
}

func (a *PgstreamActivities) defaultRun(ctx context.Context, cfg types.StreamConfig) error {
	return withPgstreamConfigFile(cfg, func(path string) error {
		command := "run"
		if cfg.Mode == types.StreamModeSnapshot {
			command = "snapshot"
		}
		return a.runPgstreamHeartbeating(ctx, []string{command, "--config", path}, cfg.StreamID)
	})
}

func (a *PgstreamActivities) defaultStop(_ context.Context, _ types.StreamConfig) error {
	return nil
}

func (a *PgstreamActivities) defaultGetLag(ctx context.Context, cfg types.StreamConfig) (int64, error) {
	health, err := a.defaultGetHealth(ctx, cfg)
	if err != nil {
		return 0, err
	}
	return health.LagBytes, nil
}

func (a *PgstreamActivities) defaultGetHealth(ctx context.Context, cfg types.StreamConfig) (*types.StreamHealthResponse, error) {
	state, err := inspectPostgresState(ctx, cfg)
	if err != nil {
		return nil, err
	}

	health := &types.StreamHealthResponse{
		Mode:                  cfg.Mode,
		Status:                "running",
		TargetType:            cfg.Target.Type,
		SchemaChangePolicy:    cfg.SchemaChangePolicy,
		ReplicationSlotName:   cfg.ReplicationSlotName,
		ReplicationSlotActive: state.SlotActive,
		CurrentLSN:            state.CurrentLSN,
		LagBytes:              state.LagBytes,
		LastChecked:           time.Now().UTC(),
		SourceReachable:       state.SourceReachable,
		TargetReachable:       state.TargetReachable,
		Preflight: types.PreflightStatus{
			MetadataExists:    state.MetadataExists,
			SlotExists:        state.SlotExists,
			SlotActive:        state.SlotActive,
			SlotPlugin:        state.SlotPlugin,
			SuggestedInitMode: state.SuggestedInitMode(),
			CheckedAt:         time.Now().UTC(),
		},
	}

	switch cfg.Mode {
	case types.StreamModeSnapshot:
		health.Phase = "snapshot"
		health.Snapshot.Phase = "snapshot"
	case types.StreamModeSnapshotAndReplication:
		health.Phase = "snapshot_and_replication"
		health.Snapshot.Phase = "snapshot_and_replication"
	default:
		health.Phase = "replication"
	}

	if err := withPgstreamConfigFile(cfg, func(path string) error {
		out, err := a.runPgstreamOutput(ctx, []string{"status", "--config", path, "--json"})
		if err != nil {
			return err
		}
		status, err := parseStatusJSON(out)
		if err != nil {
			return err
		}
		mergeHealthFromStatus(health, status)
		return nil
	}); err != nil {
		health.LastError = err.Error()
	}
	return health, nil
}

func (a *PgstreamActivities) defaultValidateRules(ctx context.Context, cfg types.StreamConfig, rules []types.AnonymizationRule) error {
	if len(rules) == 0 {
		return errors.New("rules list must not be empty")
	}
	seen := map[string]struct{}{}
	for _, rule := range rules {
		schema, table := splitQualifiedTable(rule.Table)
		if table == "" {
			return errors.New("rule table must not be empty")
		}
		if rule.Column == "" {
			return fmt.Errorf("rule column must not be empty for %s.%s", schema, table)
		}
		key := schema + "." + table + "." + rule.Column
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate anonymization rule for %s", key)
		}
		seen[key] = struct{}{}
		if err := validateTransformerName(rule.Transformer); err != nil {
			return err
		}
	}

	db, err := sql.Open("postgres", cfg.SourceDSN)
	if err != nil {
		return fmt.Errorf("open source postgres: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping source postgres: %w", err)
	}

	for _, rule := range rules {
		schema, table := splitQualifiedTable(rule.Table)
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
			)
		`, schema, table, rule.Column).Scan(&exists); err != nil {
			return fmt.Errorf("validate source schema for %s.%s.%s: %w", schema, table, rule.Column, err)
		}
		if !exists {
			return fmt.Errorf("unknown source column %s.%s.%s", schema, table, rule.Column)
		}
	}
	return nil
}

// parseLagBytes extracts the replication lag (in bytes) from `pgstream status
// --json` output. Split out from defaultGetLag so it can be unit tested.
func parseLagBytes(out []byte) (int64, error) {
	status, err := parseStatusJSON(out)
	if err != nil {
		return 0, err
	}
	if lag, ok := lookupInt64(status, "lag_bytes"); ok {
		return lag, nil
	}
	return 0, nil
}

type postgresRuntimeState struct {
	SourceReachable bool
	TargetReachable bool
	MetadataExists  bool
	SlotExists      bool
	SlotActive      bool
	SlotPlugin      string
	CurrentLSN      string
	LagBytes        int64
}

func (s postgresRuntimeState) SuggestedInitMode() string {
	switch {
	case s.MetadataExists && s.SlotExists:
		return "skip"
	case s.MetadataExists:
		return "slot-only"
	case s.SlotExists:
		return "migrations-only"
	default:
		return "full"
	}
}

func normalizeStreamConfig(cfg types.StreamConfig) types.StreamConfig {
	if cfg.Mode == "" {
		cfg.Mode = types.StreamModeReplication
	}
	if cfg.SourceType == "" {
		cfg.SourceType = types.StreamSourceTypePostgres
	}
	if cfg.Target.Type == "" {
		cfg.Target.Type = types.StreamTargetTypePostgres
	}
	if cfg.Target.Type == types.StreamTargetTypePostgres {
		if cfg.Target.Postgres == nil {
			cfg.Target.Postgres = &types.PostgresTargetConfig{}
		}
		if cfg.Target.Postgres.URL == "" {
			cfg.Target.Postgres.URL = cfg.TargetDSN
		}
	}
	if cfg.ReplicationSlotName == "" {
		cfg.ReplicationSlotName = "pgstream_slot"
	}
	if cfg.SchemaChangePolicy == "" {
		cfg.SchemaChangePolicy = types.SchemaChangePolicyAllow
	}
	if cfg.Guardrails.OnViolation == "" {
		cfg.Guardrails.OnViolation = types.GuardrailActionAlert
	}
	if cfg.Preflight.ExpectedVersion == "" {
		cfg.Preflight.ExpectedVersion = defaultExpectedPgstreamVersion
	}
	return cfg
}

func getPgstreamVersion(ctx context.Context, base baseActivities) (string, error) {
	out, err := base.runPgstreamOutput(ctx, []string{"version"})
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", errors.New("pgstream version returned empty output")
	}
	return version, nil
}

func versionMatches(actual, expected string) bool {
	return actual == expected || strings.Contains(actual, expected)
}

func needsInjector(cfg types.StreamConfig) bool {
	return cfg.Target.Type == types.StreamTargetTypeElasticsearch || cfg.Target.Type == types.StreamTargetTypeOpenSearch
}

func inspectPostgresState(ctx context.Context, cfg types.StreamConfig) (postgresRuntimeState, error) {
	state := postgresRuntimeState{TargetReachable: true}
	db, err := sql.Open("postgres", cfg.SourceDSN)
	if err != nil {
		return state, fmt.Errorf("open source postgres: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return state, fmt.Errorf("ping source postgres: %w", err)
	}
	state.SourceReachable = true

	if err := db.QueryRowContext(ctx, `
		SELECT
			EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = 'pgstream'),
			EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name = $1),
			COALESCE((SELECT active FROM pg_replication_slots WHERE slot_name = $1), false),
			COALESCE((SELECT plugin FROM pg_replication_slots WHERE slot_name = $1), ''),
			COALESCE((SELECT restart_lsn::text FROM pg_replication_slots WHERE slot_name = $1), ''),
			COALESCE((SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::bigint FROM pg_replication_slots WHERE slot_name = $1), 0)
	`, cfg.ReplicationSlotName).Scan(
		&state.MetadataExists,
		&state.SlotExists,
		&state.SlotActive,
		&state.SlotPlugin,
		&state.CurrentLSN,
		&state.LagBytes,
	); err != nil {
		return state, fmt.Errorf("inspect pgstream source state: %w", err)
	}

	if cfg.Target.Type == types.StreamTargetTypePostgres && cfg.Target.Postgres != nil && cfg.Target.Postgres.URL != "" {
		targetDB, err := sql.Open("postgres", cfg.Target.Postgres.URL)
		if err != nil {
			return state, fmt.Errorf("open target postgres: %w", err)
		}
		defer targetDB.Close()
		if err := targetDB.PingContext(ctx); err != nil {
			state.TargetReachable = false
			return state, fmt.Errorf("ping target postgres: %w", err)
		}
		state.TargetReachable = true
	}
	return state, nil
}

func validateTransformerName(name string) error {
	if name == "" {
		return errors.New("transformer name must not be empty")
	}
	if _, ok := allowedTransformers[name]; ok {
		return nil
	}
	if strings.HasPrefix(name, "greenmask_") {
		return nil
	}
	return fmt.Errorf("unsupported transformer %q", name)
}

func withPgstreamConfigFile(cfg types.StreamConfig, fn func(path string) error) error {
	data, err := renderPgstreamConfig(cfg)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "pgschema-pgstream-*.yaml")
	if err != nil {
		return fmt.Errorf("create pgstream config: %w", err)
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("chmod pgstream config: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write pgstream config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close pgstream config: %w", err)
	}
	return fn(f.Name())
}

func renderPgstreamConfig(cfg types.StreamConfig) ([]byte, error) {
	cfg = normalizeStreamConfig(cfg)
	doc := pgstreamConfigDocument{
		Source: buildSourceConfig(cfg),
		Target: buildTargetConfig(cfg),
	}
	if filters := buildModifierFilter(cfg.Filters); filters != nil || len(cfg.AnonymizationRules) > 0 {
		doc.Modifiers = &pgstreamConfigModifiers{
			Filter:          filters,
			Transformations: buildTransformations(cfg.AnonymizationRules),
		}
	}
	return yaml.Marshal(doc)
}

type pgstreamConfigDocument struct {
	Source    pgstreamConfigSource     `yaml:"source"`
	Target    pgstreamConfigTarget     `yaml:"target,omitempty"`
	Modifiers *pgstreamConfigModifiers `yaml:"modifiers,omitempty"`
}

type pgstreamConfigSource struct {
	Postgres *pgstreamSourcePostgres `yaml:"postgres,omitempty"`
}

type pgstreamSourcePostgres struct {
	URL         string                     `yaml:"url"`
	Mode        types.StreamMode           `yaml:"mode,omitempty"`
	Snapshot    *pgstreamSourceSnapshot    `yaml:"snapshot,omitempty"`
	Replication *pgstreamSourceReplication `yaml:"replication,omitempty"`
	RetryPolicy *pgstreamRetryPolicy       `yaml:"retry_policy,omitempty"`
}

type pgstreamSourceSnapshot struct {
	Mode                    types.SnapshotMode          `yaml:"mode,omitempty"`
	Tables                  []string                    `yaml:"tables,omitempty"`
	ExcludedTables          []string                    `yaml:"excluded_tables,omitempty"`
	SchemaOnlyTables        []string                    `yaml:"schema_only_tables,omitempty"`
	SnapshotWorkers         int                         `yaml:"snapshot_workers,omitempty"`
	DisableProgressTracking bool                        `yaml:"disable_progress_tracking,omitempty"`
	Data                    *pgstreamSourceSnapshotData `yaml:"data,omitempty"`
	Schema                  *pgstreamSourceSchema       `yaml:"schema,omitempty"`
	Recorder                *pgstreamSnapshotRecorder   `yaml:"recorder,omitempty"`
}

type pgstreamSourceSnapshotData struct {
	SchemaWorkers  int   `yaml:"schema_workers,omitempty"`
	TableWorkers   int   `yaml:"table_workers,omitempty"`
	BatchBytes     int64 `yaml:"batch_bytes,omitempty"`
	MaxConnections int   `yaml:"max_connections,omitempty"`
}

type pgstreamSourceSchema struct {
	PGDumpPGRestore *pgstreamSchemaPGDumpRestore `yaml:"pgdump_pgrestore,omitempty"`
}

type pgstreamSchemaPGDumpRestore struct {
	CleanTargetDB  bool   `yaml:"clean_target_db,omitempty"`
	CreateTargetDB bool   `yaml:"create_target_db,omitempty"`
	DumpFile       string `yaml:"dump_file,omitempty"`
}

type pgstreamSnapshotRecorder struct {
	RepeatableSnapshots bool   `yaml:"repeatable_snapshots,omitempty"`
	PostgresURL         string `yaml:"postgres_url,omitempty"`
}

type pgstreamSourceReplication struct {
	ReplicationSlot string                       `yaml:"replication_slot"`
	Plugin          *pgstreamSourcePluginFilters `yaml:"plugin,omitempty"`
}

type pgstreamSourcePluginFilters struct {
	AddTables    string `yaml:"add_tables,omitempty"`
	FilterTables string `yaml:"filter_tables,omitempty"`
}

type pgstreamRetryPolicy struct {
	DisableRetries bool                            `yaml:"disable_retries,omitempty"`
	Exponential    *pgstreamRetryPolicyExponential `yaml:"exponential,omitempty"`
	Constant       *pgstreamRetryPolicyConstant    `yaml:"constant,omitempty"`
}

type pgstreamRetryPolicyExponential struct {
	InitialInterval int `yaml:"initial_interval,omitempty"`
	MaxInterval     int `yaml:"max_interval,omitempty"`
}

type pgstreamRetryPolicyConstant struct {
	MaxRetries int `yaml:"max_retries,omitempty"`
	Interval   int `yaml:"interval,omitempty"`
}

type pgstreamConfigTarget struct {
	Postgres *pgstreamTargetPostgres `yaml:"postgres,omitempty"`
	Kafka    *pgstreamTargetKafka    `yaml:"kafka,omitempty"`
	Search   *pgstreamTargetSearch   `yaml:"search,omitempty"`
	Webhooks *pgstreamTargetWebhooks `yaml:"webhooks,omitempty"`
	Stdout   map[string]any          `yaml:"stdout,omitempty"`
}

type pgstreamTargetPostgres struct {
	URL                   string               `yaml:"url"`
	MaxConnections        int                  `yaml:"max_connections,omitempty"`
	DisableTriggers       bool                 `yaml:"disable_triggers,omitempty"`
	OnConflictAction      string               `yaml:"on_conflict_action,omitempty"`
	StrictMode            bool                 `yaml:"strict_mode,omitempty"`
	IgnoreDDL             bool                 `yaml:"ignore_ddl,omitempty"`
	IncludeDDLObjectTypes []string             `yaml:"include_ddl_object_types,omitempty"`
	ExcludeDDLObjectTypes []string             `yaml:"exclude_ddl_object_types,omitempty"`
	Batch                 *pgstreamTargetBatch `yaml:"batch,omitempty"`
	BulkIngest            *pgstreamBulkIngest  `yaml:"bulk_ingest,omitempty"`
	RetryPolicy           *pgstreamRetryPolicy `yaml:"retry_policy,omitempty"`
}

type pgstreamTargetBatch struct {
	Timeout          int   `yaml:"timeout,omitempty"`
	Size             int   `yaml:"size,omitempty"`
	MaxBytes         int64 `yaml:"max_bytes,omitempty"`
	MaxQueueBytes    int64 `yaml:"max_queue_bytes,omitempty"`
	IgnoreSendErrors bool  `yaml:"ignore_send_errors,omitempty"`
}

type pgstreamBulkIngest struct {
	Enabled     bool `yaml:"enabled,omitempty"`
	CopyWorkers int  `yaml:"copy_workers,omitempty"`
}

type pgstreamTargetKafka struct {
	Servers []string                  `yaml:"servers,omitempty"`
	Topic   *pgstreamTargetKafkaTopic `yaml:"topic,omitempty"`
}

type pgstreamTargetKafkaTopic struct {
	Name              string `yaml:"name,omitempty"`
	Partitions        int    `yaml:"partitions,omitempty"`
	PartitionKey      string `yaml:"partition_key,omitempty"`
	ReplicationFactor int    `yaml:"replication_factor,omitempty"`
	AutoCreate        bool   `yaml:"auto_create,omitempty"`
}

type pgstreamTargetSearch struct {
	Engine     string `yaml:"engine,omitempty"`
	URL        string `yaml:"url,omitempty"`
	Index      string `yaml:"index,omitempty"`
	HashDocIDs bool   `yaml:"hash_doc_ids,omitempty"`
}

type pgstreamTargetWebhooks struct {
	Subscriptions *pgstreamWebhookSubscriptions `yaml:"subscriptions,omitempty"`
	Notifier      *pgstreamWebhookNotifier      `yaml:"notifier,omitempty"`
}

type pgstreamWebhookSubscriptions struct {
	Store  *pgstreamWebhookStore  `yaml:"store,omitempty"`
	Server *pgstreamWebhookServer `yaml:"server,omitempty"`
}

type pgstreamWebhookStore struct {
	URL string `yaml:"url,omitempty"`
}

type pgstreamWebhookServer struct {
	Address      string `yaml:"address,omitempty"`
	ReadTimeout  int    `yaml:"read_timeout,omitempty"`
	WriteTimeout int    `yaml:"write_timeout,omitempty"`
}

type pgstreamWebhookNotifier struct {
	WorkerCount   int `yaml:"worker_count,omitempty"`
	ClientTimeout int `yaml:"client_timeout,omitempty"`
}

type pgstreamConfigModifiers struct {
	Filter          *pgstreamModifierFilter     `yaml:"filter,omitempty"`
	Transformations *pgstreamModifierTransforms `yaml:"transformations,omitempty"`
}

type pgstreamModifierFilter struct {
	IncludeTables    []string `yaml:"include_tables,omitempty"`
	ExcludeTables    []string `yaml:"exclude_tables,omitempty"`
	SchemaOnlyTables []string `yaml:"schema_only_tables,omitempty"`
}

type pgstreamModifierTransforms struct {
	ValidationMode    string                     `yaml:"validation_mode,omitempty"`
	TableTransformers []pgstreamTableTransformer `yaml:"table_transformers,omitempty"`
}

type pgstreamTableTransformer struct {
	Schema             string                               `yaml:"schema"`
	Table              string                               `yaml:"table"`
	ColumnTransformers map[string]pgstreamColumnTransformer `yaml:"column_transformers,omitempty"`
}

type pgstreamColumnTransformer struct {
	Name string `yaml:"name"`
}

func buildSourceConfig(cfg types.StreamConfig) pgstreamConfigSource {
	source := &pgstreamSourcePostgres{
		URL:  cfg.SourceDSN,
		Mode: cfg.Mode,
	}
	if cfg.Mode == types.StreamModeSnapshot || cfg.Mode == types.StreamModeSnapshotAndReplication {
		source.Snapshot = &pgstreamSourceSnapshot{
			Mode:                    defaultSnapshotMode(cfg.Snapshot.Mode),
			Tables:                  includeTables(cfg.Filters),
			ExcludedTables:          cfg.Filters.ExcludedTables,
			SchemaOnlyTables:        cfg.Filters.SchemaOnlyTables,
			SnapshotWorkers:         cfg.Snapshot.SnapshotWorkers,
			DisableProgressTracking: cfg.Snapshot.DisableProgress,
			Data: &pgstreamSourceSnapshotData{
				SchemaWorkers:  cfg.Snapshot.SchemaWorkers,
				TableWorkers:   cfg.Snapshot.TableWorkers,
				BatchBytes:     cfg.Snapshot.BatchBytes,
				MaxConnections: cfg.Snapshot.MaxConnections,
			},
			Schema: &pgstreamSourceSchema{
				PGDumpPGRestore: &pgstreamSchemaPGDumpRestore{
					CleanTargetDB:  cfg.Snapshot.CleanTargetDatabase || cfg.Snapshot.ResetTarget,
					CreateTargetDB: cfg.Snapshot.CreateTargetDB,
					DumpFile:       cfg.Snapshot.DumpFile,
				},
			},
		}
		if cfg.Target.Type == types.StreamTargetTypePostgres && cfg.Target.Postgres != nil && cfg.Target.Postgres.URL != "" {
			source.Snapshot.Recorder = &pgstreamSnapshotRecorder{
				RepeatableSnapshots: cfg.Snapshot.Repeatable,
				PostgresURL:         cfg.Target.Postgres.URL,
			}
		}
	}
	if cfg.Mode == types.StreamModeReplication || cfg.Mode == types.StreamModeSnapshotAndReplication {
		source.Replication = &pgstreamSourceReplication{
			ReplicationSlot: cfg.ReplicationSlotName,
			Plugin: &pgstreamSourcePluginFilters{
				AddTables:    strings.Join(includeTables(cfg.Filters), ","),
				FilterTables: strings.Join(excludeTables(cfg.Filters), ","),
			},
		}
	}
	if rp := buildRetryPolicy(cfg.Target.Postgres); rp != nil {
		source.RetryPolicy = rp
	}
	return pgstreamConfigSource{Postgres: source}
}

func buildTargetConfig(cfg types.StreamConfig) pgstreamConfigTarget {
	switch cfg.Target.Type {
	case types.StreamTargetTypeKafka:
		if cfg.Target.Kafka == nil {
			return pgstreamConfigTarget{}
		}
		return pgstreamConfigTarget{
			Kafka: &pgstreamTargetKafka{
				Servers: cfg.Target.Kafka.Servers,
				Topic: &pgstreamTargetKafkaTopic{
					Name:              cfg.Target.Kafka.TopicName,
					Partitions:        cfg.Target.Kafka.Partitions,
					PartitionKey:      cfg.Target.Kafka.PartitionKey,
					ReplicationFactor: cfg.Target.Kafka.ReplicationFactor,
					AutoCreate:        cfg.Target.Kafka.AutoCreate,
				},
			},
		}
	case types.StreamTargetTypeElasticsearch, types.StreamTargetTypeOpenSearch:
		search := cfg.Target.Elasticsearch
		if cfg.Target.Type == types.StreamTargetTypeOpenSearch {
			search = cfg.Target.OpenSearch
		}
		if search == nil {
			return pgstreamConfigTarget{}
		}
		return pgstreamConfigTarget{
			Search: &pgstreamTargetSearch{
				Engine:     string(search.Engine),
				URL:        search.URL,
				Index:      search.Index,
				HashDocIDs: search.HashDocIDs,
			},
		}
	case types.StreamTargetTypeWebhook:
		if cfg.Target.Webhook == nil {
			return pgstreamConfigTarget{}
		}
		return pgstreamConfigTarget{
			Webhooks: &pgstreamTargetWebhooks{
				Subscriptions: &pgstreamWebhookSubscriptions{
					Store: &pgstreamWebhookStore{URL: cfg.Target.Webhook.StoreURL},
					Server: &pgstreamWebhookServer{
						Address:      cfg.Target.Webhook.ServerAddress,
						ReadTimeout:  cfg.Target.Webhook.ReadTimeoutS,
						WriteTimeout: cfg.Target.Webhook.WriteTimeoutS,
					},
				},
				Notifier: &pgstreamWebhookNotifier{
					WorkerCount:   cfg.Target.Webhook.WorkerCount,
					ClientTimeout: cfg.Target.Webhook.ClientTimeout,
				},
			},
		}
	case types.StreamTargetTypeStdout:
		return pgstreamConfigTarget{Stdout: map[string]any{}}
	default:
		pg := cfg.Target.Postgres
		if pg == nil {
			pg = &types.PostgresTargetConfig{URL: cfg.TargetDSN}
		}
		target := &pgstreamTargetPostgres{
			URL:              pg.URL,
			MaxConnections:   pg.MaxConnections,
			DisableTriggers:  pg.DisableTriggers,
			OnConflictAction: pg.OnConflictAction,
			StrictMode:       pg.StrictMode,
			IgnoreDDL:        cfg.SchemaChangePolicy == types.SchemaChangePolicyBlock || cfg.SchemaChangePolicy == types.SchemaChangePolicyRequireApproval || pg.IgnoreDDL,
			Batch: &pgstreamTargetBatch{
				Timeout:          pg.BatchTimeoutMS,
				Size:             pg.BatchSize,
				MaxBytes:         pg.BatchMaxBytes,
				MaxQueueBytes:    pg.BatchMaxQueueBytes,
				IgnoreSendErrors: pg.IgnoreSendErrors,
			},
			BulkIngest: &pgstreamBulkIngest{
				Enabled:     pg.BulkIngest,
				CopyWorkers: pg.CopyWorkers,
			},
			RetryPolicy: buildRetryPolicy(pg),
		}
		if len(cfg.Filters.IncludeDDLObjectTypes) > 0 {
			target.IncludeDDLObjectTypes = cfg.Filters.IncludeDDLObjectTypes
		}
		if len(cfg.Filters.ExcludeDDLObjectTypes) > 0 {
			target.ExcludeDDLObjectTypes = cfg.Filters.ExcludeDDLObjectTypes
		}
		return pgstreamConfigTarget{Postgres: target}
	}
}

func buildRetryPolicy(pg *types.PostgresTargetConfig) *pgstreamRetryPolicy {
	if pg == nil {
		return nil
	}
	rp := pg.RetryPolicy
	if rp == (types.StreamRetryPolicy{}) {
		return nil
	}
	out := &pgstreamRetryPolicy{DisableRetries: rp.DisableRetries}
	if rp.InitialIntervalMS > 0 || rp.MaxIntervalMS > 0 {
		out.Exponential = &pgstreamRetryPolicyExponential{
			InitialInterval: rp.InitialIntervalMS,
			MaxInterval:     rp.MaxIntervalMS,
		}
	}
	if rp.ConstantMaxRetries > 0 || rp.ConstantIntervalMS > 0 {
		out.Constant = &pgstreamRetryPolicyConstant{
			MaxRetries: rp.ConstantMaxRetries,
			Interval:   rp.ConstantIntervalMS,
		}
	}
	return out
}

func buildModifierFilter(filters types.StreamFilters) *pgstreamModifierFilter {
	include := includeTables(filters)
	exclude := excludeTables(filters)
	if len(include) == 0 && len(exclude) == 0 && len(filters.SchemaOnlyTables) == 0 {
		return nil
	}
	return &pgstreamModifierFilter{
		IncludeTables:    include,
		ExcludeTables:    exclude,
		SchemaOnlyTables: filters.SchemaOnlyTables,
	}
}

func buildTransformations(rules []types.AnonymizationRule) *pgstreamModifierTransforms {
	if len(rules) == 0 {
		return nil
	}
	byTable := map[string]*pgstreamTableTransformer{}
	order := []string{}
	for _, rule := range rules {
		schema, table := splitQualifiedTable(rule.Table)
		key := schema + "." + table
		entry := byTable[key]
		if entry == nil {
			entry = &pgstreamTableTransformer{
				Schema:             schema,
				Table:              table,
				ColumnTransformers: map[string]pgstreamColumnTransformer{},
			}
			byTable[key] = entry
			order = append(order, key)
		}
		entry.ColumnTransformers[rule.Column] = pgstreamColumnTransformer{Name: rule.Transformer}
	}
	out := &pgstreamModifierTransforms{ValidationMode: "strict"}
	for _, key := range order {
		out.TableTransformers = append(out.TableTransformers, *byTable[key])
	}
	return out
}

func defaultSnapshotMode(mode types.SnapshotMode) types.SnapshotMode {
	if mode == "" {
		return types.SnapshotModeFull
	}
	return mode
}

func includeTables(filters types.StreamFilters) []string {
	var out []string
	out = append(out, filters.IncludedTables...)
	for _, schema := range filters.IncludedSchemas {
		out = append(out, qualifySchemaWildcard(schema))
	}
	return dedupeStrings(out)
}

func excludeTables(filters types.StreamFilters) []string {
	var out []string
	out = append(out, filters.ExcludedTables...)
	for _, schema := range filters.ExcludedSchemas {
		out = append(out, qualifySchemaWildcard(schema))
	}
	return dedupeStrings(out)
}

func qualifySchemaWildcard(schema string) string {
	if strings.Contains(schema, ".") {
		return schema
	}
	return schema + ".*"
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func splitQualifiedTable(name string) (schema string, table string) {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) == 1 {
		return "public", parts[0]
	}
	return parts[0], parts[1]
}

func parseStatusJSON(out []byte) (map[string]any, error) {
	var status map[string]any
	if err := json.Unmarshal(out, &status); err != nil {
		return nil, fmt.Errorf("parsing pgstream status output: %w", err)
	}
	return status, nil
}

func mergeHealthFromStatus(health *types.StreamHealthResponse, status map[string]any) {
	if lag, ok := lookupInt64(status, "lag_bytes"); ok {
		health.LagBytes = lag
	}
	if lsn, ok := lookupString(status, "current_lsn"); ok && lsn != "" {
		health.CurrentLSN = lsn
	}
	if slotName, ok := lookupString(status, "replication_slot_name"); ok && slotName != "" {
		health.ReplicationSlotName = slotName
	}
	if reachable, ok := lookupBool(status, "reachable"); ok {
		health.SourceReachable = reachable
	}
	if phase, ok := lookupString(status, "snapshot_phase"); ok {
		health.Snapshot.Phase = phase
	}
	if phase, ok := lookupString(status, "phase"); ok && health.Phase == "" {
		health.Phase = phase
	}
	if rows, ok := lookupInt64(status, "rows_copied"); ok {
		health.Snapshot.RowsCopied = rows
	}
	if tables, ok := lookupInt64(status, "tables_completed"); ok {
		health.Snapshot.TablesCompleted = int(tables)
	}
}

func lookupString(v any, key string) (string, bool) {
	found, ok := lookupValue(v, key)
	if !ok {
		return "", false
	}
	switch x := found.(type) {
	case string:
		return x, true
	default:
		return fmt.Sprint(x), true
	}
}

func lookupBool(v any, key string) (bool, bool) {
	found, ok := lookupValue(v, key)
	if !ok {
		return false, false
	}
	b, ok := found.(bool)
	return b, ok
}

func lookupInt64(v any, key string) (int64, bool) {
	found, ok := lookupValue(v, key)
	if !ok {
		return 0, false
	}
	switch x := found.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func lookupValue(v any, key string) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		if found, ok := x[key]; ok {
			return found, true
		}
		for _, value := range x {
			if found, ok := lookupValue(value, key); ok {
				return found, true
			}
		}
	case []any:
		for _, value := range x {
			if found, ok := lookupValue(value, key); ok {
				return found, true
			}
		}
	}
	return nil, false
}
