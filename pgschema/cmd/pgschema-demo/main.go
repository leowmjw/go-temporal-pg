// cmd/pgschema-demo is a small, self-contained web app for demoing the
// pgroll zero-downtime migration workflow end to end. It renders a page
// (using Datastar for reactivity, https://data-star.dev) that lets a
// presenter click through 5 increasingly complex real-life migration
// scenarios against a live SchemaMigrationWorkflow, watch phase/status
// change in real time over SSE, and send the app-ready / rollback signals
// that the workflow is actually waiting on.
//
// This is demo tooling, not part of the pgschema library: it talks to the
// same Temporal server and task queue as cmd/pgschema, but does not import
// or duplicate any activity/workflow logic — it only starts workflow
// executions and reads their public signal/query API.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/client"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/workflow"
)

// scenario describes one demo migration: what it does, why it's a realistic
// step, and which pgroll migration file drives it.
type scenario struct {
	ID          string
	Title       string
	Description string
	File        string
}

var scenarios = []scenario{
	{
		ID:          "1",
		Title:       "1. Add nullable column",
		Description: "Expand: add users.email (nullable). The safest possible change — no backfill, no risk to existing writes.",
		File:        "01_add_email.json",
	},
	{
		ID:          "2",
		Title:       "2. Add NOT NULL column + backfill",
		Description: "Add users.status NOT NULL DEFAULT 'active', with pgroll backfilling existing rows during the expand phase.",
		File:        "02_add_status_backfill.json",
	},
	{
		ID:          "3",
		Title:       "3. Rename a column",
		Description: "Rename users.full_name to users.display_name. pgroll keeps both names readable/writable until contract.",
		File:        "03_rename_full_name.json",
	},
	{
		ID:          "4",
		Title:       "4. Add a unique constraint",
		Description: "Add a unique constraint on users.email, built without locking out writers — a classic zero-downtime index case.",
		File:        "04_add_unique_email.json",
	},
	{
		ID:          "5",
		Title:       "5. Multi-op raw-SQL data migration",
		Description: "Split users.display_name into first_name/last_name with two backfilled add_column ops in one migration.",
		File:        "05_split_display_name.json",
	},
	{
		ID:          "5b",
		Title:       "5b. Rollback walkthrough",
		Description: "Same shape as scenario 1 (adds phone_number) — but this run is meant to be aborted. Click Rollback instead of App-ready.",
		File:        "06_rollback_demo_phone.json",
	},
	{
		ID:          "6",
		Title:       "6. PG18 bonus: sortable external ID",
		Description: "Add users.external_id UUID NOT NULL DEFAULT uuidv7() — Postgres 18's new builtin sortable UUID generator, backfilled with no extension required.",
		File:        "07_pg18_uuidv7_external_id.json",
	},
}

func scenarioByID(id string) (scenario, bool) {
	for _, s := range scenarios {
		if s.ID == id {
			return s, true
		}
	}
	return scenario{}, false
}

// run tracks the single in-flight demo workflow. A demo is presenter-driven
// and single-operator, so one global run (guarded by mu) is intentionally
// simpler than per-session state.
type run struct {
	WorkflowID string
	RunID      string
	Scenario   scenario
	StartedAt  time.Time
}

type server struct {
	temporal      client.Client
	log           *slog.Logger
	dsn           string
	schema        string
	migrationsDir string
	seedFile      string

	mu      sync.Mutex
	current *run
}

