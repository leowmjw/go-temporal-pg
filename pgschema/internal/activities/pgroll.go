// Package activities provides Temporal activity implementations for pgroll
// zero-downtime schema migrations.
package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

const defaultExpectedPgrollVersion = "v0.16.2"

const (
	reconcileActionContinue     = "continue"
	reconcileActionResumeWait   = "resume_wait"
	reconcileActionAlreadyDone  = "already_complete"
	reconcileActionSkipComplete = "skip_complete"
	reconcileActionSkipRollback = "skip_rollback"
	reconcileActionFail         = "fail"
)

// PgrollActivities holds all pgroll-related Temporal activities.
// Replace any function field with an anonymous function in tests.
type PgrollActivities struct {
	baseActivities
	ValidateFn     func(ctx context.Context, input types.MigrationInput) error
	StartFn        func(ctx context.Context, input types.MigrationInput) error
	CompleteFn     func(ctx context.Context, input types.MigrationInput) error
	RollbackFn     func(ctx context.Context, input types.MigrationInput) error
	StatusFn       func(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error)
	VersionFn      func(ctx context.Context, input types.MigrationInput) (string, error)
	ReadinessFn    func(ctx context.Context, input types.MigrationInput) (*types.PgrollReadiness, error)
	LatestSchemaFn func(ctx context.Context, input types.MigrationInput) (string, error)
	RiskFn         func(ctx context.Context, input types.MigrationInput) (*types.MigrationRiskReport, error)
	ReconcileFn    func(ctx context.Context, input types.ReconcileInput) (*types.ReconciliationResult, error)
	BaselineFn     func(ctx context.Context, input types.BaselineInput) (*types.BaselineResult, error)
}

// NewPgrollActivities returns a PgrollActivities wired to the real pgroll binary.
func NewPgrollActivities(log *slog.Logger) *PgrollActivities {
	a := &PgrollActivities{baseActivities: baseActivities{log: log}}
	a.ValidateFn = a.defaultValidate
	a.StartFn = a.defaultStart
	a.CompleteFn = a.defaultComplete
	a.RollbackFn = a.defaultRollback
	a.StatusFn = a.defaultStatus
	a.VersionFn = a.defaultVersion
	a.ReadinessFn = a.defaultReadiness
	a.LatestSchemaFn = a.defaultLatestSchema
	a.RiskFn = a.defaultRisk
	a.ReconcileFn = a.defaultReconcile
	a.BaselineFn = a.defaultBaseline
	return a
}

// ValidateMigration dry-runs the migration JSON before any DDL touches the DB.
func (a *PgrollActivities) ValidateMigration(ctx context.Context, input types.MigrationInput) error {
	end := a.startTrace(ctx, "pgroll.validate", slog.String("schema", input.Schema))
	err := a.ValidateFn(ctx, input)
	end(err)
	if err != nil {
		return &types.MigrationError{Phase: "validate", Wrapped: err}
	}
	return nil
}

// StartMigration runs pgroll start (expand phase: old+new schema coexist).
func (a *PgrollActivities) StartMigration(ctx context.Context, input types.MigrationInput) error {
	end := a.startTrace(ctx, "pgroll.start", slog.String("schema", input.Schema))
	safeHeartbeat(ctx, "starting")
	err := a.StartFn(ctx, input)
	safeHeartbeat(ctx, "started")
	end(err)
	if err != nil {
		return &types.MigrationError{Phase: "start", Wrapped: err}
	}
	return nil
}

// CompleteMigration runs pgroll complete (contract phase: old schema removed).
func (a *PgrollActivities) CompleteMigration(ctx context.Context, input types.MigrationInput) error {
	end := a.startTrace(ctx, "pgroll.complete", slog.String("schema", input.Schema))
	safeHeartbeat(ctx, "completing")
	err := a.CompleteFn(ctx, input)
	safeHeartbeat(ctx, "completed")
	end(err)
	if err != nil {
		return &types.MigrationError{Phase: "complete", Wrapped: err}
	}
	return nil
}

