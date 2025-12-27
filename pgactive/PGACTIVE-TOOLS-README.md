# pgactive-tools: Active-Active Replication State Toolkit

A production-ready Golang toolkit for monitoring and exposing the runtime state of a pgactive active-active PostgreSQL cluster.

## Features

- **Collector**: Resilient collector that queries each node's pgactive database for replication metrics
- **Storage**: SQLite storage for persisting snapshot history and node metrics
- **API Server**: HTTP API exposing snapshot data via REST and Server-Sent Events (SSE)
- **CLI**: Human-friendly commands for displaying lag and conflict metrics
- **SDK**: Go client library with Temporal activity integration
- **Observability**: Prometheus metrics, structured logs, and OpenTelemetry integration

## Quick Start

### Installation

```bash
# Clone repository
git clone https://github.com/company/pgactive-tools
cd pgactive-tools

# Build binaries
go build -o bin/pgactive-server ./cmd/server
go build -o bin/pgactive-tools ./cmd/pgactive-tools
```

### Configuration

Create a configuration file at `./config/tools.yaml` or specify an alternate path with `--config`:

```yaml
collector:
  interval: 2s
  nodes:
    - name: node1
      dsn: ${PGACTIVE_NODE1_DSN:-postgres://user:password@host1:5432/database}
    - name: node2
      dsn: ${PGACTIVE_NODE2_DSN:-postgres://user:password@host2:5432/database}

thresholds:
  lag_seconds_warn: 5
  lag_seconds_crit: 15
  conflicts_warn: 3/min

server:
  listen: :8080

database:
  path: ./data/replication_state.db
```

Environment variables can override DSN configurations:

```bash
export PGACTIVE_NODE1_DSN="postgres://user:password@host1:5432/database"
export PGACTIVE_NODE2_DSN="postgres://user:password@host2:5432/database"
```

### Running the Server

```bash
# Start the server
./bin/pgactive-server

# Or with a custom config
./bin/pgactive-server --config /path/to/config.yaml
```

### Using the CLI

```bash
# Show replication lag
./bin/pgactive-tools lag

# Show conflicts
./bin/pgactive-tools conflicts

# JSON output format
./bin/pgactive-tools lag --format json

# Exit with non-zero status if thresholds exceeded (for CI/CD)
./bin/pgactive-tools lag --fail-on-warn
```

## API Reference

### Endpoints

- **GET /snapshot**: Returns the latest snapshot as JSON
- **GET /watch**: Server-Sent Events stream of snapshots
- **GET /health**: Health check endpoint
- **GET /metrics**: Prometheus metrics

### Snapshot JSON Structure

```json
{
  "version": 123,
  "collected_at": "2023-07-17T18:25:44+08:00",
  "nodes": {
    "node1": {
      "name": "node1",
      "status": "HEALTHY",
      "metrics": {
        "lag_xid": 10,
        "lag_seconds": 1.5,
        "walsender_active": true,
        "sent_lsn": "0/1000",
        "write_lsn": "0/990", 
        "flush_lsn": "0/980",
        "replay_lsn": "0/970",
        "conflict_count": 0
      }
    },
    "node2": {
      "status": "DEGRADED",
      "metrics": {
        "lag_xid": 50,
        "lag_seconds": 5.5,
        "walsender_active": true,
        "sent_lsn": "0/2000",
        "write_lsn": "0/1900", 
        "flush_lsn": "0/1800",
        "replay_lsn": "0/1700",
        "conflict_count": 2
      }
    }
  }
}
```

## SDK Usage

### Basic Usage

```go
package main

import (
    "context"
    "log/slog"
    "os"
    "time"
    
    "company.com/infra/pgactive-upgrade/pkg/sdk"
)

func main() {
    logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
    
    // Create client
    client, err := sdk.New("http://localhost:8080", logger)
    if err != nil {
        logger.Error("Failed to create client", "error", err)
        os.Exit(1)
    }
    
    // Get snapshot
    ctx := context.Background()
    snapshot, err := client.GetSnapshot(ctx)
    if err != nil {
        logger.Error("Failed to get snapshot", "error", err)
        os.Exit(1)
    }
    
    // Process snapshot
    for nodeName, nodeState := range snapshot.Nodes {
        logger.Info("Node status", 
            "node", nodeName,
            "status", nodeState.Status,
            "lag_seconds", nodeState.Metrics.LagSeconds)
    }
    
    // Watch snapshots
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()
    
    snapshotCh, err := client.WatchSnapshots(ctx)
    if err != nil {
        logger.Error("Failed to watch snapshots", "error", err)
        os.Exit(1)
    }
    
    for snapshot := range snapshotCh {
        logger.Info("Received snapshot update", "version", snapshot.Version)
        // Process real-time updates
    }
}
```

