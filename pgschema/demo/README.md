# pgschema demo: the pgroll workflow, live

Shows the real thing pgschema exists for: zero-downtime Postgres schema
migrations, orchestrated by a Temporal workflow, run against a real pgroll
binary and a real (throwaway) Postgres — not a mock. A small web UI
(Datastar-driven, no JS build step) lets you click through 5 increasingly
complex, real-life migration scenarios and watch the workflow's phase/status
change live over SSE, including the app-ready and rollback signals the
workflow is actually waiting on mid-flight.

## One-time setup

```sh
mise run install-tools   # pgroll + pgstream binaries
mise run demo-reset      # fresh demo Postgres (v18), seeded + pgroll-baselined
```

`demo-reset` = `docker compose down -v` + `up --wait` + `pgroll init` +
`pgroll baseline`. The seed data (`init/001_seed_users.sql`) creates a
`users` table with plain SQL — deliberately **not** through pgroll — because
the realistic starting point for most teams is an *existing* schema, not a
green field. `pgroll baseline` is what lets pgroll adopt a schema it didn't
create itself (see `mise run demo-init` in `../mise.toml`).

## Run it

```sh
mise run dev
```

This brings up, together, under `overmind`: the demo Postgres, a Temporal
dev server, the pgschema worker (hot-reloading via `air` — edit
`internal/activities` or `internal/workflow` and it rebuilds live), and the
demo web UI (also hot-reloading). Open **http://localhost:8090**.

Press **Ctrl-C** (or run `overmind quit` from another shell) to stop
everything. All four processes are configured to shut down cleanly: `air`
sends the Go binaries a real interrupt (not a hard kill) so the Temporal
worker stops its workers gracefully, and cleans its `tmp/` build output;
the `postgres` line traps the stop signal and runs `docker compose down`,
so the container and network are actually removed rather than left running
in the background — the named volume (and your baselined data) is *not*
deleted, so the next `mise run dev` picks up right where you left off.

Temporal's own Web UI is at http://localhost:8233 if you want to show the
actual workflow history/event timeline alongside the demo page — the
workflow IDs are `demo-<scenario>-<unix-ts>`.

## The 5 scenarios (+2 bonus)

All operate on one evolving `users` table, basic → complex:

| # | What | Why it's realistic |
|---|------|---------------------|
| 1 | Add `email` (nullable) | The safest possible expand — no backfill, nothing can break. |
| 2 | Add `status` NOT NULL DEFAULT `'active'` | Needs a backfill for existing rows — pgroll does it in the expand phase, batched. |
| 3 | Rename `full_name` → `display_name` | Renames are usually the *first* "scary" migration a team hits — both names stay live until contract. |
| 4 | Unique constraint on `email` | Classic zero-downtime index build — no `ACCESS EXCLUSIVE` lock on a live table. |
| 5 | Split `display_name` into `first_name`/`last_name` | Multi-op, backfilled from existing data via `split_part()` — the kind of migration that's genuinely risky to hand-write. |
| 5b | Rollback walkthrough | Same shape as #1, but click **Abort / rollback** instead of **Send app-ready** — shows the workflow's compensating path. |
| 6 | `external_id UUID DEFAULT uuidv7()` | Bonus: Postgres 18's new builtin sortable-UUID generator, no `pgcrypto`/`uuid-ossp` extension needed — a migration many teams will hit going into public APIs. |

Each is one pgroll migration file in `migrations/`, run for real by
`PgrollActivities` inside `SchemaMigrationWorkflow` (see
`../internal/workflow/schema_migration.go`) — not simulated.

## What you're watching

`SchemaMigrationWorkflow` is: `validate → start (expand) → wait for
app-ready → complete (contract) → verify`. The **Send app-ready** button
sends the `app-ready` signal the workflow is genuinely parked on (in real
usage this comes from your deploy pipeline once the new app version is
live and reading/writing the new column). **Abort / rollback** sends the
`rollback` signal, which the workflow picks up and compensates via
`pgroll rollback` instead of completing.

Click a scenario, watch:
1. `phase` step through `validating → starting → waiting_for_app_ready`
2. the **Send app-ready** button appear once it's actually waiting on you
3. `phase → completing → verifying`, `status → completed`
4. the activity log (bottom panel) tracking every step with a timestamp

Run scenarios in order (1 → 6) — each depends on the schema state the
previous one left behind (e.g. #5 reads `display_name`, which only exists
after #3 has run).

## Resetting

```sh
mise run demo-reset
```

Safe to run anytime between scenarios or after a mistake — it's a
throwaway Docker volume. You'll need to re-run scenarios 1→N in order again
afterward, since state doesn't persist across a reset.

## Troubleshooting

- **"Schema is non-empty but has no migration history. Run `pgroll baseline`
  first"** — `demo-init`/`demo-reset` wasn't run (or was run against a
  different `PGROLL_DSN`). Run `mise run demo-reset`.
- **A scenario button does nothing** — check the worker's logs (the `worker`
  pane under `overmind`); the workflow may have failed validation (e.g. you
  ran scenario 5 before scenario 3, so `display_name` doesn't exist yet).
- **Datastar UI doesn't update** — the page loads `datastar.js` from a CDN
  (`cdn.jsdelivr.net`); it needs internet access in the browser. Check the
  browser console for a 404/CSP error if it's silent.