// RollbackMigration reverts the expand phase.
func (a *PgrollActivities) RollbackMigration(ctx context.Context, input types.MigrationInput) error {
	end := a.startTrace(ctx, "pgroll.rollback", slog.String("schema", input.Schema))
	err := a.RollbackFn(ctx, input)
	end(err)
	if err != nil {
		return &types.MigrationError{Phase: "rollback", Wrapped: err}
	}
	return nil
}

// GetMigrationStatus returns the current pgroll migration state.
func (a *PgrollActivities) GetMigrationStatus(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error) {
	end := a.startTrace(ctx, "pgroll.status", slog.String("schema", input.Schema))
	status, err := a.StatusFn(ctx, input)
	end(err)
	if err != nil {
		return nil, &types.MigrationError{Phase: "status", Wrapped: err}
	}
	return status, nil
}

// CheckPgrollVersion verifies the worker can execute pgroll and reports the version.
func (a *PgrollActivities) CheckPgrollVersion(ctx context.Context, input types.MigrationInput) (string, error) {
	end := a.startTrace(ctx, "pgroll.version")
	version, err := a.VersionFn(ctx, input)
	end(err)
	if err != nil {
		return "", &types.MigrationError{Phase: "version", Wrapped: err}
	}
	return version, nil
}

// CheckPgrollReadiness verifies pgroll metadata exists for the target schema.
func (a *PgrollActivities) CheckPgrollReadiness(ctx context.Context, input types.MigrationInput) (*types.PgrollReadiness, error) {
	end := a.startTrace(ctx, "pgroll.readiness", slog.String("schema", input.Schema))
	readiness, err := a.ReadinessFn(ctx, input)
	end(err)
	if err != nil {
		return nil, &types.MigrationError{Phase: "preflight", Wrapped: err}
	}
	return readiness, nil
}

// GetLatestSchema returns the current latest versioned schema name.
func (a *PgrollActivities) GetLatestSchema(ctx context.Context, input types.MigrationInput) (string, error) {
	end := a.startTrace(ctx, "pgroll.latest_schema", slog.String("schema", input.Schema))
	latest, err := a.LatestSchemaFn(ctx, input)
	end(err)
	if err != nil {
		return "", &types.MigrationError{Phase: "latest_schema", Wrapped: err}
	}
	return latest, nil
}

// AnalyzeMigrationRisk classifies pgroll operations and applies policy gates.
func (a *PgrollActivities) AnalyzeMigrationRisk(ctx context.Context, input types.MigrationInput) (*types.MigrationRiskReport, error) {
	end := a.startTrace(ctx, "pgroll.risk", slog.String("schema", input.Schema))
	report, err := a.RiskFn(ctx, input)
	end(err)
	if err != nil {
		return nil, &types.MigrationError{Phase: "policy", Wrapped: err}
	}
	return report, nil
}

// ReconcileMigrationState compares workflow intent with current pgroll state.
func (a *PgrollActivities) ReconcileMigrationState(ctx context.Context, input types.ReconcileInput) (*types.ReconciliationResult, error) {
	end := a.startTrace(ctx, "pgroll.reconcile", slog.String("phase", input.Phase), slog.String("schema", input.Migration.Schema))
	result, err := a.ReconcileFn(ctx, input)
	end(err)
	if err != nil {
		return nil, &types.MigrationError{Phase: "reconcile", Wrapped: err}
	}
	return result, nil
}

// CreateBaseline runs pgroll baseline for an existing schema.
func (a *PgrollActivities) CreateBaseline(ctx context.Context, input types.BaselineInput) (*types.BaselineResult, error) {
	end := a.startTrace(ctx, "pgroll.baseline", slog.String("schema", input.Schema), slog.String("version", input.Version), slog.String("directory", input.Directory), slog.String("operator", input.Operator))
	result, err := a.BaselineFn(ctx, input)
	end(err)
	if err != nil {
		return nil, &types.MigrationError{Phase: "baseline", Wrapped: err}
	}
	return result, nil
}

func (a *PgrollActivities) defaultValidate(ctx context.Context, input types.MigrationInput) error {
	return a.runPgroll(ctx, input.DSN, input.Schema, []string{"validate"}, input.MigrationJSON)
}