### Temporal Integration

```go
package workflow

import (
    "context"
    "time"
    
    "go.temporal.io/sdk/activity"
    "go.temporal.io/sdk/workflow"
    
    "company.com/infra/pgactive-upgrade/pkg/sdk"
)

func YourWorkflow(ctx workflow.Context) error {
    options := workflow.ActivityOptions{
        StartToCloseTimeout: 10 * time.Second,
        RetryPolicy: &temporal.RetryPolicy{
            MaximumAttempts: 3,
        },
    }
    ctx = workflow.WithActivityOptions(ctx, options)
    
    var snapshot types.ReplicationSnapshot
    if err := workflow.ExecuteActivity(ctx, GetReplicationSnapshot).Get(ctx, &snapshot); err != nil {
        return err
    }
    
    // Use snapshot in workflow
    workflow.GetLogger(ctx).Info("Received snapshot", "version", snapshot.Version)
    
    return nil
}

func GetReplicationSnapshot(ctx context.Context) (types.ReplicationSnapshot, error) {
    // Get logger from Temporal context
    logger := activity.GetLogger(ctx)
    
    // Create client
    client, err := sdk.New("http://localhost:8080", logger)
    if err != nil {
        return types.ReplicationSnapshot{}, err
    }
    
    // Create Temporal activity wrapper
    activity := sdk.NewTemporalActivity(client)
    
    // Get snapshot using activity wrapper
    return activity.GetReplicationSnapshot(ctx)
}
```

## Architecture

```
┌──────────────┐    ┌──────────────┐    ┌──────────────┐
│ PostgreSQL   │    │ PostgreSQL   │    │ PostgreSQL   │
│ Node 1       │    │ Node 2       │    │ Node N       │
└──────┬───────┘    └──────┬───────┘    └──────┬───────┘
       │                   │                   │
       │                   │                   │
       │                   ▼                   │
       │           ┌──────────────┐            │
       └──────────►│  Collector   │◄───────────┘
                  │  (Metrics)    │
                  └──────┬───────┘
                         │
                         ▼
                  ┌──────────────┐
                  │   SQLite     │
                  │   Storage    │
                  └──────┬───────┘
                         │
                         ▼
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│  CLI Tools   │   │  HTTP API    │   │  Prometheus  │
│  (lag,       │◄──┤  Server      ├──►│  Metrics     │
│   conflicts) │   │  (/snapshot) │   │  (/metrics)  │
└──────────────┘   └──────┬───────┘   └──────────────┘
                         │
                         ▼
                  ┌──────────────┐
                  │  Go SDK      │
                  │  Client      │
                  └──────┬───────┘
                         │
                         ▼
                  ┌──────────────┐
                  │  Temporal    │
                  │  Workflows   │
                  └──────────────┘
```

## Build & Deployment

### Building from Source

```bash
# Build all binaries
go build -o bin/pgactive-server ./cmd/server
go build -o bin/pgactive-tools ./cmd/pgactive-tools

# Run tests
go test ./...
```

### Docker Deployment

```bash
# Build Docker image
docker build -t pgactive-tools:latest .

# Run server container
docker run -p 8080:8080 \
  -v $(pwd)/config:/app/config \
  -v $(pwd)/data:/app/data \
  -e PGACTIVE_NODE1_DSN="postgres://user:password@host1:5432/database" \
  -e PGACTIVE_NODE2_DSN="postgres://user:password@host2:5432/database" \
  pgactive-tools:latest
```

### Kubernetes Deployment

See [examples/kubernetes](examples/kubernetes) for deployment manifests.

## Observability

### Prometheus Metrics

The following metrics are exposed at `/metrics`:

- `pgactive_replication_lag_xid`: Replication lag in transaction IDs
- `pgactive_replication_lag_seconds`: Replication lag in seconds
- `pgactive_node_conflicts_total`: Number of conflicts detected
- `pgactive_node_status`: Node health status (0=Unknown, 1=Healthy, 2=Degraded, 3=Critical, 4=Down)
- `pgactive_collector_errors_total`: Number of collection errors
- `pgactive_snapshot_version`: Current snapshot version

### OpenTelemetry Integration

To enable OpenTelemetry integration, set the following environment variables:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="http://localhost:4317"
export OTEL_SERVICE_NAME="pgactive-tools"
```

## License

MIT License
