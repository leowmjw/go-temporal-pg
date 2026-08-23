# pgschema — notes for agents

Temporal-based schema migration / CDC-stream / preview-clone package wrapping the
`pgroll` and `pgstream` CLIs. Package layout: `internal/activities` (CLI wrappers),
`internal/workflow` (Temporal workflows), `internal/types`, `cmd/pgschema` (worker entrypoint).

## pgroll

Status: **planning backlog**. The current pgroll integration covers the core migration lifecycle: validate, start, complete, rollback, and status. The gaps below capture additional pgroll capabilities and operational hardening that are not yet fully leveraged.

### Main gaps

| Gap | Why it matters | Suggested implementation direction | Acceptance criteria |
|---|---|---|---|
| No `pgroll init` / readiness check | Migrations assume pgroll has already been initialized in the target database. First-time runs can fail late if pgroll metadata is missing. | Add a preflight activity that verifies pgroll metadata exists for the configured database/schema. If missing, either fail with a clear message or optionally run `pgroll init` behind an explicit input flag such as `AllowInitialize`. | Workflow fails early with an actionable error when pgroll is not initialized. Optional init path is idempotent and covered by tests. |
| No `baseline` flow | Existing production databases need a safe adoption path without replaying historical migrations. | Add a brownfield onboarding workflow/activity around `pgroll baseline`. Capture baseline name, migrations directory/path, schema, operator identity, and timestamp in logs/audit output. | A database with existing tables can be registered as a pgroll-managed schema without applying DDL. Tests cover successful baseline and already-baselined behavior. |
| No `latest schema` integration | Application rollout needs to know which versioned schema to connect to during the expand phase. Today this appears to be handled outside the workflow. | Add an activity that runs `pgroll latest schema` and exposes the result through workflow progress/query output. Use it after `start` so deployment tooling can set `search_path` or equivalent app configuration. | After `StartMigration`, workflow progress includes the latest pgroll schema name. Tests verify it is available before waiting for app readiness. |
| Status is only used after completion | Operators and automation cannot continuously compare Temporal’s phase with pgroll’s actual database state during the critical rollout window. | Poll or query pgroll status at phase boundaries: before start, after start, while waiting for app readiness, before complete, after complete, and after rollback. Store last observed pgroll status in progress response. | Progress query reports both Temporal phase and pgroll status/version. Workflow detects unexpected state and fails or escalates with a clear reason. |
| No operation-level policy/risk analysis | All migration JSON is treated as equivalent. Risky changes such as raw SQL, renames, constraints, defaults, or large-table operations may need approval or scheduling controls. | Parse migration JSON before `validate`. Classify operations into risk levels. Add policy checks for raw SQL, renames, constraints, backfills/defaults, destructive operations, and operations targeting protected schemas/tables. | Validation returns a structured risk report. Configurable policy can block or require approval for high-risk operations. Unit tests cover representative pgroll operations. |
| No runtime pgroll binary/version check | A missing or unexpected pgroll binary fails only when an activity executes. Different pgroll versions may support different operations or CLI flags. | Add startup or preflight check that runs `pgroll version` or equivalent. Compare against the expected pinned version from tooling. Include the observed version in logs and workflow metadata. | Worker/preflight fails fast if pgroll is missing. Version mismatch is visible and optionally fatal depending on configuration. |
| No reconciliation/idempotency against pgroll state | Manual pgroll commands, worker crashes, retries, or partially completed activities can cause Temporal state and database state to diverge. | Add a reconciliation function that reads pgroll status before each mutating step and decides whether to no-op, continue, rollback, or fail. Keep command execution idempotent where possible. | Restarted workflows can resume safely from known pgroll states. Tests cover already-started, already-completed, rolled-back, and unknown/divergent states. |

### Suggested implementation order

1. **Preflight and version checks**
   - Add pgroll binary existence/version check.
   - Add pgroll metadata readiness check.
   - Fail fast with actionable errors.