func (a *PgrollActivities) defaultStart(ctx context.Context, input types.MigrationInput) error {
	return a.runPgroll(ctx, input.DSN, input.Schema, []string{"start", "--complete=false"}, input.MigrationJSON)
}

func (a *PgrollActivities) defaultComplete(ctx context.Context, input types.MigrationInput) error {
	return a.runPgroll(ctx, input.DSN, input.Schema, []string{"complete"}, "")
}

func (a *PgrollActivities) defaultRollback(ctx context.Context, input types.MigrationInput) error {
	return a.runPgroll(ctx, input.DSN, input.Schema, []string{"rollback"}, "")
}

func (a *PgrollActivities) defaultStatus(ctx context.Context, input types.MigrationInput) (*types.MigrationStatus, error) {
	out, err := a.runCommand(ctx, "pgroll", []string{"--dsn", input.DSN, "--schema", input.Schema, "status", "--output", "json"}, withStdoutOnly())
	if err != nil {
		return nil, fmt.Errorf("pgroll status failed: %w", err)
	}
	status, err := parsePgrollStatusOutput(out)
	if err != nil {
		return nil, err
	}
	return status, nil
}

func (a *PgrollActivities) defaultVersion(ctx context.Context, input types.MigrationInput) (string, error) {
	out, err := a.runCommand(ctx, "pgroll", []string{"version"}, withStdoutOnly())
	if err != nil {
		out, err = a.runCommand(ctx, "pgroll", []string{"--version"}, withStdoutOnly())
		if err != nil {
			return "", fmt.Errorf("pgroll binary not available: %w", err)
		}
	}
	observed := normalizePgrollVersion(out)
	if observed == "" {
		return "", fmt.Errorf("pgroll version output was empty")
	}
	expected := strings.TrimSpace(input.ExpectedPgrollVersion)
	if expected == "" {
		expected = defaultExpectedPgrollVersion
	}
	if expected != "" && !strings.Contains(observed, expected) {
		if input.RequireExactPgrollVersion {
			return "", fmt.Errorf("pgroll version mismatch: expected %s, got %s", expected, observed)
		}
		a.logger().WarnContext(ctx, "pgroll version mismatch", slog.String("expected", expected), slog.String("observed", observed))
	}
	return observed, nil
}

func (a *PgrollActivities) defaultReadiness(ctx context.Context, input types.MigrationInput) (*types.PgrollReadiness, error) {
	status, err := a.defaultStatus(ctx, input)
	if err == nil {
		return &types.PgrollReadiness{Initialized: true, Message: fmt.Sprintf("pgroll metadata ready (%s)", status.Status)}, nil
	}
	if !looksLikeMissingPgrollMetadata(err) {
		return nil, fmt.Errorf("pgroll readiness check failed: %w", err)
	}
	if !input.AllowInitialize {
		return nil, fmt.Errorf("pgroll metadata missing for schema %q; run pgroll init or set allow_initialize: %w", input.Schema, err)
	}
	if runErr := a.runPgroll(ctx, input.DSN, input.Schema, []string{"init"}, ""); runErr != nil {
		return nil, fmt.Errorf("pgroll metadata missing and auto-init failed: %w", runErr)
	}
	status, err = a.defaultStatus(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("pgroll init completed but readiness re-check failed: %w", err)
	}
	return &types.PgrollReadiness{Initialized: true, AutoInitialized: true, Message: fmt.Sprintf("pgroll metadata initialized for schema %q", status.Schema)}, nil
}

func (a *PgrollActivities) defaultLatestSchema(ctx context.Context, input types.MigrationInput) (string, error) {
	out, err := a.runCommand(ctx, "pgroll", []string{"--dsn", input.DSN, "--schema", input.Schema, "latest", "schema"}, withStdoutOnly())
	if err != nil {
		return "", fmt.Errorf("pgroll latest schema failed: %w", err)
	}
	latest := strings.TrimSpace(string(out))
	if latest == "" {
		return "", fmt.Errorf("pgroll latest schema returned empty output")
	}
	return latest, nil
}

