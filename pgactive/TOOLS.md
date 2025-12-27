# TOOLS – Active-Active Replication State Toolkit (PRD)

## 1. Purpose
Provide a set of production-ready Golang tools that continuously **observe, persist and expose** the runtime state of a pgactive active-active PostgreSQL cluster so that a companion *Orchestrator Agent* (running inside a Temporal workflow) can make data-driven decisions such as traffic shifting, conflict remediation, and automatic fail-over.

## 2. Problem Statement
pgactive replicates changes **asynchronously** between independent RDS for PostgreSQL instances.  Because replication is not synchronous and each node can accept writes, we must track:
* Replication lag (time & XID/LSN based)
* Health of WAL sender/receiver slots
* Frequency and details of write conflicts
* Overall node / replication-set health (e.g., disabled subscriptions)
without manually querying each database.

Existing monitoring dashboards are insufficient because the Temporal workflow must *programmatically* access **fresh, low-latency** metrics and historical context while executing activities.

## 3. Goals & Non-Goals
Goals
1. Collect and normalise pgactive replication metrics & conflict information from **N nodes**.
2. Persist the latest snapshot in a versioned, query-friendly **SQLite database** stored in `./data/replication_state.db`.
3. Expose RESTful HTTP endpoints (built with Go's `net/http`) for the Temporal Orchestrator Agent to fetch metrics or subscribe to updates.
4. Emit Prometheus metrics & structured logs for SRE dashboards.
5. Run as a lightweight side-car **binary** or library that can be embedded inside Temporal activities.

Non-Goals
* Managing schema migrations or deploying pgactive itself.
* Performing conflict resolution – only surfacing data; remediation is handled elsewhere.
* Generic PostgreSQL monitoring (only pgactive-specific state).

## 4. High-Level Architecture
```
┌────────────┐      scrape   ┌──────────────┐
│  Collector │◄─────────────┤  pgactive DBs │
└────┬───────┘               └──────────────┘
     │ snapshot                        ▲
     ▼                                 │
┌────────────┐   query / stream  ┌──────────────┐
│  Storage   │──────────────────►│  API Server  │──► Temporal Workflow
└────────────┘                   └──────────────┘
     ▲                                    ▲
     │ Prom metrics                       │ health
┌────┴────────┐                   ┌───────┴─────────┐
│ Observabity │                   │  CLI / Metrics │
└─────────────┘                   └─────────────────┘
```
### Components
1. **Collector (pkg/collector)** – Goroutine per node; runs SQL below every *T* seconds:
   ```sql
   SELECT node_name,
          last_applied_xact_id::int - last_sent_xact_id::int AS lag_xid,
          last_sent_xact_at - last_applied_xact_at           AS lag_time,
          walsender_active,
          sent_lsn, write_lsn, flush_lsn, replay_lsn
   FROM   pgactive.pgactive_node_slots;

   SELECT count(*) AS conflict_cnt
   FROM   pgactive.pgactive_conflict_history
   WHERE  local_conflict_time > now() - interval '1 minute';
   ```
2. **Storage (pkg/store)** – SQLite database located at `./data/replication_state.db`; accessed via `database/sql` with `modernc.org/sqlite` driver.
3. **API Server (cmd/server)** – Built with Go's `net/http` (standard library). Exposes:
   * `GET /snapshot` – returns latest JSON snapshot
   * `GET /watch` – Server-Sent Events (SSE) stream of snapshots
4. **CLI (cmd/pgactive-tools)** – Human-friendly `lag` and `conflicts` sub-commands.
5. **SDK (pkg/sdk)** – Typed Go client for Temporal activities.

## 5. Detailed Requirements
### Functional
F-1 Scrape interval configurable (default 2s).
F-2 Support TLS & IAM auth for RDS.
F-3 Auto-discover pgactive nodes via ENV list or SQL `SELECT * FROM pgactive.pgactive_nodes`.
F-4 Collector retries with exponential back-off; exposes per-node status.
F-5 API `GetSnapshot` returns monotonic `Version` and `CollectedAt` timestamps.
F-6 `Watch` streams deltas ≤1 KB per message to keep Temporal activity lightweight.
F-7 CLI returns non-zero exit code if any node exceeds SLA thresholds (configurable), enabling CI/CD checks.

### Non-Functional
NF-1 **Latency:** < 1 s between query execution and API availability.
NF-2 **Performance:** ≤10 MB RAM, ≤50 CPU-msec per scrape @ 10 nodes.
NF-3 **Reliability:** 3 consecutive scrape failures mark node `DEGRADED`; 10 failures mark `DOWN`.
NF-4 **Security:** No plaintext passwords in logs; supports AWS Secrets Manager integration.
NF-5 **Observability:** Prometheus `pgactive_lag_seconds`, `pgactive_conflict_total`, etc.; structured JSON logs.
NF-6 **Extensibility:** New metrics added via declarative SQL file in `config/queries`.

## 6. Public APIs
### HTTP Endpoints
```
GET /snapshot
  Response 200 OK
  {
    "version": <int>,
    "collected_at": "RFC3339 timestamp",
    "nodes": [ ... ]
  }

GET /watch
  Content-Type: text/event-stream (SSE)
  Data: Snapshot JSON whenever state updates
```
The `watch` endpoint streams snapshots whenever the internal version counter increments.

## 7. Temporal Integration
* **Activity:** `FetchReplicationSnapshot` → calls SDK `GetSnapshot`.
* **Signal:** Orchestrator Agent receives `Snapshot` via Temporal signal channel to react (e.g., pause writes).
* **Heartbeat:** Activity heartbeats every 5 s with lag summary to detect long network partitions.

## 8. Configuration
```
PGACTIVE_TOOLS_CONFIG=./config/tools.yaml
```
Example YAML
```yaml
collector:
  interval: 2s
  nodes:
    - name: endpoint1
      dsn:  postgresql://... # env or secret ref
    - name: endpoint2
      dsn:  postgresql://...
thresholds:
  lag_seconds_warn: 5
  lag_seconds_crit: 15
  conflicts_warn: 3/min
```

## 9. Milestones
1. **MVP (2 wks):** Collector + SQLite storage + CLI.
2. **v0.2 (1 wk):** HTTP API + Prometheus metrics.
3. **v1.0 (3 wks):** Temporal SDK, Secrets Manager integration, Docker image, Terraform module.

## 10. Open Questions / Risks
* How to handle schema changes if pgactive blocks DDL? – Might require maintenance window coordination.
* Do we need cross-region latency compensation in collector scraping? – TBD.
* Conflict history table can grow quickly; retention policy needed.

## 11. Acceptance Criteria
* End-to-end demo: Temporal workflow pauses writes when `lag_seconds` > threshold and resumes automatically.
* 90th-percentile API latency <200 ms under load (100 rps).
* Lint, unit tests (≥80% coverage) & GitHub Actions CI passing.

---
*Last updated: 2025-07-17*