2. **Status model expansion**
   - Extend migration progress to include pgroll status/version/latest schema.
   - Query status at every phase boundary.
   - Add reconciliation helpers.

3. **Latest schema support**
   - Add `latest schema` activity.
   - Surface latest schema through workflow query output.
   - Document how deployment tooling should consume it.

4. **Brownfield onboarding**
   - Add `baseline` activity/workflow.
   - Add tests against an existing schema.
   - Make onboarding explicit and separate from normal migration execution.

5. **Policy/risk analysis**
   - Parse migration JSON.
   - Classify operation types and risk levels.
   - Add configurable policy gates and approval hooks.

6. **Operational hardening**
   - Add audit-friendly structured logs for pgroll command, schema, migration name, status, version, and duration.
   - Redact DSNs and secrets from all logs/errors.
   - Add tests for command failures and malformed pgroll output.

### Notes for next agent

- Keep pgroll lifecycle activities small and explicit. Prefer adding new activity methods rather than overloading existing validate/start/complete behavior.
- Preserve Temporal determinism: do all pgroll CLI/database inspection inside activities, not directly in workflow code.
- Treat `pgroll init` and `pgroll baseline` as onboarding/setup operations, not normal migration steps, unless explicitly requested by input.
- The workflow should not assume that Temporal state is the source of truth after a retry or restart. Always reconcile against pgroll status before mutating.
- For app rollout integration, the most useful missing artifact is the latest versioned schema name after `start`.
- For safety, policy analysis should happen before `pgroll validate`/`start`, so blocked migrations fail before any DDL is attempted.
- pgstream roadmap items should be added separately under a future `## pgstream` section.

## pgstream

Status: **planning backlog**. Keep this section separate from `## pgroll`. The pgroll roadmap covers zero-downtime schema migration. This section covers CDC, replication, snapshotting, anonymization, and downstream streaming capabilities via pgstream.

The current pgstream integration covers a basic CDC lifecycle:

- initialize pgstream metadata / replication slot
- run a long-lived stream
- stop via Temporal cancellation
- poll replication lag
- restart the workflow when anonymization rules change

The gaps below capture broader pgstream capabilities that are not yet fully leveraged.

### Main gaps

