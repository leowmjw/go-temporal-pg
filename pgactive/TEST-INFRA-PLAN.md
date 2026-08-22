# Test Infrastructure Plan

Status: **plan only — not implemented.** Parked while focus moves to `pgschema`
tooling. The only changes currently on disk are the minimal ones needed to make
`make test-integration` pass (see "Current state" at the bottom).

## Goal

`docker-compose.test.yml` should start **Postgres and nothing else**. Everything
else is either run locally on the host, replaced with an in-process fake, or
dropped entirely.

## 1. Compose: reduce to Postgres only

Keep exactly two services, both stock upstream images:

| Service | Image | Port | Role |
|---|---|---|---|
| `postgres-source` | `postgres:17` | 5432 | upgrade source |
| `postgres-target` | `postgres:18` | 5433 | upgrade target |

Deliberately different majors so the 17 → 18 rolling-upgrade path under test is
real. Both keep `wal_level=logical`, `max_replication_slots`,
`max_logical_replication_workers`, the `test/sql/init-*.sql` seed mounts, and
`pg_isready` healthchecks.

Remove from compose:

- **`temporal` + `postgres-temporal`** → run locally instead. `temporal server
  start-dev` gives an in-memory server on `:7233` with the UI on `:8233`, no
  database container and no `auto-setup` schema step. Two containers and a
  healthcheck disappear. Note that most existing tests don't need even this —
  they use the Temporal Go SDK's `testsuite` in-process environment, which
  needs no server at all. The one exception is `TestIntegrationTestSuite`,
  which currently self-skips with "Temporal server not available".
- **`prometheus`, `grafana`, `jaeger`** → drop. There are no assertions against
  any of them; they are three images and ~1GB of pulls serving nothing. If
  observability needs manual inspection later, that belongs in a separate
  opt-in `docker-compose.observability.yml`, not the test path.
- **`localstack`** → see section 2.
- **`pgactive-upgrade`, `test-data-generator`** → already behind a
  `profiles: ["app"]` guard because neither can build (`Dockerfile.test` and
  `test/Dockerfile.datagen` don't exist; the real `Dockerfile` builds
  `./cmd/server` and `./cmd/pgactive-tools` and copies `config/`, none of which
  exist — `cmd/` holds only `main.go`). Either delete these services or fix the
  images; leaving them half-defined is the worst of the three.

Also still outstanding: `shared_preload_libraries='pgactive'` was removed from
both Postgres services because stock images don't ship the extension and
Postgres exits at startup without it. If the tests ever need to exercise real
pgactive rather than the mocked activities, this needs a purpose-built image
that compiles the extension — that is its own piece of work.

## 2. Replacing LocalStack

**Correcting the premise:** as far as I know LocalStack isn't deprecated or
discontinued — it's still actively developed commercially. I'd want to confirm
that before it's written down as fact. But the conclusion to drop it holds
regardless, for a different and more decisive reason:

**No local emulator implements Aurora.** Aurora's distinguishing behaviour —
the shared distributed storage layer, fast clone, failover semantics, Serverless
v2 scaling — is proprietary and has no open reimplementation. LocalStack
emulates the RDS *control plane* (`CreateDBCluster`, `DescribeDBClusters`, …)
and backs it with an ordinary Postgres process. For a tool whose entire job is
orchestrating an Aurora upgrade, that fixture answers "does my SDK call
serialize correctly", not "does my upgrade work". It buys very little for a
whole container.

Options considered:

| Option | Verdict |
|---|---|
| **Go interfaces + hand-written fakes** | **Recommended.** Define an `AWSClient` interface at the activity boundary, fake it in tests. Zero containers, fast, and lets you script the failure modes that matter (failover mid-shift, cluster stuck in `modifying`) — which no emulator will reproduce anyway. Fits how the activities are already structured. |
| **Real Aurora in a sandbox AWS account** | The only thing that genuinely validates Aurora behaviour. Slow, costs money, needs credentials. Worth it as a small, separately-tagged, manually-triggered suite — not in `make test`. |
| **Moto (`moto server`)** | Python sidecar, RDS control-plane mocking only. Same fidelity ceiling as LocalStack with an extra language runtime. No gain. |
| **Testcontainers LocalStack module** | Wraps LocalStack; inherits the fidelity problem. Would at least remove it from compose, but section 1 removes it anyway. |
| **Keep LocalStack** | Only justifiable if IAM/Secrets Manager wiring specifically needs testing — and those are better served by fakes too. |

Proposed split: fakes for the default suite, plus a build-tagged
`//go:build aws_live` suite against real Aurora for pre-release confidence.

## 3. Resulting developer workflow

```bash
# once per session, if a test actually needs a server
temporal server start-dev

# per run
docker compose -f docker-compose.test.yml up -d   # two Postgres containers
make test
```

Net effect: 8 containers → 2, no LocalStack, no Temporal database, no
observability stack, and no unbuildable services in the default path.

## 4. Open questions

1. Is `TestIntegrationTestSuite` (the skipping one) worth keeping? If the
   `testsuite` in-process environment covers it, deleting it removes the last
   reason to run a Temporal server locally at all.
2. Does the pgactive extension need to be genuinely exercised, or do mocked
   activities remain sufficient? This decides whether a custom Postgres image
   is needed.
3. Is there a sandbox AWS account available for an `aws_live` suite?

## Current state on disk

Working tree changes made so far, all in service of getting the suite green —
none of the restructuring above:

- `test/integration/real_integration_test.go` — `postgres:15.4` → `postgres:18`.
  The original "hang" was just an image pull for a tag not in the local cache.
- `Makefile` — restored `test: test-unit test-integration`.
- `docker-compose.test.yml` — dropped obsolete `version:`, Postgres 17/18,
  removed the `shared_preload_libraries='pgactive'` startup failure and two
  nonexistent bind-mount paths, fixed the Temporal healthcheck (`tctl` is
  deprecated in 1.25), guarded the two unbuildable services behind a profile.

`make test` passes. The compose file still defines all 8 services; trimming it
to two is item 1 above and has not been done.
