# pgschema — notes for agents

Temporal-based schema migration / CDC-stream / preview-clone package wrapping the
`pgroll` and `pgstream` CLIs. Package layout: `internal/activities` (CLI wrappers),
`internal/workflow` (Temporal workflows), `internal/types`, `cmd/pgschema` (worker
entrypoint), `cmd/pgschema-demo` (demo web UI), `demo/` (docker-compose + pgroll
migration fixtures for the demo).

`go build ./...`, `go vet ./...`, `go test ./... -race` are green as of this pass.
No CLAUDE.md exists at repo root or under `pgschema/` — this file is the only
agent-facing guidance here.

## pgroll — roadmap gaps (status: planning backlog)

Current integration covers validate/start/complete/rollback/status. Gaps below are
real backlog, not yet implemented:

| Gap | Why it matters | Direction |
|---|---|---|
| No `pgroll init`/readiness check | First-time runs fail late if pgroll metadata is missing | Preflight activity; fail fast or gate behind `AllowInitialize` |
| No `baseline` flow | Existing (brownfield) DBs need adoption without replaying history | Wrap `pgroll baseline`; log operator/timestamp. **Demo now exercises this at the CLI level (`mise run demo-init`) but no activity/workflow wraps it yet** |
| No `latest schema` integration | App rollout needs the versioned schema name after `start` | Activity for `pgroll latest schema`; surface via progress query |
| Status only checked after completion | Can't compare Temporal phase vs pgroll DB state mid-flight | Query status at each phase boundary; store last-observed in progress |
| No operation-level risk/policy analysis | Raw SQL, renames, destructive ops all treated equally | Parse migration JSON pre-`validate`; classify risk; configurable block/approve gates |
| No pgroll binary/version preflight | Missing/wrong binary fails only when an activity runs | `pgroll version` check at startup; compare to pinned version |
| No reconciliation/idempotency vs pgroll state | Crash/retry can diverge Temporal state from DB state | Read pgroll status before each mutating step; no-op/continue/rollback/fail accordingly |

Notes: keep pgroll activities small/explicit; do all pgroll CLI/DB inspection in
activities, never in workflow code (determinism); treat `init`/`baseline` as
onboarding ops, not normal migration steps.

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
(`cmd/pgschema-demo/main_test.go`); template rendering by `TestIndexRenders`.

## Misc

- This session's environment auto-redacts literal secret-shaped substrings
  (e.g. `scheme://user:pass@host`) in tool-written file content — a test literal
  containing `://` can silently come out as `******` with the marker gone,
  breaking string-form-detection logic under test. If a DSN-parsing test fails
  in a way that looks like the wrong branch was taken, `grep` the actual file
  content for the literal you meant to write before assuming the parsing code
  is wrong.

## Cleanup / refactor backlog

Identified in session 2026-08-24. Listed by priority.

### 1. Remove `PollLag` activity — dead code, duplicates workflow guardrail logic (HIGH)

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

### 2. Remove `GetLag` activity — thin wrapper, dead code (HIGH)

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

### 3. Fix stale comments in `cdc_stream_test.go` (LOW)

Lines 44 and 355 say "Lag is populated by the PollLag goroutine" / "recent
PollLag activity result". The workflow goroutine actually calls `GetStreamHealth`
(not `PollLag`). Update comments to match the real mechanism.

### 4. Export / deduplicate `redactDSN` (LOW)

`cmd/pgschema-demo/main.go` contains `redactForLog`, a local copy of
`internal/activities.redactDSN`. The comment in `main.go` acknowledges the
duplication. Simplest fix: export `RedactDSN` from the `activities` package (or
move to `internal/dsn`) and use it from `cmd/pgschema-demo/main.go`.

### 5. `QueryStreamLag` convenience query (DEFER)

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
