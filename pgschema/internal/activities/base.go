// Package activities - shared plumbing for every Activities struct.
//
// baseActivities factors out what used to be copy-pasted across
// AlertActivities, PgrollActivities, PgstreamActivities, and
// PreviewDBActivities: the nil-safe logger, the exec.CommandContext
// wrap-error-with-output pattern, and (new) a pair of structured "flow"
// log helpers that give an agent reading logs enough to reconstruct the
// causal trace of a workflow run without a separate tracing backend.
package activities

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
)

// baseActivities holds state shared by every Activities struct in this
// package. Embed it by value: `type FooActivities struct { baseActivities; ... }`.
type baseActivities struct {
	log *slog.Logger
}

// logger returns the configured logger, falling back to slog.Default() so an
// Activities struct built as a zero value (e.g. in a test) never nil-panics
// on a log call.
func (b baseActivities) logger() *slog.Logger {
	if b.log == nil {
		return slog.Default()
	}
	return b.log
}

// ─── Trace / flow logging ──────────────────────────────────────────────────
//
// Every activity method, and every external command it shells out to, logs a
// matched start/end pair through startTrace. Each pair shares a "flow" field
// (start|end), an "op" name, and a random "trace_id" so retries and
// concurrent activities don't get their start/end lines interleaved and
// mismatched; the end line also carries elapsed duration and, if any, error.
// workflowAttrs adds the Temporal workflow/run/activity IDs when available so
// every line can be correlated back to a specific execution.
//
// This is deliberately just structured slog fields, not a tracer — the goal
// is that an agent (or `jq`) can reconstruct "what happened, in what order,
// how long did it take, did it fail" purely by reading the worker's log
// stream, which is what's actually available in most pgschema deployments
// today (no tracing backend assumed).

// startTrace logs the start of a named unit of work and returns a func that
// logs its completion. Call the returned func via defer:
//
//	end := a.startTrace(ctx, "pgroll.start", slog.String("schema", input.Schema))
//	defer func() { end(retErr) }()
func (b baseActivities) startTrace(ctx context.Context, op string, attrs ...slog.Attr) func(err error) {
	traceID := uuid.NewString()
	start := time.Now()

	startAttrs := append([]slog.Attr{
		slog.String("flow", "start"),
		slog.String("op", op),
		slog.String("trace_id", traceID),
	}, attrs...)
	startAttrs = append(startAttrs, workflowAttrs(ctx)...)
	b.logger().LogAttrs(ctx, slog.LevelInfo, op, startAttrs...)

	return func(err error) {
		endAttrs := []slog.Attr{
			slog.String("flow", "end"),
			slog.String("op", op),
			slog.String("trace_id", traceID),
			slog.Duration("elapsed", time.Since(start)),
		}
		endAttrs = append(endAttrs, workflowAttrs(ctx)...)
		if err != nil {
			endAttrs = append(endAttrs, slog.String("error", err.Error()))
			b.logger().LogAttrs(ctx, slog.LevelError, op, endAttrs...)
			return
		}
		b.logger().LogAttrs(ctx, slog.LevelInfo, op, endAttrs...)
	}
}

// workflowAttrs pulls the Temporal workflow/run/activity identifiers out of
// ctx so flow log lines can be correlated back to the execution that
// produced them. activity.GetInfo panics outside a real activity context
// (e.g. a unit test calling an activity method directly), so that case is
// recovered and simply yields no extra attrs rather than crashing the log
// call.
func workflowAttrs(ctx context.Context) (attrs []slog.Attr) {
	defer func() {
		if recover() != nil {
			attrs = nil
		}
	}()
	info := activity.GetInfo(ctx)
	if info.WorkflowExecution.ID == "" {
		return nil
	}
	return []slog.Attr{
		slog.String("workflow_id", info.WorkflowExecution.ID),
		slog.String("run_id", info.WorkflowExecution.RunID),
		slog.String("activity_id", info.ActivityID),
	}
}

// ─── Command execution ─────────────────────────────────────────────────────

// cmdConfig configures a single runCommand call.
type cmdConfig struct {
	stdoutOnly   bool   // use cmd.Output() (stdout only) instead of cmd.CombinedOutput()
	heartbeatMsg string // non-empty: run via Start/Wait, heartbeating this message every 30s while it blocks
}

type cmdOption func(*cmdConfig)

// withStdoutOnly captures only stdout (used for `--output json` commands
// where stderr noise would break JSON parsing).
func withStdoutOnly() cmdOption { return func(c *cmdConfig) { c.stdoutOnly = true } }

// withHeartbeat marks the command as long-running: it runs via Start/Wait
// instead of a single blocking call, recording a Temporal heartbeat with msg
// every 30s until it exits, so HeartbeatTimeout doesn't fire against a
// healthy but silent subprocess (pgstream run, pg_dump | psql, etc).
func withHeartbeat(msg string) cmdOption { return func(c *cmdConfig) { c.heartbeatMsg = msg } }

