// Package activities - copy-on-write preview DB activities.
//
// PreviewDBActivities creates, anonymises, migrates, and destroys ephemeral
// preview databases so that developers and agents can test schema changes
// against real data without any PII exposure.
package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
)

// PreviewDBActivities holds activities for managing preview clone databases.
type PreviewDBActivities struct {
	CloneFn              func(ctx context.Context, input types.PreviewCloneInput) (string, error)
	ApplyAnonymizationFn func(ctx context.Context, input types.AnonymizationInput) error
	RunMigrationPreviewFn func(ctx context.Context, targetDSN string, migrationJSON string) error
	DropFn               func(ctx context.Context, targetDSN string) error
	log                  *slog.Logger
}

// NewPreviewDBActivities returns a PreviewDBActivities backed by real
// pg_dump/psql and pgstream binaries.
func NewPreviewDBActivities(log *slog.Logger) *PreviewDBActivities {
	a := &PreviewDBActivities{log: log}
	a.CloneFn = a.defaultClone
	a.ApplyAnonymizationFn = a.defaultApplyAnonymization
	a.RunMigrationPreviewFn = a.defaultRunMigrationPreview
	a.DropFn = a.defaultDrop
	return a
}
// logger returns the struct's logger, falling back to slog.Default() if nil.
func (a *PreviewDBActivities) logger() *slog.Logger {
	if a.log == nil {
		return slog.Default()
	}
	return a.log
}


// ─── Temporal activity methods ────────────────────────────────────────────────

// CloneDatabase creates a fresh preview DB by dumping from source and
// restoring into a new schema-namespaced database.  Returns the target DSN.
func (a *PreviewDBActivities) CloneDatabase(ctx context.Context, input types.PreviewCloneInput) (string, error) {
	a.logger().InfoContext(ctx, "cloning database for preview",
		slog.String("preview_id", input.PreviewID),
		slog.String("source", redactDSN(input.SourceDSN)))

	targetDSN, err := a.CloneFn(ctx, input)
	if err != nil {
		return "", &types.PreviewError{PreviewID: input.PreviewID, Wrapped: err}
	}
	return targetDSN, nil
}

// ApplyAnonymization runs pgstream snapshot-mode transformations against the
// preview clone, replacing PII columns with synthetic values.
// This MUST succeed before ExposePreviewEndpoint can run.
func (a *PreviewDBActivities) ApplyAnonymization(ctx context.Context, input types.AnonymizationInput) error {
	a.logger().InfoContext(ctx, "applying anonymization rules",
		slog.Int("rule_count", len(input.Rules)))

	if err := a.ApplyAnonymizationFn(ctx, input); err != nil {
		return fmt.Errorf("anonymization failed: %w", err)
	}
	return nil
}

// RunMigrationPreview applies the pending pgroll migration against the clone.
// This is a dry-run in the sense that the preview DB is ephemeral — it tests
// whether the migration is safe before touching production.
func (a *PreviewDBActivities) RunMigrationPreview(ctx context.Context, targetDSN, migrationJSON string) error {
	a.logger().InfoContext(ctx, "running migration preview")
	if err := a.RunMigrationPreviewFn(ctx, targetDSN, migrationJSON); err != nil {
		return fmt.Errorf("migration preview failed: %w", err)
	}
	return nil
}

// ExposePreviewEndpoint builds and returns the connection details for the
// preview clone, including its expiry time derived from the TTL.
func (a *PreviewDBActivities) ExposePreviewEndpoint(_ context.Context, targetDSN string, ttl time.Duration) (*types.PreviewEndpoint, error) {
	ep := &types.PreviewEndpoint{
		DSN:       targetDSN,
		Schema:    "public",
		ExpiresAt: time.Now().Add(ttl),
	}
	return ep, nil
}

// DropPreviewDatabase drops the ephemeral preview database.  Called on TTL
// expiry or explicit cleanup signal.
func (a *PreviewDBActivities) DropPreviewDatabase(ctx context.Context, targetDSN string) error {
	a.logger().InfoContext(ctx, "dropping preview database")
	if err := a.DropFn(ctx, targetDSN); err != nil {
		return fmt.Errorf("drop preview DB failed: %w", err)
	}
	return nil
}

// ─── Default (binary) implementations ────────────────────────────────────────

// previewDBName generates a stable, UUID-based preview DB name.
func previewDBName(previewID string) string {
	// Use deterministic name from UUID v5 so retries are idempotent.
	ns := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	id := uuid.NewSHA1(ns, []byte(previewID))
	return "preview_" + strings.ReplaceAll(id.String(), "-", "_")
}