func main() {
	addr := flag.String("addr", ":8090", "listen address for the demo web UI")
	dsn := flag.String("dsn", envOr("PGROLL_DSN", "postgres://postgres:postgres@localhost:5432/pgschema_demo?sslmode=disable"), "DSN of the demo Postgres database (see demo/docker-compose.yml)")
	schemaName := flag.String("schema", envOr("SCHEMA", "public"), "pgroll-managed schema name")
	migrationsDir := flag.String("migrations", "./demo/migrations", "directory containing the scenario migration JSON files")
	seedFile := flag.String("seed", "./demo/init/001_seed_users.sql", "SQL file used to reseed the users table on reset")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	c, err := client.Dial(client.Options{
		HostPort:  os.Getenv("TEMPORAL_ADDRESS"),
		Namespace: os.Getenv("TEMPORAL_NAMESPACE"),
	})
	if err != nil {
		log.Error("failed to connect to Temporal", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer c.Close()

	srv := &server{
		temporal:      c,
		log:           log,
		dsn:           *dsn,
		schema:        *schemaName,
		migrationsDir: *migrationsDir,
		seedFile:      *seedFile,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.handleIndex)
	mux.HandleFunc("GET /status", srv.handleStatus)
	mux.HandleFunc("POST /scenario/{id}/start", srv.handleStartScenario)
	mux.HandleFunc("POST /signal/app-ready", srv.handleSignal(workflow.SignalAppReady, "app-ready"))
	mux.HandleFunc("POST /signal/rollback", srv.handleSignal(workflow.SignalRollback, "rollback"))
	mux.HandleFunc("POST /reset", srv.handleReset)

	log.Info("pgschema demo web UI listening", slog.String("addr", *addr), slog.String("dsn", activities.RedactDSN(*dsn)))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Error("server exited", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}


// ─── Page ───────────────────────────────────────────────────────────────────

var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, struct{ Scenarios []scenario }{scenarios}); err != nil {
		s.log.Error("render index", slog.String("error", err.Error()))
	}
}

// ─── Status snapshot (page load / reconnect) ──────────────────────────────

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	f := sseHeaders(w)
	s.mu.Lock()
	cur := s.current
	s.mu.Unlock()

	if cur == nil {
		_ = patchSignals(w, f, map[string]any{
			"phase": "idle", "status": "idle", "percent": 0, "message": "", "workflowId": "", "runId": "",
		})
		return
	}

	progress, err := s.queryProgress(r.Context(), cur)
	if err != nil {
		_ = patchSignals(w, f, map[string]any{"message": "status query failed: " + err.Error()})
		return
	}
	_ = patchSignals(w, f, progressSignals(cur, progress))
}

// ─── Start a scenario ───────────────────────────────────────────────────────

func (s *server) handleStartScenario(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sc, ok := scenarioByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	f := sseHeaders(w)

	s.mu.Lock()
	if s.current != nil {
		s.mu.Unlock()
		patchElements(w, f, "#log-lines", "append", logLine(fmt.Sprintf("a run is already active (%s) — wait for it to finish first", s.current.WorkflowID)))
		return
	}
	s.mu.Unlock()

	migrationJSON, err := os.ReadFile(filepath.Join(s.migrationsDir, sc.File))
	if err != nil {
		patchElements(w, f, "#log-lines", "append", logLine("failed to read migration file: "+err.Error()))
		return
	}

	workflowID := fmt.Sprintf("demo-%s-%d", sanitizeID(sc.ID), time.Now().Unix())
	input := types.MigrationInput{
		DSN:           s.dsn,
		Schema:        s.schema,
		MigrationJSON: string(migrationJSON),
	}

	wr, err := s.temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: workflow.SchemaMigrationTaskQueue,
	}, workflow.SchemaMigrationWorkflow, input)
	if err != nil {
		patchElements(w, f, "#log-lines", "append", logLine("failed to start workflow: "+err.Error()))
		return
	}

	cur := &run{WorkflowID: workflowID, RunID: wr.GetRunID(), Scenario: sc, StartedAt: time.Now()}
	s.mu.Lock()
	s.current = cur
	s.mu.Unlock()

	patchElements(w, f, "#log-lines", "append", logLine(fmt.Sprintf("started %q as workflow %s", sc.Title, workflowID)))
	_ = patchSignals(w, f, map[string]any{
		"workflowId": workflowID, "runId": wr.GetRunID(),
		"phase": "init", "status": "running", "percent": 0, "message": "",
	})

	s.streamProgress(r.Context(), w, f, cur)
}