// runCommand runs an external binary and traces it via startTrace, tracing
// under "exec.<name>". On failure the returned error wraps both the
// underlying exec error and any captured output. This is the single place
// that implements the exec.CommandContext + wrap-error-with-output pattern
// previously reimplemented separately as runPgroll, runPgstream,
// runPgstreamOutput, runPgstreamHeartbeating, and the inline psql/pg_dump
// calls in preview_db.go.
func (b baseActivities) runCommand(ctx context.Context, name string, args []string, opts ...cmdOption) ([]byte, error) {
	var cfg cmdConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	end := b.startTrace(ctx, "exec."+name, slog.String("args", strings.Join(redactArgs(args), " ")))

	cmd := exec.CommandContext(ctx, name, args...)

	var out []byte
	var err error
	switch {
	case cfg.heartbeatMsg != "":
		out, err = runHeartbeating(ctx, cmd, cfg.heartbeatMsg)
	case cfg.stdoutOnly:
		out, err = cmd.Output()
	default:
		out, err = cmd.CombinedOutput()
	}

	end(err)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w — %s", name, strings.Join(args, " "), err, string(out))
	}
	return out, nil
}

// runHeartbeating starts cmd, waits for it to exit, and records a Temporal
// heartbeat with msg on a 30s ticker while it blocks. Both stdout and stderr
// are captured and returned combined, matching cmd.CombinedOutput()'s
// contract for the non-heartbeating path.
func runHeartbeating(ctx context.Context, cmd *exec.Cmd, msg string) ([]byte, error) {
	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			return []byte(combined.String()), err
		case <-ticker.C:
			safeHeartbeat(ctx, msg)
		}
	}
}

// redactArgs runs redactDSN over every arg so a logged command line never
// contains a raw password, whether the DSN was passed as a flag value
// (pgroll's --dsn, pgstream's --source-url/--target-url) or as a bare
// positional argument (psql, pg_dump). redactDSN is a no-op for args that
// don't look like a DSN.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = redactDSN(a)
	}
	return out
}

// ─── pgroll / pgstream CLI helpers shared across Activities structs ───────

// runPgroll invokes the pgroll CLI. When migrationJSON is non-empty it is
// written to a temp file and passed as the trailing positional <file>
// argument that `validate`/`start` require (pgroll v0.16.2 does not accept
// the migration on stdin — confirmed against the real binary; see
// AGENT.md). The flag is `--postgres-url`, not `--dsn` — also confirmed
// against `pgroll --help`, since v0.16.2 renamed/never had a `--dsn` flag.
// Shared by PgrollActivities and PreviewDBActivities (migration preview).
func (b baseActivities) runPgroll(ctx context.Context, dsn, schema string, args []string, migrationJSON string) error {
	full := append([]string{"--postgres-url", dsn, "--schema", schema}, args...)

	if migrationJSON != "" {
		f, err := os.CreateTemp("", "pgroll-migration-*.json")
		if err != nil {
			return fmt.Errorf("write migration file: %w", err)
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(migrationJSON); err != nil {
			f.Close()
			return fmt.Errorf("write migration file: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("write migration file: %w", err)
		}
		full = append(full, f.Name())
	}

	_, err := b.runCommand(ctx, "pgroll", full)
	return err
}

// runPgrollOutput invokes pgroll and returns stdout only, for subcommands
// like `status` that print JSON to stdout with no dedicated output flag
// (pgroll v0.16.2's `status` has no `--output`/`--json` flag — it always
// prints JSON).
func (b baseActivities) runPgrollOutput(ctx context.Context, dsn, schema string, args []string) ([]byte, error) {
	full := append([]string{"--postgres-url", dsn, "--schema", schema}, args...)
	return b.runCommand(ctx, "pgroll", full, withStdoutOnly())
}

// runPgroll status is handled separately (defaultStatus) because it needs
// stdout-only output for clean JSON parsing.

// runPgstream invokes the pgstream CLI for a short-lived subcommand and
// discards its output on success.
func (b baseActivities) runPgstream(ctx context.Context, args []string) error {
	_, err := b.runCommand(ctx, "pgstream", args)
	return err
}

// runPgstreamOutput invokes the pgstream CLI and returns stdout only (used
// for `--output json` commands).
func (b baseActivities) runPgstreamOutput(ctx context.Context, args []string) ([]byte, error) {
	return b.runCommand(ctx, "pgstream", args, withStdoutOnly())
}

// runPgstreamHeartbeating invokes a long-running pgstream subcommand (e.g.
// `run`, which drives CDC for up to 7 days), heartbeating stream_id=...
// every 30s while it blocks.
func (b baseActivities) runPgstreamHeartbeating(ctx context.Context, args []string, streamID string) error {
	_, err := b.runCommand(ctx, "pgstream", args, withHeartbeat(fmt.Sprintf("stream_id=%s running", streamID)))
	return err
}