func (a *PgrollActivities) defaultRisk(_ context.Context, input types.MigrationInput) (*types.MigrationRiskReport, error) {
	return analyzeMigrationRisk(input)
}

func (a *PgrollActivities) defaultReconcile(ctx context.Context, input types.ReconcileInput) (*types.ReconciliationResult, error) {
	status, err := a.defaultStatus(ctx, input.Migration)
	if err != nil {
		return nil, err
	}
	migrationName := parseMigrationName(input.Migration.MigrationJSON)
	statusText := strings.ToLower(strings.TrimSpace(status.Status))
	currentVersion := status.EffectiveVersion()
	matchesCurrent := migrationName != "" && currentVersion == migrationName

	result := &types.ReconciliationResult{Action: reconcileActionContinue, Status: status}
	switch input.Phase {
	case "before_start":
		switch {
		case strings.Contains(statusText, "in progress") && matchesCurrent:
			result.Action = reconcileActionResumeWait
			result.Reason = "pgroll already shows this migration in progress"
		case strings.Contains(statusText, "complete") && matchesCurrent:
			result.Action = reconcileActionAlreadyDone
			result.Reason = "pgroll already shows this migration as complete"
		case strings.Contains(statusText, "roll") && matchesCurrent:
			result.Action = reconcileActionFail
			result.Reason = "pgroll reports the current migration as rolled back"
		}
	case "before_complete":
		switch {
		case strings.Contains(statusText, "complete") && matchesCurrent:
			result.Action = reconcileActionSkipComplete
			result.Reason = "pgroll already completed this migration"
		case strings.Contains(statusText, "in progress") && matchesCurrent:
			result.Action = reconcileActionContinue
		default:
			result.Action = reconcileActionFail
			result.Reason = fmt.Sprintf("unexpected pgroll status before complete: status=%q version=%q migration=%q", status.Status, currentVersion, migrationName)
		}
	case "before_rollback":
		switch {
		case strings.Contains(statusText, "in progress") && matchesCurrent:
			result.Action = reconcileActionContinue
		case strings.Contains(statusText, "complete") && matchesCurrent:
			result.Action = reconcileActionSkipRollback
			result.Reason = "pgroll already completed this migration"
		case strings.Contains(statusText, "no migrations"):
			result.Action = reconcileActionSkipRollback
			result.Reason = "pgroll reports no active migration to roll back"
		case strings.Contains(statusText, "roll"):
			result.Action = reconcileActionSkipRollback
			result.Reason = "pgroll already reports a rolled back state"
		case migrationName != "" && currentVersion != "" && currentVersion != migrationName:
			result.Action = reconcileActionSkipRollback
			result.Reason = fmt.Sprintf("pgroll is on different version %q", currentVersion)
		}
	}
	a.logger().InfoContext(ctx, "pgroll reconciliation decision", slog.String("phase", input.Phase), slog.String("action", result.Action), slog.String("reason", result.Reason), slog.String("status", status.Status), slog.String("version", currentVersion))
	return result, nil
}

func (a *PgrollActivities) defaultBaseline(ctx context.Context, input types.BaselineInput) (*types.BaselineResult, error) {
	if input.Version == "" {
		return nil, fmt.Errorf("baseline version is required")
	}
	if input.Directory == "" {
		return nil, fmt.Errorf("baseline directory is required")
	}
	if err := os.MkdirAll(input.Directory, 0o755); err != nil {
		return nil, fmt.Errorf("create baseline directory: %w", err)
	}

	migrationInput := types.MigrationInput{
		DSN:                       input.DSN,
		Schema:                    input.Schema,
		AllowInitialize:           input.AllowInitialize,
		ExpectedPgrollVersion:     input.ExpectedPgrollVersion,
		RequireExactPgrollVersion: input.RequireExactPgrollVersion,
	}
	version, err := a.defaultVersion(ctx, migrationInput)
	if err != nil {
		return nil, err
	}
	if status, err := a.defaultStatus(ctx, migrationInput); err == nil {
		if status.Status != "No migrations" && status.EffectiveVersion() != "" {
			return &types.BaselineResult{
				Version:       input.Version,
				Directory:     input.Directory,
				Schema:        input.Schema,
				Operator:      input.Operator,
				Status:        "already_baselined",
				PgrollVersion: version,
				CreatedAt:     time.Now().UTC(),
			}, nil
		}
	}

	args := []string{"--dsn", input.DSN, "--schema", input.Schema, "baseline", input.Version, input.Directory, "--yes"}
	if strings.EqualFold(input.Format, "json") || input.Format == "" {
		args = append(args, "--json")
	}
	if _, err := a.runCommand(ctx, "pgroll", args); err != nil {
		return nil, fmt.Errorf("pgroll baseline failed: %w", err)
	}
	return &types.BaselineResult{
		Version:       input.Version,
		Directory:     input.Directory,
		Schema:        input.Schema,
		Operator:      input.Operator,
		Status:        "created",
		PgrollVersion: version,
		CreatedAt:     time.Now().UTC(),
	}, nil
}

