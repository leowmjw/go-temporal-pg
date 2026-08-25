// cmd/pgschema is the CLI entry-point for the pgschema worker.
// Running this binary starts the Temporal worker for all three task queues:
//   - pgschema-migration  (SchemaMigrationWorkflow + BaselineWorkflow)
//   - pgschema-cdc        (CDCStreamWorkflow)
//   - pgschema-preview    (PreviewCloneWorkflow)
package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/types"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/workflow"
)

func main() {
	log := slog.New(slog.NewMultiHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo, AddSource: true}),
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}),
	))
	slog.SetDefault(log)

	c, err := client.Dial(client.Options{
		HostPort:  os.Getenv("TEMPORAL_ADDRESS"), // "" keeps the SDK default (127.0.0.1:7233)
		Namespace: os.Getenv("TEMPORAL_NAMESPACE"),
	})
	if err != nil {
		log.Error("failed to connect to Temporal", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer c.Close()

	pgrollActs := activities.NewPgrollActivities(log)
	pgstreamActs := activities.NewPgstreamActivities(log)
	previewActs := activities.NewPreviewDBActivities(log)
	alertActs := activities.NewAlertActivities(log)
	alertActs.DefaultWebhookURL = os.Getenv("PGSCHEMA_ALERT_WEBHOOK_URL")
	if alertActs.DefaultWebhookURL == "" {
		log.Warn("PGSCHEMA_ALERT_WEBHOOK_URL not set; operator paging is disabled")
	}

	startupInput := types.MigrationInput{
		ExpectedPgrollVersion:     envOrDefault("PGSCHEMA_EXPECTED_PGROLL_VERSION", "v0.16.2"),
		RequireExactPgrollVersion: parseBoolEnv("PGSCHEMA_REQUIRE_EXACT_PGROLL_VERSION"),
	}
	version, err := pgrollActs.CheckPgrollVersion(context.Background(), startupInput)
	if err != nil {
		log.Error("pgroll startup check failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	log.Info("pgroll startup check complete", slog.String("pgroll_version", version), slog.String("expected_pgroll_version", startupInput.ExpectedPgrollVersion))

	migrationWorker := worker.New(c, workflow.SchemaMigrationTaskQueue, worker.Options{})
	migrationWorker.RegisterWorkflow(workflow.SchemaMigrationWorkflow)
	migrationWorker.RegisterWorkflow(workflow.BaselineWorkflow)
	migrationWorker.RegisterActivity(pgrollActs)
	migrationWorker.RegisterActivity(alertActs)

	cdcWorker := worker.New(c, workflow.CDCStreamTaskQueue, worker.Options{})
	cdcWorker.RegisterWorkflow(workflow.CDCStreamWorkflow)
	cdcWorker.RegisterActivity(pgstreamActs)
	cdcWorker.RegisterActivity(alertActs)

	previewWorker := worker.New(c, workflow.PreviewCloneTaskQueue, worker.Options{})
	previewWorker.RegisterWorkflow(workflow.PreviewCloneWorkflow)
	previewWorker.RegisterActivity(previewActs)
	previewWorker.RegisterActivity(alertActs)

	log.Info("pgschema workers starting")
	if err := migrationWorker.Start(); err != nil {
		log.Error("migration worker failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if err := cdcWorker.Start(); err != nil {
		log.Error("cdc worker failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if err := previewWorker.Start(); err != nil {
		log.Error("preview worker failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	<-worker.InterruptCh()
	migrationWorker.Stop()
	cdcWorker.Stop()
	previewWorker.Stop()
	log.Info("pgschema workers stopped")
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func parseBoolEnv(key string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return value == "1" || value == "true" || value == "yes" || value == "y"
}