| Gap | Why it matters | Suggested implementation direction | Acceptance criteria |
|---|---|---|---|
| No explicit snapshot / backfill mode support | CDC pipelines usually need an initial copy of existing data before streaming new changes. Without first-class snapshot support, targets may start incomplete unless manually seeded. | Extend stream configuration with an explicit mode: `replication`, `snapshot`, or `snapshot_and_replication`. Add fields for snapshot scope, table filters, repeatability, batch sizing, and snapshot-only execution. | Workflow can run snapshot-only and snapshot-then-replication pipelines. Tests cover empty target, existing target, and snapshot failure before streaming begins. |
| No first-class DDL / schema-change replication policy | pgstream can support schema-change-aware replication, but production systems need control over which schema changes are allowed to propagate automatically. | Add schema-change policy configuration: `allow`, `block`, `alert_only`, or `require_approval`. Surface DDL/schema events in workflow status and alerts. | DDL events are visible to operators. Configurable policy can block or alert on risky schema changes. Tests cover allowed and blocked schema-change events. |
| No table / schema / object include-exclude filtering | Many CDC pipelines should replicate only selected data, especially for multi-tenant, privacy, cost, or preview-environment scenarios. | Add filters to stream configuration: included schemas, excluded schemas, included tables, excluded tables, object types, and glob/pattern support where pgstream supports it. | A stream can be scoped to specific tables/schemas. Invalid or conflicting filters fail validation before starting pgstream. |
| No non-Postgres target support | Current usage is Postgres-to-Postgres focused, but pgstream can be useful for Kafka, Elasticsearch/OpenSearch, webhooks, search indexing, eventing, and fan-out pipelines. | Generalize target configuration with typed targets: `postgres`, `kafka`, `opensearch`, `elasticsearch`, and `webhook`. Keep Postgres-to-Postgres as the first supported production path while designing the model for extension. | Existing Postgres-to-Postgres behavior remains compatible. Configuration can represent at least one additional target type without changing workflow signatures again. |
| No generated config-file workflow | Advanced pgstream use cases are easier to express and test via config files than via a growing list of CLI flags. | Add a pgstream config renderer that turns typed stream configuration into a temporary config file. Unit-test rendering independently from process execution. | Config rendering has golden-file tests. New pgstream options can be added by extending config structs instead of hand-building many ad-hoc CLI flags. |
| No pgstream binary / version preflight | A missing or wrong pgstream binary will fail late inside the workflow. Version mismatches can change supported flags and config syntax. | Add a preflight activity that checks pgstream is installed and records its version. Compare against the expected pinned version from tooling. Support strict and warning-only modes. | Workflow fails fast with an actionable error if pgstream is unavailable. Version is included in structured logs/status. Optional strict mode fails on mismatch. |
| Lag visibility is too narrow | A single lag value is not enough for production operations. Operators also need source health, target health, slot status, LSN position, last event timestamp, error counts, and throughput. | Expand stream status to include lag bytes, slot name, current LSN, last processed event time, source connectivity, target connectivity, batch counts, retry counts, and recent errors where available. | Workflow query returns a richer CDC status object. Alerts distinguish between lag, source failure, target failure, and stalled replication. |
| No WAL / replication-slot safety guardrails | Stalled replication slots can retain WAL indefinitely and create source database risk. | Add configurable guardrails: max lag bytes, max lag duration, max inactive slot duration, and optional automatic escalation/stop behavior. | Workflow escalates or stops when thresholds are exceeded. Tests cover lag threshold breach, recovery, and repeated polling failures. |
| No idempotent slot / metadata reconciliation | Existing replication slots, stale pgstream metadata, or partial previous runs can cause startup conflicts. | Add reconciliation before init/run: inspect pgstream metadata, check replication slot existence, verify slot ownership, and decide whether to reuse, recreate, or fail. | Restarted workflows handle already-initialized, stale-slot, and conflicting-slot cases predictably. |
| No dead-letter / failed-event strategy | CDC streams need a plan for events that cannot be applied downstream due to constraints, schema mismatch, serialization errors, webhook failures, or target outages. | Add a failure policy: retry, pause, skip with audit, write to dead-letter target, or alert. Surface failed-event metadata safely without leaking secrets or sensitive row values. | Target apply failures produce deterministic workflow behavior. Operators can see why the stream paused/failed and where rejected events were recorded. |
| No deep anonymization / transformation validation | Runtime anonymization updates are accepted structurally, but rules should be validated against source schema and supported transformer names/options before restarting a stream. | Add validation for table existence, column existence, transformer names, transformer options, duplicate/conflicting rules, and protected columns. | Invalid anonymization updates are rejected before stream restart. Tests cover unknown table, unknown column, unknown transformer, duplicate rules, and protected-field violations. |
| No explicit restart contract for config updates | Updating anonymization rules requires a restart. That behavior should be visible and controlled rather than implicit. | Track restart reason, restart count, last restart timestamp, and restart initiator. Add restart rate limits. If pgstream later supports hot reload, hide that behind a reload/restart abstraction. | Workflow status shows when the stream restarted and why. Excessive restarts trigger alerting or rejection. |
| No multi-target fan-out orchestration | A single source may need to feed multiple downstream systems, each with independent failure behavior. | Support either one workflow per target or one workflow coordinating child workflows per target. Decide whether target failures are isolated or fatal to the whole stream. | One source can replicate to multiple configured targets with independent status and failure policy per target. |
| No operational metrics integration | Temporal lag queries are useful, but production monitoring should not require querying Temporal directly. | Emit metrics such as `pgschema_pgstream_lag_bytes`, `pgschema_pgstream_restarts_total`, `pgschema_pgstream_errors_total`, `pgschema_pgstream_events_total`, and `pgschema_pgstream_target_latency_ms`. | Metrics are emitted with safe labels such as stream ID, source name, and target type. Secrets are never exposed in labels or logs. |
| No secure generated-config handling | Source and target URLs can contain credentials. Generated config files and command errors can leak secrets if not handled carefully. | Add shared DSN/config redaction. Prefer environment-variable or secret-reference interpolation where possible. Ensure temporary config files use restrictive permissions and are deleted after use. | No logs/errors expose credentials. Temp config files are created with restrictive permissions and cleaned up. Tests cover DSN redaction and config cleanup. |
| No real pgstream integration tests for advanced modes | Unit tests validate workflow behavior, but snapshotting, replication, schema changes, and transformations need real Postgres/pgstream coverage. | Add build-tagged integration tests using local Postgres containers and the real pgstream binary. Start with Postgres-to-Postgres snapshot-and-replication. Add DDL and anonymization tests later. | Integration test proves initial copy, ongoing DML replication, cancellation, restart, and at least one schema-change path. |