func parsePgrollStatusOutput(out []byte) (*types.MigrationStatus, error) {
	var raw struct {
		Name      string    `json:"name"`
		Version   string    `json:"version"`
		Status    string    `json:"status"`
		Schema    string    `json:"schema"`
		StartedAt time.Time `json:"started_at"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parsing pgroll status output: %w", err)
	}
	status := &types.MigrationStatus{
		Name:      strings.TrimSpace(raw.Name),
		Version:   strings.TrimSpace(raw.Version),
		Status:    strings.TrimSpace(raw.Status),
		Schema:    strings.TrimSpace(raw.Schema),
		StartedAt: raw.StartedAt,
	}
	if status.Version == "" {
		status.Version = status.Name
	}
	if status.Name == "" {
		status.Name = status.Version
	}
	if status.Status == "" {
		return nil, fmt.Errorf("parsing pgroll status output: missing status")
	}
	return status, nil
}

func normalizePgrollVersion(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func looksLikeMissingPgrollMetadata(err error) bool {
	msg := strings.ToLower(err.Error())
	needles := []string{"init", "initialize", "state schema", "metadata", "no migrations table", "pgroll was not initialized"}
	for _, needle := range needles {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func analyzeMigrationRisk(input types.MigrationInput) (*types.MigrationRiskReport, error) {
	doc, err := parseMigrationDocument(input.MigrationJSON)
	if err != nil {
		return nil, err
	}
	report := &types.MigrationRiskReport{
		MigrationName: doc.Name,
		OverallRisk:   "low",
		Summary:       "migration contains no elevated-risk operations",
	}
	for idx, operation := range doc.Operations {
		if len(operation) != 1 {
			return nil, fmt.Errorf("operation %d must contain exactly one pgroll operation", idx)
		}
		for opName, body := range operation {
			finding := classifyOperationRisk(input.Schema, input.Policy, opName, body)
			if finding == nil {
				continue
			}
			report.Findings = append(report.Findings, *finding)
			if compareRisk(finding.Risk, report.OverallRisk) > 0 {
				report.OverallRisk = finding.Risk
			}
			if policyBlocksFinding(input.Policy, *finding) {
				report.Blocked = true
			}
		}
	}
	if threshold := input.Policy.RequireApprovalForRisk; threshold != "" && compareRisk(report.OverallRisk, threshold) >= 0 && !input.Policy.Approved {
		report.RequiresApproval = true
	}
	if len(report.Findings) > 0 {
		report.Summary = fmt.Sprintf("migration risk=%s findings=%d", report.OverallRisk, len(report.Findings))
	}
	if report.Blocked {
		report.Summary = report.Summary + "; blocked by policy"
	}
	if report.RequiresApproval {
		report.Summary = report.Summary + "; requires approval"
	}
	return report, nil
}

type migrationDocument struct {
	Name       string                       `json:"name"`
	Operations []map[string]json.RawMessage `json:"operations"`
}

func parseMigrationDocument(raw string) (*migrationDocument, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("migration_json is required")
	}
	var doc migrationDocument
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse migration json: %w", err)
	}
	return &doc, nil
}

func parseMigrationName(raw string) string {
	doc, err := parseMigrationDocument(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(doc.Name)
}

func classifyOperationRisk(defaultSchema string, policy types.MigrationPolicy, opName string, body json.RawMessage) *types.MigrationRiskFinding {
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	targetSchema := extractString(payload, "schema")
	if targetSchema == "" {
		targetSchema = defaultSchema
	}
	targetTable := operationTargetTable(opName, payload)
	target := targetSchema
	if targetTable != "" {
		target = target + "." + targetTable
	}

	newFinding := func(category, risk, reason string) *types.MigrationRiskFinding {
		return &types.MigrationRiskFinding{Operation: opName, Category: category, Risk: risk, Target: target, Reason: reason}
	}

	switch opName {
	case "sql":
		return newFinding("raw_sql", "high", "raw SQL bypasses structured pgroll safety checks")
	case "rename_table", "rename_column", "rename_constraint":
		return newFinding("rename", "medium", "rename operations need coordinated application rollout")
	case "drop_table", "drop_column", "drop_index", "drop_constraint", "drop_multicolumn_constraint":
		return newFinding("destructive", "high", "destructive operations can remove or invalidate existing data paths")
	case "create_constraint":
		return newFinding("constraint", "medium", "new constraints can block existing writes until data is compliant")
	case "alter_column":
		if containsKey(payload, "default") || containsKey(payload, "using") {
			return newFinding("default_backfill", "high", "column rewrite/default operations may backfill or rewrite large tables")
		}
		return newFinding("alter", "medium", "column alterations can require coordinated rollout and validation")
	case "add_column":
		if containsKey(payload, "default") {
			return newFinding("default_backfill", "medium", "column defaults may backfill or lock large tables")
		}
	case "create_table":
		if containsKey(payload, "constraints") {
			return newFinding("constraint", "medium", "table-level constraints should be reviewed for rollout impact")
		}
	case "create_index":
		if containsKey(payload, "unique") {
			return newFinding("constraint", "medium", "unique indexes can fail if existing data is not unique")
		}
	}

	if inSliceFold(targetSchema, policy.ProtectedSchemas) {
		return newFinding("protected_schema", "critical", "operation touches a protected schema")
	}
	if targetTable != "" && inSliceFold(targetTable, policy.ProtectedTables) {
		return newFinding("protected_table", "critical", "operation touches a protected table")
	}
	return nil
}

func policyBlocksFinding(policy types.MigrationPolicy, finding types.MigrationRiskFinding) bool {
	switch finding.Category {
	case "raw_sql":
		return policy.BlockRawSQL
	case "rename":
		return policy.BlockRenames
	case "constraint":
		return policy.BlockConstraints
	case "default_backfill":
		return policy.BlockDefaults
	case "destructive":
		return policy.BlockDestructive
	case "protected_schema", "protected_table":
		return true
	default:
		return false
	}
}

func compareRisk(a, b string) int {
	rank := map[string]int{"": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}
	return rank[strings.ToLower(a)] - rank[strings.ToLower(b)]
}

func extractString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func containsKey(m map[string]any, key string) bool {
	if _, ok := m[key]; ok {
		return true
	}
	for _, value := range m {
		switch child := value.(type) {
		case map[string]any:
			if containsKey(child, key) {
				return true
			}
		case []any:
			for _, item := range child {
				if childMap, ok := item.(map[string]any); ok && containsKey(childMap, key) {
					return true
				}
			}
		}
	}
	return false
}

func operationTargetTable(opName string, payload map[string]any) string {
	switch opName {
	case "create_table", "drop_table":
		if name := extractString(payload, "name"); name != "" {
			return name
		}
	case "rename_table":
		if from := extractString(payload, "from"); from != "" {
			return from
		}
	case "rename_column", "add_column", "drop_column", "alter_column", "create_index", "drop_index", "create_constraint", "drop_constraint", "drop_multicolumn_constraint", "set_replica_identity":
		if table := extractString(payload, "table"); table != "" {
			return table
		}
	}
	return ""
}

func inSliceFold(value string, items []string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}
