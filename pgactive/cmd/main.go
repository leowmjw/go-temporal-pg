package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"company.com/infra/pgactive-upgrade/internal/activities"
	"company.com/infra/pgactive-upgrade/internal/workflow"
)

func main() {
	ctx := context.Background()

	// Setup structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: true,
	}))
	slog.SetDefault(logger)

	logger.Info("Starting pgactive upgrade service")

	// Setup AWS clients
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		logger.Error("Failed to load AWS config", slog.String("error", err.Error()))
		os.Exit(1)
	}

	rdsClient := rds.NewFromConfig(cfg)
	secretsClient := secretsmanager.NewFromConfig(cfg)

	// Setup Temporal client
	temporalHost := os.Getenv("TEMPORAL_HOST")
	if temporalHost == "" {
		temporalHost = "localhost:7233"
	}

	temporalClient, err := client.Dial(client.Options{
		HostPort: temporalHost,
	})
	if err != nil {
		logger.Error("Failed to connect to Temporal", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer temporalClient.Close()

	// Setup worker
	w := worker.New(temporalClient, workflow.TaskQueue, worker.Options{
		EnableLoggingInReplay: true,
	})

	// Register workflow
	w.RegisterWorkflow(workflow.RollingUpgradeWorkflow)

	// Register activities
	activities := activities.NewActivities(rdsClient, secretsClient, logger)
	w.RegisterActivity(activities.ValidateInput)
	w.RegisterActivity(activities.ProvisionTargetDB)
	w.RegisterActivity(activities.ConfigurePgactiveParams)
	w.RegisterActivity(activities.InstallPgactiveExtension)
	w.RegisterActivity(activities.WaitForSync)
	w.RegisterActivity(activities.TrafficShiftPhase)
	w.RegisterActivity(activities.RunHealthChecks)
	w.RegisterActivity(activities.Cutover)
	w.RegisterActivity(activities.OptionallyDetachOld)
	w.RegisterActivity(activities.DecommissionSource)
	w.RegisterActivity(activities.ExecuteRollback)

	logger.Info("Starting Temporal worker", slog.String("task_queue", workflow.TaskQueue))

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		logger.Info("Received shutdown signal")
		cancel()
	}()

	// Start worker
	err = w.Run(worker.InterruptCh())
	if err != nil {
		logger.Error("Worker failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	logger.Info("pgactive upgrade service stopped")
}
