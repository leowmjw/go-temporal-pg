# pgschema — notes for agents

Temporal-based schema migration / CDC-stream / preview-clone package wrapping the
`pgroll` and `pgstream` CLIs. Package layout: `internal/activities` (CLI wrappers),
`internal/workflow` (Temporal workflows), `internal/types`, `cmd/pgschema` (worker
entrypoint), `cmd/pgschema-demo` (demo web UI), `demo/` (docker-compose + pgroll
migration fixtures for the demo).

`go build ./...`, `go vet ./...`, `go test ./... -race` are green as of this pass.
No CLAUDE.md exists at repo root or under `pgschema/` — this file is the only
agent-facing guidance here.

## pgroll — roadmap (status: all originally-listed gaps implemented 2026-08-25)

Current integration covers validate/start/complete/rollback/status, and every gap
below is now implemented and wired into `SchemaMigrationWorkflow`
(`internal/workflow/schema_migration.go`) and/or `BaselineWorkflow`
(`internal/workflow/baseline.go`), not just present as standalone activities:

| Gap | Status | Where |
|---|---|---|
| `pgroll init`/readiness check | **Done** | `PgrollActivities.CheckPgrollReadiness`/`defaultReadiness` (`internal/activities/pgroll.go`) — probes status, auto-runs `pgroll init` when `AllowInitialize` is set, else fails fast with a clear message. Called at workflow start. |
| `baseline` flow | **Done** | `PgrollActivities.CreateBaseline`/`defaultBaseline` wraps `pgroll baseline <version> <dir> --yes`, records operator/timestamp/status (`already_baselined` vs `created`); orchestrated end-to-end by `BaselineWorkflow` (version+readiness preflight, then baseline), queryable via `baseline-status`. |
| `latest schema` integration | **Done** | `PgrollActivities.GetLatestSchema`/`defaultLatestSchema` runs `pgroll latest schema`; called after `start` and surfaced as `progress.LatestSchema` on the `migration-progress` query. |
| Status checked only after completion | **Done** | `ReconcileMigrationState` is called at `before_start`, `before_complete`, and `before_rollback` phase boundaries (plus best-effort status refreshes throughout); `progress.ReconciliationAction`/`progress.PgrollStatus` expose the last-observed state. |
| Operation-level risk/policy analysis | **Done** | `PgrollActivities.AnalyzeMigrationRisk`/`analyzeMigrationRisk` parses the migration JSON pre-`validate`, classifies each operation (raw SQL, renames, destructive ops, defaults/backfills, protected schema/table), and enforces `types.MigrationPolicy` block/require-approval gates — a blocked or unapproved migration fails the workflow with a non-retryable error before `validate` runs. |
| pgroll binary/version preflight | **Done** | `PgrollActivities.CheckPgrollVersion`/`defaultVersion` runs `pgroll version` (falls back to `--version`), compares against `ExpectedPgrollVersion` (default `v0.16.2`), warns or hard-fails per `RequireExactPgrollVersion`. Called first in both workflows. |
| Reconciliation/idempotency vs pgroll state | **Done** | `ReconcileMigrationState`/`defaultReconcile` reads live pgroll status+version before each mutating step and returns `continue`/`resume_wait`/`already_complete`/`skip_complete`/`skip_rollback`/`fail`; the workflow branches on the action instead of blindly re-running `start`/`complete`/`rollback` on retry. |

All of the above is gated behind `workflow.GetVersion(ctx, "pgroll-roadmap", ...)`
in `schema_migration.go` (`roadmapEnabled`) for replay-safety against
already-running workflow histories — new executions get the full roadmap path,
in-flight ones from before this change keep their original (non-versioned)
behavior until they complete.

Notes: keep pgroll activities small/explicit; do all pgroll CLI/DB inspection in
activities, never in workflow code (determinism); treat `init`/`baseline` as
onboarding ops, not normal migration steps.

No further pgroll roadmap items are tracked here — new gaps should be added as a
fresh table/section rather than reopening the rows above.

## pgstream — roadmap gaps (status: planning backlog)

Current integration: init metadata/slot, run a long-lived stream, stop via
cancellation, poll lag, restart on anonymization-rule change. Keep this section
separate from pgroll — different lifecycle. Gaps:

| Gap | Direction |
|---|---|
| No snapshot/backfill mode | Explicit `replication`/`snapshot`/`snapshot_and_replication` mode on `StreamConfig` |
| No DDL/schema-change replication policy | `allow`/`block`/`alert_only`/`require_approval`, surfaced in status |
| No table/schema include-exclude filtering | Filters on `StreamConfig` for multi-tenant/preview scoping |
| No non-Postgres targets | Typed targets: postgres/kafka/opensearch/webhook; Postgres-to-Postgres stays default |
| No config-file generation | Render typed config to a temp file instead of growing CLI flags; golden-file tests |
| No pgstream binary/version preflight | Same shape as the pgroll one above |
| Lag visibility too narrow | Add slot/LSN/connectivity/throughput/error-count to status |
| No WAL/replication-slot guardrails | Max lag bytes/duration, max inactive slot duration, auto-escalate |
| No idempotent slot/metadata reconciliation | Inspect slot before init/run; reuse/recreate/fail |
| No dead-letter/failed-event strategy | retry/pause/skip/dead-letter/alert policy |
| No anonymization/transformer validation | Validate table/column/transformer names before restart |
| No explicit restart contract | Track restart reason/count/timestamp; rate-limit |
| No multi-target fan-out | One workflow per target, or child workflows; decide failure isolation |
| No metrics | `pgschema_pgstream_{lag_bytes,restarts_total,errors_total,events_total}` |
| No secure generated-config handling | Redact DSNs in generated configs; restrictive temp-file perms; cleanup |
| No real integration tests | Build-tagged tests against real Postgres + pgstream binary |

Notes: long-running pgstream execution stays in activities; cancellation is the
stop mechanism; preserve `ContinueAsNew` for unbounded history; never log raw DSNs
or row-level data; replication slots left abandoned retain WAL on the source.

## Fixed in past sessions (context for *why* code looks the way it does)

- **`activities/pgstream.go` `defaultGetLag`**: was a `return 0, nil` stub, now
  parses `lag_bytes` via `parseLagBytes` (unit-tested).
- **`PollLag`/`RunStream`**: heartbeat on a ticker while the long-running
  poll/exec blocks, so `HeartbeatTimeout` doesn't fire on a healthy stream.
- **`preview_db.go` `defaultClone`**: creates the target DB first, uses
  `pg_dump --format=plain` (was `--format=custom`, which `psql` can't ingest —
  silent failure), builds DSNs via `joinDBName`/`extractDBName` instead of naive
  string concat (was corrupting keyword=value DSNs).
- **`defaultApplyAnonymization`**: now actually marshals `input.Rules` into a
  pgstream transformer config and passes `--config` (previously ignored rules
  entirely).
- **`redactDSN`**: walks a quoted `password='...'` to its real closing quote
  instead of stopping at the first space (was leaking password tails).
- **`AlertActivities.DefaultWebhookURL`**: used when `AlertMessage.WebhookURL` is
  empty — every real caller leaves it empty, so without this no paging ever
  happened. Wired from `PGSCHEMA_ALERT_WEBHOOK_URL` in `main.go`.
- **Update handlers now actually wired**: `extend-wait` (`schema_migration.go`),
  `extend-ttl` (`preview_clone.go`) drive real deadline loops instead of a fixed
  timer; `update-anon-rules` (`cdc_stream.go`) triggers a `ContinueAsNew` restart
  since the external pgstream process can't hot-reload.
- **`cdc_stream.go` shutdown gap**: every exit path now cancels the run context
  before receiving on `lagDone`, so that receive can't block for the full 7-day
  `StartToCloseTimeout`. Was the most severe bug found in that pass (most of the
  CDC test suite hung on it). Also: `RunStream`'s retry `MaximumAttempts` was `0`
  (unlimited) — bounded to `20` so a permanently-failing stream's alert path is
  reachable.
- **Test-harness gotcha**: `TestWorkflowEnvironment.UpdateWorkflow(...)` queues
  its callback async — asserting on `OnAccept`/`OnComplete` in the *same*
  `RegisterDelayedCallback` body that called it reads stale zero-values. Always
  split into two callbacks (issue update in one, assert in a later one).
