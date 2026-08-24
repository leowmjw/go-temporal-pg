//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"

	_ "github.com/lib/pq"
)

func TestPgstreamSnapshotCopiesRows(t *testing.T) {
	if _, err := exec.LookPath("pgstream"); err != nil {
		t.Skip("pgstream binary not installed")
	}

	ctx := context.Background()
	sourceDSN, sourceDB := startPostgres(t, ctx, "source_snapshot")
	defer sourceDB.Close()
	targetDSN, targetDB := startPostgres(t, ctx, "target_snapshot")
	defer targetDB.Close()

	_, err := sourceDB.ExecContext(ctx, `
		CREATE TABLE users (id SERIAL PRIMARY KEY, email TEXT NOT NULL);
		INSERT INTO users (email) VALUES ('alice@example.com'), ('bob@example.com');
	`)
	require.NoError(t, err)

	acts := activities.NewPgstreamActivities(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	cfg := types.StreamConfig{
		SourceDSN:           sourceDSN,
		TargetDSN:           targetDSN,
		ReplicationSlotName: "pgstream_snapshot_slot",
		Mode:                types.StreamModeSnapshot,
		Filters:             types.StreamFilters{IncludedTables: []string{"public.users"}},
		Target: types.StreamTargetConfig{
			Type:     types.StreamTargetTypePostgres,
			Postgres: &types.PostgresTargetConfig{URL: targetDSN},
		},
	}

	require.NoError(t, acts.RunFn(ctx, cfg))
	require.Eventually(t, func() bool {
		var count int
		if err := targetDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
			return false
		}
		return count == 2
	}, 30*time.Second, 500*time.Millisecond)
}

func TestPgstreamSnapshotAndReplicationReplicatesInsert(t *testing.T) {
	if _, err := exec.LookPath("pgstream"); err != nil {
		t.Skip("pgstream binary not installed")
	}

	ctx := context.Background()
	sourceDSN, sourceDB := startPostgres(t, ctx, "source_replication")
	defer sourceDB.Close()
	targetDSN, targetDB := startPostgres(t, ctx, "target_replication")
	defer targetDB.Close()

	_, err := sourceDB.ExecContext(ctx, `
		CREATE TABLE users (id SERIAL PRIMARY KEY, email TEXT NOT NULL);
		INSERT INTO users (email) VALUES ('alice@example.com');
	`)
	require.NoError(t, err)

	acts := activities.NewPgstreamActivities(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	cfg := types.StreamConfig{
		SourceDSN:           sourceDSN,
		TargetDSN:           targetDSN,
		ReplicationSlotName: "pgstream_repl_slot",
		Mode:                types.StreamModeSnapshotAndReplication,
		Filters:             types.StreamFilters{IncludedTables: []string{"public.users"}},
		Target: types.StreamTargetConfig{
			Type:     types.StreamTargetTypePostgres,
			Postgres: &types.PostgresTargetConfig{URL: targetDSN},
		},
	}

	require.NoError(t, acts.InitFn(ctx, cfg))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- acts.RunFn(runCtx, cfg)
	}()

	require.Eventually(t, func() bool {
		var count int
		if err := targetDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
			return false
		}
		return count == 1
	}, 30*time.Second, 500*time.Millisecond)

	_, err = sourceDB.ExecContext(ctx, `INSERT INTO users (email) VALUES ('carol@example.com')`)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		var count int
		if err := targetDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
			return false
		}
		return count == 2
	}, 60*time.Second, 500*time.Millisecond)

	cancel()
	err = <-runErr
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "signal: killed") || strings.Contains(err.Error(), "context canceled"))
}

func startPostgres(t *testing.T, ctx context.Context, dbName string) (string, *sql.DB) {
	t.Helper()

	container, err := postgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:16"),
		postgres.WithDatabase(dbName),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(5*time.Minute)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.PingContext(ctx))
	return dsn, db
}