// streamProgress polls the workflow's migration-progress query and pushes
// datastar-patch-signals/elements events until the run reaches a terminal
// status, the client disconnects, or a safety timeout elapses.
func (s *server) streamProgress(ctx context.Context, w http.ResponseWriter, f http.Flusher, cur *run) {
	deadline := time.Now().Add(15 * time.Minute)
	lastPhase := ""
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				patchElements(w, f, "#log-lines", "append", logLine("demo watchdog timeout — giving up on this run"))
				s.clearCurrent(cur)
				return
			}

			progress, err := s.queryProgress(ctx, cur)
			if err != nil {
				continue // transient — e.g. workflow not yet queryable right after Start
			}

			if progress.Phase != lastPhase {
				lastPhase = progress.Phase
				patchElements(w, f, "#log-lines", "append", logLine(fmt.Sprintf("[%s] phase → %s", cur.WorkflowID, progress.Phase)))
			}
			_ = patchSignals(w, f, progressSignals(cur, progress))

			if isTerminal(progress.Status) {
				patchElements(w, f, "#log-lines", "append", logLine(fmt.Sprintf("[%s] finished: %s", cur.WorkflowID, progress.Status)))
				s.clearCurrent(cur)
				return
			}
		}
	}
}

func (s *server) queryProgress(ctx context.Context, cur *run) (*types.ProgressResponse, error) {
	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	val, err := s.temporal.QueryWorkflow(qCtx, cur.WorkflowID, cur.RunID, workflow.QueryMigrationProgress)
	if err != nil {
		return nil, err
	}
	var progress types.ProgressResponse
	if err := val.Get(&progress); err != nil {
		return nil, err
	}
	return &progress, nil
}

func (s *server) clearCurrent(cur *run) {
	s.mu.Lock()
	if s.current == cur {
		s.current = nil
	}
	s.mu.Unlock()
}

func isTerminal(status string) bool {
	switch status {
	case "completed", "rolled_back", "rollback_failed", "failed":
		return true
	default:
		return false
	}
}

func progressSignals(cur *run, p *types.ProgressResponse) map[string]any {
	return map[string]any{
		"workflowId": cur.WorkflowID,
		"runId":      cur.RunID,
		"phase":      p.Phase,
		"status":     p.Status,
		"percent":    p.Percent,
		"message":    p.Message,
	}
}

// ─── Signals (app-ready / rollback) ────────────────────────────────────────

func (s *server) handleSignal(signalName, label string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := sseHeaders(w)

		s.mu.Lock()
		cur := s.current
		s.mu.Unlock()

		if cur == nil {
			patchElements(w, f, "#log-lines", "append", logLine("no active run to signal"))
			return
		}

		if err := s.temporal.SignalWorkflow(r.Context(), cur.WorkflowID, cur.RunID, signalName, nil); err != nil {
			patchElements(w, f, "#log-lines", "append", logLine(fmt.Sprintf("failed to send %s signal: %s", label, err.Error())))
			return
		}
		patchElements(w, f, "#log-lines", "append", logLine(fmt.Sprintf("[%s] sent %q signal", cur.WorkflowID, label)))
	}
}

// ─── Reset (recover from a stuck/bad demo state) ──────────────────────────

// handleReset wipes and reseeds the demo schema, then re-runs `pgroll init`
// + `pgroll baseline`, streaming progress into the activity log. It's the
// escape hatch for when the demo gets into a state the UI can't recover
// from on its own (a failed workflow, a scenario run out of order, a stale
// `current` run).
//
// Deliberately does NOT touch the Postgres container (no `docker compose
// down`/`up`, i.e. NOT `mise run demo-reset`): that command is owned by the
// `postgres` line in the dev Procfile, and stopping those containers out
// from under it makes that foreground process exit — which overmind treats
// as one of its managed processes dying, and it tears down the *entire*
// `mise run dev` session (temporal, worker, web included) in response. A
// schema-level wipe gets the same "start scenario 1 from a clean slate"
// result without taking down anything else.
func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	f := sseHeaders(w)

	// Whatever was in flight is meaningless once the schema gets wiped.
	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()

	_ = patchSignals(w, f, map[string]any{
		"resetting": true, "phase": "idle", "status": "idle", "percent": 0,
		"message": "", "workflowId": "", "runId": "",
	})
	logf := func(msg string) { patchElements(w, f, "#log-lines", "append", logLine(msg)) }
	logf("resetting demo database (schema wipe + reseed + pgroll init)…")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := s.resetDatabase(ctx, logf); err != nil {
		logf("reset failed: " + err.Error())
	} else {
		logf("demo database reset — ready to run scenario 1")
	}
	_ = patchSignals(w, f, map[string]any{"resetting": false})
}