func (a *PreviewDBActivities) defaultClone(ctx context.Context, input types.PreviewCloneInput) (string, error) {
	dbName := previewDBName(input.PreviewID)
	base := baseConnStr(input.SourceDSN)
	targetDSN := joinDBName(base, dbName)
	maintenanceDSN := joinDBName(base, "postgres")

	// 0. The target database does not exist yet — pg_dump/psql only transfer
	//    schema+data, they never create the destination database themselves.
	createOut, err := exec.CommandContext(ctx, "psql", maintenanceDSN,
		"-c", fmt.Sprintf("CREATE DATABASE %q", dbName)).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("create preview database: %w\n%s", err, string(createOut))
	}

	// 1. pg_dump (plain SQL) | psql target.
	// NOTE: --format=custom produces a binary archive that only pg_restore
	// (not psql) can read; plain-text SQL output is required for this pipe.
	dumpCmd := exec.CommandContext(ctx, "pg_dump",
		"--no-owner", "--no-acl", "--format=plain", input.SourceDSN)
	restoreCmd := exec.CommandContext(ctx, "psql", targetDSN)

	pipe, err := dumpCmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("pipe setup: %w", err)
	}
	restoreCmd.Stdin = pipe

	if err := dumpCmd.Start(); err != nil {
		return "", fmt.Errorf("pg_dump start: %w", err)
	}
	if err := restoreCmd.Run(); err != nil {
		return "", fmt.Errorf("psql restore: %w", err)
	}
	if err := dumpCmd.Wait(); err != nil {
		return "", fmt.Errorf("pg_dump wait: %w", err)
	}
	return targetDSN, nil
}

func (a *PreviewDBActivities) defaultApplyAnonymization(ctx context.Context, input types.AnonymizationInput) error {
	args := []string{
		"snapshot",
		"--pgstream-pgurl", input.TargetDSN,
	}
	if len(input.Rules) == 0 {
		return runPgstream(ctx, args)
	}

	cfgData, err := marshalAnonymizationConfig(input.Rules)
	if err != nil {
		return fmt.Errorf("build anonymization config: %w", err)
	}
	cfgFile, err := os.CreateTemp("", "pgstream-anon-*.json")
	if err != nil {
		return fmt.Errorf("write anonymization config: %w", err)
	}
	defer os.Remove(cfgFile.Name())
	if _, err := cfgFile.Write(cfgData); err != nil {
		cfgFile.Close()
		return fmt.Errorf("write anonymization config: %w", err)
	}
	if err := cfgFile.Close(); err != nil {
		return fmt.Errorf("write anonymization config: %w", err)
	}

	args = append(args, "--config", cfgFile.Name())
	return runPgstream(ctx, args)
}

// anonymizationConfig is the transformer config document passed to
// `pgstream snapshot --config`.
type anonymizationConfig struct {
	Transformers []types.AnonymizationRule `json:"transformers"`
}

// marshalAnonymizationConfig builds the pgstream transformer config JSON for
// a set of anonymization rules. Split out from defaultApplyAnonymization so
// the rule-to-config mapping can be unit tested without shelling out.
func marshalAnonymizationConfig(rules []types.AnonymizationRule) ([]byte, error) {
	return json.Marshal(anonymizationConfig{Transformers: rules})
}

func (a *PreviewDBActivities) defaultRunMigrationPreview(ctx context.Context, targetDSN, migrationJSON string) error {
	return runPgroll(ctx, targetDSN, "public", []string{"start", "--complete"}, migrationJSON)
}

func (a *PreviewDBActivities) defaultDrop(ctx context.Context, targetDSN string) error {
	out, err := exec.CommandContext(ctx, "psql", targetDSN,
		"-c", "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()").
		CombinedOutput()
	if err != nil {
		return fmt.Errorf("terminate connections: %w\n%s", err, string(out))
	}
	// Extract dbname from DSN and DROP DATABASE.
	dbName := extractDBName(targetDSN)
	out, err = exec.CommandContext(ctx, "psql", joinDBName(baseConnStr(targetDSN), "postgres"),
		"-c", fmt.Sprintf("DROP DATABASE IF EXISTS %q", dbName)).
		CombinedOutput()
	if err != nil {
		return fmt.Errorf("drop database: %w\n%s", err, string(out))
	}
	return nil
}

// ─── DSN helpers ─────────────────────────────────────────────────────────────

// baseConnStr strips the database name from a DSN, returning a connection
// string suitable for connecting to the postgres maintenance database.
func baseConnStr(dsn string) string {
	// URI form: ******hostname:port/dbname → ******hostname:port
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if slash := strings.LastIndex(rest, "/"); slash >= 0 {
			return dsn[:i+3] + rest[:slash]
		}
	}
	// keyword=value form: strip dbname= token
	parts := strings.Fields(dsn)
	filtered := parts[:0]
	for _, p := range parts {
		if !strings.HasPrefix(p, "dbname=") {
			filtered = append(filtered, p)
		}
	}
	return strings.Join(filtered, " ")
}

// joinDBName appends dbName to a base connection string produced by
// baseConnStr, respecting whether base is URI-form ("scheme://host:port") or
// keyword=value form ("host=... user=..."). Naively concatenating "/dbname"
// onto a keyword=value base corrupts the last keyword's value instead of
// selecting a database.
func joinDBName(base, dbName string) string {
	if strings.Contains(base, "://") {
		return base + "/" + dbName
	}
	if base == "" {
		return "dbname=" + dbName
	}
	return base + " dbname=" + dbName
}

func extractDBName(dsn string) string {
	// URI form
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if slash := strings.LastIndex(rest, "/"); slash >= 0 {
			return rest[slash+1:]
		}
	}
	// keyword=value form
	for _, p := range strings.Fields(dsn) {
		if strings.HasPrefix(p, "dbname=") {
			return strings.TrimPrefix(p, "dbname=")
		}
	}
	return ""
}
