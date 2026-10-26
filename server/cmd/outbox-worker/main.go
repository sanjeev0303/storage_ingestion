// Command outbox-worker publishes committed outbox events to the queue and
// runs event reconciliation.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	sqsqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/sqs"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	outboxworker "github.com/sanjeev0303/object-storage-platform/internal/worker/outbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "outbox worker failed:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "publish one batch, reconcile once, print both as JSON and exit")
	dryRun := flag.Bool("dry-run", false, "reconcile without changing anything (with -once, publish nothing either)")
	tenant := flag.String("tenant", "", "limit publishing and reconciliation to one tenant")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ec, err := config.LoadEvents()
	if err != nil {
		return fmt.Errorf("load event configuration: %w", err)
	}
	if ec.Queue != "sqs" {
		return errors.New("EVENT_QUEUE=sqs is required: there is no queue to publish to")
	}

	log := logger.New(cfg.AppEnv)
	if *once {
		// stdout carries the JSON report; logs go to stderr.
		log = logger.NewTo(os.Stderr, cfg.AppEnv)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()

	db, err := postgres.Open(startupCtx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	queues := []string{event.QueueProcessing, event.QueueLifecycle, event.QueueIngestion, event.QueueDatasets, event.QueuePacking}
	queue, err := sqsqueue.New(startupCtx, sqsqueue.Config{
		Region: ec.SQSRegion, Endpoint: ec.SQSEndpoint,
		AccessKeyID: ec.SQSAccessKeyID, SecretAccessKey: ec.SQSSecretAccessKey,
		Prefix: ec.SQSQueuePrefix, MaxReceiveCount: ec.SQSMaxReceiveCount, CreateQueues: ec.SQSCreateQueues,
	}, queues...)
	if err != nil {
		return fmt.Errorf("connect to SQS: %w", err)
	}

	owner, err := workerID()
	if err != nil {
		return fmt.Errorf("create worker ID: %w", err)
	}
	registry := metrics.New()
	outbox := postgres.NewOutboxRepository(db)

	publisher, err := events.NewPublisher(outbox, queue, events.PublisherConfig{
		Owner: owner, BatchSize: ec.PublishBatch, Concurrency: ec.PublishConcurrency,
		Lease: ec.PublishLease, PublishTimeout: ec.PublishTimeout, TenantID: *tenant,
		Retry: retry.Policy{BaseDelay: ec.PublishBackoffBase, MaxDelay: ec.PublishBackoffMax, MaxAttempts: ec.PublishMaxAttempts},
	}, log, registry)
	if err != nil {
		return fmt.Errorf("create publisher: %w", err)
	}

	checks := events.Checks{
		Uploads: postgres.NewUploadRepository(db), Assets: postgres.NewAssetRepository(db),
		Jobs: postgres.NewProcessingJobRepository(db), Deletions: postgres.NewDeletionRepository(db),
		Holds: postgres.NewLegalHoldRepository(db), Ingestion: postgres.NewIngestionRepository(db),
		Datasets: postgres.NewDatasetRepository(db), Packs: postgres.NewPackRepository(db),
	}
	reconciler, err := events.NewReconciler(outbox, postgres.NewInboxRepository(db), postgres.NewDeadLetterRepository(db),
		postgres.NewDiscrepancyRepository(db), postgres.NewCursorRepository(db), queue, checks,
		events.ReconcilerConfig{
			Queues: queues, StuckAfter: ec.StuckAfter, Window: ec.ReconcileWindow, MaxRepublish: ec.MaxRepublish,
			BatchSize: ec.ReconcileBatch, TenantID: *tenant, DryRun: *dryRun,
		}, log, registry)
	if err != nil {
		return fmt.Errorf("create reconciler: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		var result struct {
			Publish   events.PublishStats
			Reconcile events.ReconcileReport
		}
		if !*dryRun {
			if result.Publish, err = publisher.RunOnce(ctx); err != nil {
				return err
			}
		}
		result.Reconcile, err = reconciler.RunOnce(ctx)
		if printErr := printJSON(result); printErr != nil {
			return printErr
		}
		return err
	}

	if ec.MetricsAddr != "" {
		registry.Publish("events")
		stopMetrics := metrics.Serve(ec.MetricsAddr, db.Pool.Ping, log)
		defer stopMetrics()
	}

	log.Info("outbox worker starting", "worker_id", owner, "tenant", *tenant, "reconcile_dry_run", *dryRun,
		"batch_size", ec.PublishBatch, "concurrency", ec.PublishConcurrency, "lease", ec.PublishLease.String(),
		"max_attempts", ec.PublishMaxAttempts, "reconcile_interval", ec.ReconcileInterval.String())
	outboxworker.Run(ctx, publisher, reconciler, ec.PublishInterval, ec.ReconcileInterval, log)
	log.Info("outbox worker stopped")
	return nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// workerID names this process on the leases it takes.
func workerID() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(suffix[:])), nil
}