### Suggested implementation order

1. **Preflight and safety**
    - Add pgstream binary/version check.
    - Validate source and target connectivity.
    - Reconcile pgstream metadata and replication slot state.
    - Add max-lag / WAL-retention guardrails.

2. **Configuration model expansion**
    - Add explicit stream mode: `snapshot`, `replication`, `snapshot_and_replication`.
    - Add table/schema/object filters.
    - Preserve the current Postgres-to-Postgres path as the default supported mode.

3. **Config-file generation**
    - Implement typed pgstream config rendering.
    - Add golden-file tests for generated configs.
    - Prefer config-driven execution over adding many new CLI flags.

4. **Snapshot support**
    - Add snapshot-only workflow path.
    - Add snapshot-then-replication workflow path.
    - Track snapshot phase, tables completed, rows copied if pgstream exposes that detail, and failure state.

5. **Schema-change handling**
    - Surface schema-change/DDL events in stream status.
    - Add policy modes: `allow`, `block`, `alert_only`, `require_approval`.
    - Add tests for schema changes during active streams.

6. **Transformation and anonymization hardening**
    - Validate rules against the source schema.
    - Validate transformer names and options.
    - Make restart behavior explicit and auditable.
    - Add restart rate limits.

7. **Target expansion**
    - Add typed configuration for Kafka, OpenSearch/Elasticsearch, and webhook targets.
    - Start with validation/config rendering first.
    - Add real end-to-end tests only after Postgres-to-Postgres advanced mode is stable.

8. **Observability**
    - Expand the lag query into a full stream-health query.
    - Emit metrics and structured logs.
    - Add alerting thresholds for lag, stalled streams, failed targets, and repeated restarts.

9. **Integration testing**
    - Add build-tagged real pgstream tests.
    - Cover snapshot, replication, DDL propagation, anonymization, cancellation, restart, and lag threshold behavior.

### Notes for next agent

- Keep this section separate from `## pgroll`; do not merge the two roadmaps.
- pgroll is for schema migration lifecycle. pgstream is for CDC, snapshots, replication, transformations, and downstream streaming.
- Keep long-running pgstream execution inside activities. Workflow code must remain deterministic.
- Treat cancellation as the primary stop mechanism unless pgstream adds a reliable external stop command.
- Preserve `ContinueAsNew` behavior for long-running streams so workflow history does not grow without bound.
- Prefer config-file generation over expanding command-line argument construction indefinitely.
- Be careful with replication slots: abandoned or stalled slots can retain WAL on the source database.
- Treat anonymization and transformation configuration as security-sensitive.
- Do not log raw DSNs, generated configs with secrets, or row-level data.
- For multi-target support, decide whether target failures are isolated per target or fatal to the entire stream.
- Add integration tests incrementally. Start with local Postgres-to-Postgres snapshot-and-replication before adding Kafka/OpenSearch/webhook fixtures.