- **Duplication consolidated** (`activities/base.go`, `activities/dsn.go`):
  `baseActivities{ log }` embedded by all 4 activity structs (was 4x copy-pasted
  `logger()`); `redactDSN`/`baseConnStr`/`joinDBName`/`extractDBName` share one
  `isURIForm` classifier; `runCommand(ctx, name, args, opts...)` is the one place
  that shells out + wraps errors (was reimplemented 5 times). `runPgroll`/
  `runPgstream*` are thin named wrappers over it. New activity types should embed
  `baseActivities` and use `runCommand` rather than reimplementing either.
- **Structured trace/flow logging** (`baseActivities.startTrace` in `base.go`):
  every activity method and `runCommand` call emits matched `flow=start`/
  `flow=end` log lines sharing a `trace_id`, with `elapsed` on the end line and
  automatic `workflow_id`/`run_id`/`activity_id` via `activity.GetInfo` (safely
  recovered outside real activity contexts). Not OpenTelemetry — just enough
  structure for an agent/`jq` to reconstruct a run's timeline from logs alone.
  Gaps: no downstream log-query consumer exists yet; `trace_id` is per-operation,
  not per-workflow-run (no single ID threads one workflow execution end-to-end).
- **Double-run / already-applied migration** (`internal/activities/base.go`,
  `internal/activities/pgroll.go`, `internal/workflow/schema_migration.go`):
  re-running an already-completed scenario (double click, or re-clicking
  "Run" after it finished) used to fail. Two distinct shapes were found —
  fixed in two passes, worth knowing both happened:
  1. *Add/create ops* (`column "email" already exists`): the workflow tried
     to compensate via `pgroll rollback`, which *also* errored (`unable to
     get active migration: no active migration`) since nothing was actually
     started — paging the operator at "critical" and leaving the workflow
     `rollback_failed`. First fix: `IsAlreadyAppliedError`/
     `IsNoActiveMigrationError` classify pgroll's CLI error text (see the SDK
     section above for why this is text-matched at all), and
     `before_rollback` only attempts a real rollback when status is
     genuinely `"in progress"`.
  2. *Rename/drop ops* (`column "full_name" does not exist on table
     "users"` — the same double-run, but the FLIP side: the old name is
     gone because the first run already renamed it away): text-matching
     `"already exists"` doesn't catch this, and broadening to also match
     `"does not exist"` would be actively wrong — that same error text is
     also what a genuinely different, broken migration produces (e.g.
     clicking a later demo scenario before its prerequisite ran; see
     Troubleshooting in `demo/README.md`), and swallowing THAT as "already
     applied, nothing to do" would silently hide a real failure while
     reporting success.

  The real fix for (2), which also subsumes (1) as its primary path now:
  make pgroll's own tracked migration name **content-addressable** instead
  of random. `runPgroll` (`base.go`) used to write the migration JSON to
  `os.CreateTemp("", "pgroll-migration-*.json")` — a random suffix, because
  pgroll v0.16.2 rejects an explicit top-level `"name"` field in the
  document itself and instead derives a migration's tracked name from the
  temp file's basename. `MigrationFileName(migrationJSON)` (`base.go`) hashes
  the content instead (`sha256`, truncated), written into a fresh
  `os.MkdirTemp` dir each call (unique path per invocation, so two truly
  concurrent runs of identical content can't race on the same file — the
  directory doesn't affect the name pgroll derives, only the basename does).
  `migrationIdentity` (`pgroll.go`) recomputes the same hash from
  `MigrationInput.MigrationJSON` to compare against pgroll's reported
  current version in `reconcileDecision` — this is what `matchesCurrent` was
  *originally* meant to do; it just never worked because nothing produced a
  real, comparable name before. `SchemaMigrationWorkflow` now checks
  `reconcile("before_start")` **before** `ValidateMigration` (previously
  validate always ran first) — pgroll reporting `"Complete"` with a matching
  content hash is a reliable "this exact migration already fully applied"
  signal, so a double-run short-circuits to `completed` before validate gets
  a chance to fail on either shape of already-changed schema, for any
  operation type — no error-text matching needed for the primary path
  anymore. A DIFFERENT migration against the same `"Complete"` status
  doesn't match and falls through to validate normally, so it still fails
  for real (verified — see tests below). `IsAlreadyAppliedError` stays as a
  defense-in-depth fallback inside the `shouldStart` branch (fires only if
  reconcile itself errored, or on a pre-roadmap replay where reconcile is
  skipped entirely) — `ValidateMigration`/`StartMigration` still return a
  non-retryable error for that shape so it fails fast instead of burning the
  5-attempt/~30s backoff.

  `reconcileDecision` stayed split out as a pure function for unit testing
  without a live pgroll/DB — see `TestReconcileDecision_*` and
  `TestMigrationIdentity_DeterministicAndDistinct` in
  `internal/activities/pgroll_test.go`. Real-binary end-to-end coverage in
  `test/integration/pgroll_double_run_test.go` (build tag `integration`,
  testcontainers Postgres): `..._RunTwice_SecondRunIsIdempotent` (add-column
  shape), `..._RunTwice_RenameIsAlsoIdempotent` (rename shape),
  `..._DifferentMigrationAfterComplete_StillFails` (the false-positive
  guard — a distinct, invalid migration against a `"Complete"` schema must
  still fail, not be swallowed).
- **No raw pgroll detail in presenter-facing messages**: the friendly-skip
  paths above (`friendlyAlreadyAppliedMessage` in `schema_migration.go`) log
  the full pgroll error text at `WARN` tagged `ref_id=<workflowID>` and return
  only `"...nothing to do. (ref: <workflowID>)"` to `progress.Message` — the
  workflow ID doubles as the searchable ref since the demo UI already
  displays it. Don't reintroduce `err.Error()`/`cause.Error()` directly into
  `progress.Message` for these paths; grep `ref_id=<id>` in worker logs
  instead. Covered by
  `TestValidationFailure_AlreadyApplied_FriendlyMessage`
  (`internal/workflow/schema_migration_test.go`).

## Real pgroll v0.16.2 CLI bugs found + fixed (discovered by running it, not by docs)

pgroll's actual CLI has drifted from common examples. All confirmed against the
real binary (`pgroll --help`, and reading `pkg/mod/.../pgroll@v0.16.2/cmd/*.go`):

- Flag is `--postgres-url`, not `--dsn` — every pgroll invocation was previously a
  CLI parse error. Fixed in `runPgroll`/`runPgrollOutput` (`base.go`) and the
  `migrate-*` mise tasks.
- `validate`/`start` take the migration file as a positional `<file>` argument,
  **not stdin**. `runPgroll` now writes `migrationJSON` to a temp file and passes
  its path.
- `status` has no `--output`/`--json` flag — it always prints JSON. Dropped the
  fake flag; `defaultStatus` now goes through `runPgrollOutput` (shared
  `runCommand` + tracing) instead of a bespoke `exec.CommandContext` block.
- `go install github.com/xataio/pgroll/cmd/pgroll@...` / `.../pgstream/cmd/
  pgstream@...` 404 — both modules put `main()` at the module root, not under
  `cmd/<name>`. Fixed `install-tools` to install the module root path.
- Brownfield adoption needs `pgroll baseline <version> <dir>` (not just `init`)
  before any migration will run against a schema pgroll didn't create — pgroll
  errors with "non-empty but has no migration history" otherwise. `baseline`
  requires a pre-existing target dir and writes a placeholder file there (for
  manual `pg_dump` completion, per pgroll docs) — content unused by pgschema, only
  the DB-side state it records matters. Demo points this at gitignored
  `.data/pgroll-baseline/`.
- Known, not fixed: `types.MigrationStatus.Name`/`StartedAt` don't exist in real
  pgroll `status` JSON (`{schema, version, status}`) — always zero-valued. Only
  `Status`/`Schema` are real. Harmless today since `schema_migration.go` only
  checks `Status != "Complete"`, but don't assume `Name`/`StartedAt` are wired up.

Validated end-to-end for real: all 6 demo migrations run start→complete against
live Postgres 18 (see `demo/`), plus the exact `PgrollActivities` code path
(`ValidateMigration`→`StartMigration`→`GetMigrationStatus`→`CompleteMigration`)
exercised directly against a live DB in a throwaway test.

## pgroll has a real Go SDK — CLI shelling is not the only option (found 2026-08-25)

`internal/activities/pgroll.go` drives pgroll entirely via `exec.Command` +
temp files (`runPgroll` in `base.go`), then classifies failures by
`strings.Contains(strings.ToLower(err.Error()), ...)` against CLI
stdout/stderr text (`IsAlreadyAppliedError`, `IsNoActiveMigrationError`, both
in `pgroll.go`). That works but is inherently flaky: CLI wording can change
across pgroll releases, gets ANSI-color-coded in a real terminal, and ANY
matching detail (a temp-file path, an internal error format) has to be
scraped from a flat string.

Investigated whether pgroll exposes a library API instead. It does — the
same module installed via `go install github.com/xataio/pgroll@v0.16.2` has
a full in-process Go SDK under `pkg/`, Apache-2.0, Go 1.26.3 module (matches
this repo's toolchain), no CGO observed:

```go
pkg/roll/roll.go:      func New(ctx, pgURL, schema string, state *state.State, opts ...Option) (*Roll, error)
pkg/roll/roll.go:      func (m *Roll) Init(ctx) error
pkg/roll/baseline.go:  func (m *Roll) CreateBaseline(ctx, baselineVersion string) error
pkg/roll/execute.go:   func (m *Roll) Validate(ctx, migration *migrations.Migration) error
pkg/roll/execute.go:   func (m *Roll) Start(ctx, migration *migrations.Migration, cfg *backfill.Config) error
pkg/roll/execute.go:   func (m *Roll) Complete(ctx) error
pkg/roll/execute.go:   func (m *Roll) Rollback(ctx) error
pkg/roll/status.go:    func (m *Roll) Status(ctx, schema string) (*Status, error)
```

Calling these directly instead of shelling out would replace string-matching
with real typed errors:

- `pkg/migrations/errors.go` exports structured, `errors.As`-able types per
  failure kind: `TableAlreadyExistsError{Name}`, `ColumnAlreadyExistsError{Table,Name}`,
  `IndexAlreadyExistsError{Name}` (and the inverse `*DoesNotExistError`
  family) — exactly the "already applied" class this codebase currently
  detects by substring.
- `pkg/state/errors.go` exports `var ErrNoActiveMigration = errors.New("no
  active migration")` as an `errors.Is`-able sentinel. `Roll.Rollback()`
  (`pkg/roll/execute.go:258`) wraps it with `%w` all the way up
  (`state.GetActiveMigration` returns it on `sql.ErrNoRows`, `Rollback`
  wraps as `fmt.Errorf("unable to get active migration: %w", err)`) —
  exactly the "no active migration" class this codebase also currently
  detects by substring.

**Why the CLI path can never get this**: `exec.Command` + stdout/stderr
necessarily discards Go type information — cobra's error printing just
calls `.Error()`. String-matching is the only option *while shelling out*;
it is not a shortcut that could be tightened later without the switch below.

**Cost of switching — nontrivial, own refactor, not a small patch**:
`pkg/roll.New` needs a `*state.State` (its own constructor + connection
setup, separate from `Roll`), and `Roll.Validate`/`.Start` take a typed
`*migrations.Migration`, not a JSON string — so this project's
`MigrationJSON string` field on `types.MigrationInput` and the whole
exec/temp-file plumbing in `activities/base.go` (`runPgroll`,
`runPgrollOutput`, `runCommand`'s pgroll-specific callers) would need
replacing with library calls, with our JSON unmarshaled into
`migrations.Migration` instead of written to a file. `PgrollActivities`'
`ValidateFn`/`StartFn`/etc. func-field pattern for test injection would
still work, just wrapping library calls instead of CLI calls, and
`IsAlreadyAppliedError`/`IsNoActiveMigrationError` would become `errors.As`/
`errors.Is` checks against the real pgroll error types instead of substring
matches — the same call sites (`internal/workflow/schema_migration.go`'s
validate short-circuit and `triggerRollback`'s before_rollback skip) stay,
just with a more precise/robust classification underneath.

Not attempted yet — flagged for a future dedicated pass, not bundled into
the double-run fix below.

## Local dev loop (`mise.toml`, `.air*.toml`, `Procfile`)

- `[env]` in `mise.toml`: `_.file = ".env"` loads `pgschema/.env` (copy from
  `.env.sample`) into every task/mise-activated shell; `_.path` prepends
  `~/go/bin` so `go install`-ed binaries (pgroll, pgstream) are found.
- `air` (`.air.toml` for the worker, `.air-demo.toml` for the demo web UI)
  rebuilds+restarts on `.go`/`.json` changes. Field names verified against
  `air init`'s generated default config (don't trust memory for air's schema,
  verify against `air init` output if unsure). Key fields for clean shutdown:
  `send_interrupt = true` (send SIGINT to the child, not just kill it — air
  treats SIGINT and SIGTERM the same for its own shutdown, which matters because
  overmind's default stop signal is SIGTERM), `kill_delay` (grace period before
  hard kill), `misc.clean_on_exit = true` (removes `tmp/` build output). Don't
  set `full_bin` unless it needs to differ from `bin` — leaving it equal to `bin`
  is a no-op, just noise.
- `overmind` (`Procfile`) runs postgres + temporal + worker + demo web together:
  `mise run dev`. The `postgres` line is `sh -c 'trap "docker compose ... down"
  EXIT; docker compose ... up'` — guarantees the container+network are actually
  torn down (not just left stopped) on any exit (Ctrl-C, `overmind quit`,
  SIGTERM), while the *named volume* survives (down has no `-v`), so baselined
  demo data persists across `mise run dev` restarts. Verified for real: sent
  SIGTERM to the wrapped process, confirmed container+network removed within
  ~2s, then confirmed the volume/data survived a subsequent `docker compose up`.
  `mise run dev` passes `overmind start --timeout 15` (default is 5s) so Docker
  has enough grace period to actually finish before a hard SIGKILL.
- `mise run doctor` checks air/overmind/go/temporal/docker/pgroll/pgstream/`.env`
  presence with fix hints. `temporal` itself is assumed pre-installed standalone
  (not a mise tool).
- `cmd/pgschema/main.go`'s `client.Dial` reads `TEMPORAL_ADDRESS`/
  `TEMPORAL_NAMESPACE` from env (empty keeps SDK defaults).

**Sandbox quirk (this agent environment specifically, not pgschema itself)**:
backgrounding a long-running Go daemon (`go run ./cmd/pgschema`, plain `air`,
`temporal server start-dev`, etc.) via `&`/`disown`, `nohup`, or the Bash tool's
`run_in_background` was observed getting reaped within ~3-5s regardless of
method, logging a clean start-then-immediate-stop with no error — looks exactly
like the process received SIGINT/SIGTERM instantly. A genuine OS-level daemon
(`docker compose up` under a real docker daemon) was *not* affected and ran
fine in the background. If you need to verify a long-running Go process's
behavior in this environment and backgrounding it gets silently killed, don't
fight it — instead exercise the exact code path directly (e.g. a throwaway
`_test.go` in the target package calling the real functions against a live
dependency) rather than trying to keep a full server alive across tool calls.

## pgroll workflow demo (`demo/`, `cmd/pgschema-demo/`)

`demo/README.md` is the presenter walkthrough — read that first for the full
script. `demo/docker-compose.yml` runs throwaway Postgres 18 (note: PG18's
official image changed its volume convention — mount `/var/lib/postgresql`, the
parent dir, not `.../data`, or the container refuses to start with "data in
/var/lib/postgresql/data (unused mount/volume)"), seeded via plain SQL
(`demo/init/`) with a brownfield `users` table. `mise run demo-reset` baselines
it into pgroll; `demo/migrations/*.json` holds 6 real pgroll migrations, basic →
complex, ending in a Postgres-18 `uuidv7()` bonus (all validated live, see
above).

`cmd/pgschema-demo` is a standalone web server (stdlib `net/http` +
[Datastar](https://data-star.dev) v1.0.2 — verified there is no released v2, the
"v2-style" `datastar-patch-*` wire protocol is what v1.0.2 actually ships,
CDN-loaded, no JS build step) that starts a real `SchemaMigrationWorkflow` per
scenario click and streams `migration-progress` + phase transitions back over
SSE, with buttons for the real `app-ready`/`rollback` signals. Only imports
`internal/workflow`/`internal/types` — no activity/workflow logic duplicated.
SSE wire-format exactness is pinned by `TestSSEWireFormat`
(`cmd/pgschema-demo/main_test.go`); template rendering by `TestIndexRenders`,
`TestScenarioPageRenders`, `TestScenarioPageLastHasNoNextLink`,
`TestScenarioPageUnknownID`.

### Demo UX overhaul (2026-08-25/26): per-scenario pages + live version panel

The demo used to be one page: a scenario list, a shared "live workflow state"
box, and an activity log. It's now two page types, still zero-build-step
Datastar + `html/template`:

- **`template.go`**: `commonCSS` (shared, compile-time-concatenated Go string
  const) + `landingHTML` (`/` — just scenario cards linking out, plus the
  reset button) + `scenarioHTML` (`/scenario/{id}` — plan panel on top,
  Activity Log bottom-left, Live Workflow State bottom-right, a custom
  `div`-based progress bar + step-dot row instead of `<progress>`, a
  `data-show` CSS spinner during preflight/validate). Each scenario page also
  links to `/` (back) and, via `nextScenario(id)` in `main.go`, to the next
  scenario in the `scenarios` slice (omitted on the last one) — presenter
  convenience for stepping through 1→2→3→…→6 without returning to the
  landing page each time.
- **`plan.go`** (new): parses the scenario's pgroll migration JSON into
  `renderPlanDiff` (+/- lines, scoped to the op shapes this demo's 7 files
  actually use: `add_column`, `rename_column`, `alter_column`/unique) and
  `renderPlanGraph` (op nodes in a flow, highlight driven purely by
  `data-class` against `$phase`/`$percent` — no server re-render needed as
  the run progresses, since the plan itself never changes mid-run).
- **`versions.go`** (new) + a **key finding**: pgroll's expand/contract model
  is real, queryable Postgres state, not something the demo needs to
  simulate. Each version lives as an actual schema named
  `<schema>_<version>` (`progress.LatestSchema` was already one of these);
  old and new coexist during expand, `complete` drops the old one, `rollback`
  drops the new one. So the whole "old vs. new schema, side by side,
  selectable, greys out once cleaned up" panel is driven by plain
  `information_schema.schemata`/`.columns` queries via short-lived
  `pgx.Connect` calls (same pattern `resetDatabase` already used) — **no new
  Temporal activities/queries were added**, and `internal/workflow`/
  `internal/activities`/`internal/types` are untouched by this whole pass.
  `run.SeenVersions` (union of every versioned schema ever observed live)
  vs. a fresh `listVersionedSchemas` result is what distinguishes "greyed
  out, cleaned up" from "currently live" — not any client-side/optimistic
  state. `streamProgress`'s existing 700ms ticker also refreshes the version
  panel each tick (`gatherVersionSnapshot` + `patchElements(..., "outer",
  ...)`) once `percent >= 5`, rather than opening a second SSE stream.
  Backfill scenarios (2, 5, 6 — `scenario.BackfillColumn`) get a concrete
  before/after row-coverage line from the same snapshot.
  "Switching" a version via `POST /versions/{schema}/activate` only changes
  which schema the panel *previews* (read-only) — committing/aborting the
  real migration is still exactly the pre-existing `app-ready`/`rollback`
  signals; the two are intentionally not conflated.

**Gotcha hit while verifying this pass — read before assuming "it's not
working"**: `.gitignore` had a bare `pgschema-demo` line meant to ignore the
built binary. Because the built binary and the *source directory*
(`cmd/pgschema-demo/`) share the same name, that pattern also matched the
directory and silently hid new files under it from `git status`/`git add`
(existing tracked files like `main.go` still showed as modified — only
newly-created files like `plan.go`/`versions.go` vanished). Fixed by scoping
the pattern to the actual binary path
(`pgschema/cmd/pgschema-demo/pgschema-demo`). If a new file in this directory
isn't showing up in `git status`, check `git check-ignore -v <path>` before
assuming you forgot to create it.

**Gotcha hit while verifying this pass, #2 — stale orphaned dev server**:
after editing demo source under an already-running `mise run dev`
(`air`-supervised), the browser kept showing old behavior even though the
binary rebuilt successfully. Cause: an *orphaned* `pgschema-demo-dev`
process from an earlier, already-exited `overmind`/`air` session (`ps -o
ppid` showed `1`, i.e. reparented to launchd) was still bound to `:8090` and
serving stale code; the current session's freshly-built process couldn't
bind the port at all. Diagnosis: `lsof -i :8090` for the PID, then `ps -o
pid,ppid,command -p <pid>` — a demo server whose parent isn't the current
`air`/`overmind` tree is the tell. Fix was just `kill <stale-pid>`; the
live session's process (or a user-initiated `mise run dev` restart) then
bound the now-free port immediately. Worth checking this first any time "the
UI still shows old behavior after a rebuild" during this kind of live dev-loop
debugging session.

## Misc

- This session's environment auto-redacts literal secret-shaped substrings
  (e.g. `scheme://user:pass@host`) in tool-written file content — a test literal
  containing `://` can silently come out as `******` with the marker gone,
  breaking string-form-detection logic under test. If a DSN-parsing test fails
  in a way that looks like the wrong branch was taken, `grep` the actual file
  content for the literal you meant to write before assuming the parsing code
  is wrong.

## Cleanup / refactor backlog

**Status: all items completed 2026-08-24.**

### 1. ~~Remove `PollLag` activity~~ — DONE

`CDCStreamWorkflow` (`internal/workflow/cdc_stream.go`) has an inline goroutine
that calls `GetStreamHealth` on a timer and enforces guardrails (MaxLagBytes,
MaxConsecutivePollFailures, OnViolation). `PollLag` is a separate Temporal
activity in `internal/activities/pgstream.go` that re-implements the same logic
independently. The workflow **never dispatches `PollLag`** — it is registered
(because `RegisterActivity(pgstreamActs)` registers all methods on the struct)
but never called via `workflow.ExecuteActivity`. Safe to delete:

- `func (a *PgstreamActivities) PollLag(...)` (~55 lines)
- `PollLagFn` field on `PgstreamActivities`
- `PollLag` tests in `internal/activities/pgstream_test.go`
  (`TestPollLag_TicksAndStops`, `TestPollLag_ErrorsTolerated`,
  `TestPollLag_Heartbeats`) — all test an activity the workflow never calls

### 2. ~~Remove `GetLag` activity~~ — DONE

`GetLag` delegates entirely to `defaultGetLag → defaultGetHealth`, returning only
`health.LagBytes`. It is never dispatched by the workflow; it exists only to
support `PollLag`. Remove:

- `func (a *PgstreamActivities) GetLag(...)` (~12 lines)
- `GetLagFn` field on `PgstreamActivities`
- `defaultGetLag` private function (~8 lines)
- `GetLag` tests in `internal/activities/pgstream_test.go`
  (`TestGetLag_Success`, `TestGetLag_Error`)

Keep `defaultGetHealth` — it is used by `GetStreamHealth`, which the workflow
does call.

### 3. ~~Fix stale comments in `cdc_stream_test.go`~~ — DONE

Lines 44 and 355 say "Lag is populated by the PollLag goroutine" / "recent
PollLag activity result". The workflow goroutine actually calls `GetStreamHealth`
(not `PollLag`). Update comments to match the real mechanism.

### 4. ~~Export / deduplicate `redactDSN`~~ — DONE

`cmd/pgschema-demo/main.go` contains `redactForLog`, a local copy of
`internal/activities.redactDSN`. The comment in `main.go` acknowledges the
duplication. Simplest fix: export `RedactDSN` from the `activities` package (or
move to `internal/dsn`) and use it from `cmd/pgschema-demo/main.go`.

### 5. `QueryStreamLag` convenience query (DEFERRED — intentional)

`"lag"` query returns a subset of `"health"`. Not wrong; keep unless the API
surface is being deliberately narrowed. No action required.

---

**Validation after cleanup** (from `pgschema/`):
```
go vet ./...
go test ./...
go build ./...
```
All three must stay green. `go test -race ./...` preferred if time permits.
