# pgschema — notes for agents

Temporal-based schema migration / CDC-stream / preview-clone package wrapping the
`pgroll` and `pgstream` CLIs. Package layout: `internal/activities` (CLI wrappers),
`internal/workflow` (Temporal workflows), `internal/types`, `cmd/pgschema` (worker entrypoint).

## Status: known stubs/bugs below are FIXED

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