### Previous Findings ..

Everything that used to be listed here as a "known stub / broken path" or "update
handler that doesn't do anything" has been fixed and has a regression test proving
it (see the `*_test.go` file next to each source file). `go build ./...`, `go vet
./...`, and `go test ./... -race` are all green as of this pass. Treat this file as
a map of *why* each area looks the way it does, not a list of open work.

- **`activities/pgstream.go` `defaultGetLag`** — was a hardcoded `return 0, nil`
  stub; now parses `lag_bytes` from `pgstream status --output json` via the
  extracted, unit-tested `parseLagBytes` helper.
- **`activities/pgstream.go` `PollLag` / `RunStream`** — now call `safeHeartbeat`
  on a ticker while the long-running poll/exec is in flight, so Temporal's
  `HeartbeatTimeout: 2*time.Minute` (set in `cdc_stream.go`) doesn't fire against a
  healthy stream. `defaultRun` uses the new `runPgstreamHeartbeating` helper
  (`Start()`+`Wait()` + ticker) instead of a single blocking `CombinedOutput()`.
  Covered by `TestRunStream_HeartbeatsWhileRunFnBlocks` / `TestPollLag_Heartbeats`.
- **`activities/preview_db.go` `defaultClone`** — now creates the target database
  first (`CREATE DATABASE`), uses `pg_dump --format=plain` (was `--format=custom`,
  a binary archive that only `pg_restore` can read — `psql` was silently getting
  fed a format it can't ingest), and builds the target DSN via the new
  `joinDBName` helper instead of naive `baseConnStr+"/"+dbName`, which corrupted
  keyword=value DSNs. `defaultDrop` uses the same helper so cleanup can always
  recover the db name via `extractDBName`. Covered by `TestJoinDBName` /
  `TestJoinDBName_RoundTripsThroughExtractDBName`.
- **`activities/preview_db.go` `defaultApplyAnonymization`** — now marshals
  `input.Rules` into a pgstream transformer config (`marshalAnonymizationConfig`),
  writes it to a temp file, and passes `--config`; previously ignored `input.Rules`
  entirely. Covered by `TestMarshalAnonymizationConfig`.
- **`activities/pgroll.go` `redactDSN`** — now walks a single-quoted
  `password='...'` value to its real closing quote instead of stopping at the
  first whitespace, which used to leak the tail of a quoted password containing
  spaces. Covered by `TestRedactDSN_QuotedPasswordWithSpace`.
- **`activities/alert.go` `defaultPage`** — `AlertActivities` now has a
  `DefaultWebhookURL` field used when `AlertMessage.WebhookURL` is empty (every
  real caller — `pageOperator` in the workflow package — builds `AlertMessage`
  without ever setting it). `cmd/pgschema/main.go` wires it from
  `PGSCHEMA_ALERT_WEBHOOK_URL`. Covered by
  `TestPage_DefaultWebhookURL_UsedWhenMessageEmpty`.

## Update handlers — now actually wired

- `schema_migration.go` **`extend-wait`** — the handler sends the extension
  through a buffered `extendWaitCh`; the Step-3 wait is now a loop that recomputes
  `remaining` against a `waitDeadline` and recreates its timer on every extension,
  instead of a single fixed 60-minute `workflow.NewTimer`. Covered by
  `TestUpdateHandler_ExtendWait_ActuallyDelaysRollback` (asserts the workflow is
  still running well past the *original* deadline once extended).
- `preview_clone.go` **`extend-ttl`** — same pattern via `extendTTLCh` driving a
  `ttlDeadline` loop in Step 5, so the real drop timer moves, not just the
  query-visible `endpoint.ExpiresAt`. Covered by
  `TestUpdateHandler_ExtendTTL_ActuallyDelaysDrop`.
- `cdc_stream.go` **`update-anon-rules`** — `RunStream` wraps an external
  process that can't be hot-reloaded, so there's no way to make an in-place
  update take effect. The handler now sends to `restartCh`; Step 4's loop treats
  that as a third exit condition (alongside `stopped`/`streamErr`) and, when nothing
  else has already claimed the exit, cancels the run and `ContinueAsNew`s with the
  updated `cfg` so the fresh run's `RunStream` actually picks up the new rules.
  Covered by `TestUpdateAnonymizationRules_TriggersRestart`. Note:
  `TestUpdateAnonymizationRules_Valid` no longer also sends a stop signal — the
  restart races it out first, which is the new correct behavior, not a test bug.

## Shutdown / cancellation gap — fixed

`cdc_stream.go` now derives `runCtx, cancelRun := workflow.WithCancel(ctx)` and
schedules both `RunStream` and the lag-polling `PollLag` goroutine under it.
Every exit path (`stopped`, `streamErr`, `restart`, and the `MaxIterations`
`ContinueAsNew`) calls `cancelRun()` before `lagDone.Receive(ctx, nil)`, so that
receive can no longer block for up to the 7-day `StartToCloseTimeout`. Covered by
`TestHappyPath_StopSignal` (previously hung/failed on `test timeout: 3s` — this
was the most severe bug found: most of the CDC test suite failed on this before
the fix).

Related, separately-discovered bug fixed in the same file: `RunStream`'s
`RetryPolicy.MaximumAttempts` was `0` (unlimited). Combined with the shutdown fix
this became directly observable — a permanently-failing stream retried forever
and `streamFuture` never resolved, so the "stream died → alert operator" path was
unreachable no matter how badly the stream failed. Now bounded at `20`. Covered by
`TestStreamDies_AlertFired`.

## Test-harness gotcha (not a production bug, but will bite you)

`TestWorkflowEnvironment.UpdateWorkflow(...)` queues the update handler's
invocation as an internal callback (`env.postCallback(fn, true)`) — it does **not**
run synchronously before `UpdateWorkflow` returns. Asserting on the
`TestUpdateCallback`'s `OnAccept`/`OnReject`/`OnComplete` results in the *same*
`RegisterDelayedCallback` body that called `UpdateWorkflow` reads stale
(zero-value) state, because that queued callback is only drained *after* the
current one returns. Every Update-handler test in this package now follows the
two-callback pattern: one callback issues `UpdateWorkflow` and captures results
into outer-scoped variables; a second, later-scheduled callback asserts on them.
If you add a new Update-handler test, follow the same split or it will silently
assert on empty/zero values instead of the real outcome.

## Duplication that could still be consolidated (untouched, not a bug)

- `logger()` nil-fallback is copy-pasted identically across all 4 activity structs
  (`AlertActivities`, `PgrollActivities`, `PgstreamActivities`, `PreviewDBActivities`).
- DSN parsing (URI vs keyword=value) is independently reimplemented in
  `redactDSN` (`pgroll.go`) and `baseConnStr`/`extractDBName`/`joinDBName`
  (`preview_db.go`).
- `exec.CommandContext` + wrap-error-with-output pattern is reimplemented several
  times (`runPgroll`, `runPgstream`, `runPgstreamOutput`, `runPgstreamHeartbeating`,
  inline in `defaultClone`/`defaultDrop`) with inconsistent error formatting.

If adding a new activity type, prefer factoring a shared `baseActivities{ log }`
and a shared `runCommand` helper rather than copying these patterns again.

## Misc

- No CLAUDE.md exists at repo root or under `pgschema/` — this file is the only
  agent-facing guidance for this package.
- Watch out for this session's environment auto-redacting literal secret-shaped
  substrings (e.g. a `scheme://user:pass@host` URI) in tool-written file content —
  a test literal that should contain `://` can silently come out as `******` with
  the `://` marker gone, breaking string-form-detection logic under test. If a
  DSN-parsing test fails in a way that looks like the wrong branch was taken,
  `grep` the actual file content for the literal you meant to write before
  assuming the parsing code is wrong.