// resetDatabase drops and recreates the demo schema (plus pgroll's own
// `pgroll` state schema), reseeds it from seedFile, and re-baselines it with
// pgroll — the same end state `mise run demo-init` produces against a fresh
// container, but performed in place against the running Postgres.
func (s *server) resetDatabase(ctx context.Context, logf func(string)) error {
	conn, err := pgx.Connect(ctx, s.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	logf(fmt.Sprintf("dropping schema %q and pgroll's state schema", s.schema))
	if _, err := conn.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pgx.Identifier{s.schema}.Sanitize())); err != nil {
		return fmt.Errorf("drop schema %s: %w", s.schema, err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", pgx.Identifier{s.schema}.Sanitize())); err != nil {
		return fmt.Errorf("recreate schema %s: %w", s.schema, err)
	}
	if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS pgroll CASCADE"); err != nil {
		return fmt.Errorf("drop pgroll state schema: %w", err)
	}

	logf("reseeding from " + s.seedFile)
	seedSQL, err := os.ReadFile(s.seedFile)
	if err != nil {
		return fmt.Errorf("read seed file: %w", err)
	}
	if _, err := conn.Exec(ctx, string(seedSQL)); err != nil {
		return fmt.Errorf("run seed sql: %w", err)
	}

	if err := s.runStreamed(ctx, logf, "pgroll", "--postgres-url", s.dsn, "--schema", s.schema, "init"); err != nil {
		return fmt.Errorf("pgroll init: %w", err)
	}

	baselineDir := ".data/pgroll-baseline"
	if err := os.MkdirAll(baselineDir, 0o755); err != nil {
		return fmt.Errorf("create baseline dir: %w", err)
	}
	if err := s.runStreamed(ctx, logf, "pgroll", "--postgres-url", s.dsn, "--schema", s.schema, "baseline", "00_baseline", baselineDir, "-y"); err != nil {
		return fmt.Errorf("pgroll baseline: %w", err)
	}
	return nil
}

// runStreamed runs a command, streaming each line of its combined
// stdout/stderr into logf as it happens (rather than buffering it all until
// exit) so a slow step like pgroll init is visible in the activity log
// while it's running, not just after.
func (s *server) runStreamed(ctx context.Context, logf func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		pw.Close()
	}()

	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		logf(scanner.Text())
	}
	return <-done
}

// ─── SSE helpers (Datastar v1.0.2 wire protocol: datastar-patch-signals /
// datastar-patch-elements — see https://data-star.dev/reference/sse_events) ──

func sseHeaders(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	f, _ := w.(http.Flusher)
	return f
}

func patchSignals(w io.Writer, f http.Flusher, v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "event: datastar-patch-signals\ndata: signals %s\n\n", b)
	if f != nil {
		f.Flush()
	}
	return nil
}

func patchElements(w io.Writer, f http.Flusher, selector, mode, html string) {
	fmt.Fprintf(w, "event: datastar-patch-elements\n")
	fmt.Fprintf(w, "data: selector %s\n", selector)
	fmt.Fprintf(w, "data: mode %s\n", mode)
	for _, line := range strings.Split(html, "\n") {
		fmt.Fprintf(w, "data: elements %s\n", line)
	}
	fmt.Fprint(w, "\n")
	if f != nil {
		f.Flush()
	}
}

func logLine(msg string) string {
	return fmt.Sprintf(`<div class="log-line"><span class="ts">%s</span> %s</div>`,
		time.Now().Format("15:04:05"), template.HTMLEscapeString(msg))
}

func sanitizeID(id string) string {
	return strings.ReplaceAll(id, "/", "-")
}
