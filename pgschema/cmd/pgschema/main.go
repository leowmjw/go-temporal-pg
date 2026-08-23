// cmd/pgschema is the CLI entry-point for the pgschema worker.
// Running this binary starts the Temporal worker for all three task queues:
//   - pgschema-migration  (SchemaMigrationWorkflow)
//   - pgschema-cdc        (CDCStreamWorkflow)
//   - pgschema-preview    (PreviewCloneWorkflow)
package main

import (
	"log/slog"
	"os"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/leowmjw/go-temporal-pg/pgschema/internal/activities"
	"github.com/leowmjw/go-temporal-pg/pgschema/internal/workflow"
)

func main() {
	// Go 1.26: slog.NewMultiHandler broadcasts to JSON (for log aggregators) and
	// text (for human terminals) simultaneously.
	log := slog.New(slog.NewMultiHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:     slog.LevelInfo,
			AddSource: true,
		}),
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelWarn,
		}),
	))
	slog.SetDefault(log)

	c, err := client.Dial(client.Options{})
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

	migrationWorker := worker.New(c, workflow.SchemaMigrationTaskQueue, worker.Options{})
	migrationWorker.RegisterWorkflow(workflow.SchemaMigrationWorkflow)
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

	worker.InterruptCh()
	migrationWorker.Stop()
	cdcWorker.Stop()
	previewWorker.Stop()
	log.Info("pgschema workers stopped")
}
