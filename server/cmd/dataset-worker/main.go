// Command dataset-worker runs managed datasets' background work: manifest
// imports, validation jobs and readiness assessments, recovery of jobs a
// stopped worker left, and the release of datasets left VALIDATING. With
// EVENT_QUEUE=sqs it also consumes the datasets queue; the poll keeps
// running as the safety net either way.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	sqsqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/sqs"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
	datasetworker "github.com/sanjeev0303/object-storage-platform/internal/worker/dataset"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dataset worker failed:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "run every sweep once, print what each did as JSON and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	dc, err := config.LoadDatasets()
	if err != nil {
		return fmt.Errorf("load dataset configuration: %w", err)
	}
	ec, err := config.LoadEvents()
	if err != nil {
		return fmt.Errorf("load event configuration: %w", err)
	}
	sec, err := config.LoadSecurity()
	if err != nil {
		return fmt.Errorf("load security configuration: %w", err)
	}
	log := logger.New(cfg.AppEnv)
	if *once {
		// stdout carries the JSON output; logs go to stderr.
		log = logger.NewTo(os.Stderr, cfg.AppEnv)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()
	db, err := postgres.Open(startupCtx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	storageConfig, err := s3storage.LoadConfig()
	if err != nil {
		return fmt.Errorf("load object storage configuration: %w", err)
	}
	client, err := s3storage.NewClient(startupCtx, storageConfig)
	if err != nil {
		return fmt.Errorf("initialize S3 client: %w", err)
	}
	storage := s3storage.NewObjectStorage(client)
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return fmt.Errorf("S3_BUCKET is required")
	}
	// Published versions live in the delivery bucket.
	contentBucket := bucket
	if delivery, err := config.LoadDelivery(); err == nil && delivery.Bucket != "" {
		contentBucket = delivery.Bucket
	}

	registry := metrics.New()
	auditService := audit.NewService(postgres.NewAuditRepository(db), log, registry, sec.Audit.Retention)
	svc, err := datasetservice.NewService(datasetservice.Dependencies{
		Repo: postgres.NewDatasetRepository(db), Manifests: storage, ManifestBucket: bucket,
		Content: storage, ContentBucket: contentBucket,
		Quotas: quota.NewService(postgres.NewQuotaRepository(db), sec.Quotas, auditService, log),
		Audit:  auditService, Metrics: registry, Log: log,
	}, dc)
	if err != nil {
		return fmt.Errorf("create dataset service: %w", err)
	}
	owner, err := workerID()
	if err != nil {
		return err
	}

	if *once {
		return printJSON(datasetworker.RunOnce(context.Background(), svc, owner, dc.WorkerSlots, log))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if dc.MetricsAddr != "" {
		registry.Publish("datasets")
		// /health answers 200 while the database answers.
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := db.Pool.Ping(pingCtx); err != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok\n"))
		})
		go func() {
			if err := http.ListenAndServe(dc.MetricsAddr, nil); err != nil {
				log.Error("metrics listener stopped", "error", err)
			}
		}()
	}
	if ec.Queue == "sqs" {
		consumer, err := datasetConsumer(startupCtx, db, ec, dc, svc, owner, log, registry)
		if err != nil {
			return err
		}
		go consumer.Run(ctx)
		log.Info("dataset consumer started", "queue", event.QueueDatasets)
	}

	log.Info("dataset worker started", "owner", owner, "poll_interval", dc.PollInterval, "slots", dc.WorkerSlots)
	datasetworker.Run(ctx, svc, owner, dc.WorkerSlots, dc.PollInterval, log)
	log.Info("dataset worker stopped")
	return nil
}

// datasetConsumer connects to the queue and builds the datasets queue's
// consumer. A handler runs an import or validation for at most its
// budget, so its timeout is at least a lease.
func datasetConsumer(ctx context.Context, db *postgres.DB, ec config.Events, dc config.Datasets, svc *datasetservice.Service,
	owner string, log *slog.Logger, registry *metrics.Registry) (*events.Consumer, error) {
	queue, err := sqsqueue.New(ctx, sqsqueue.Config{
		Region: ec.SQSRegion, Endpoint: ec.SQSEndpoint,
		AccessKeyID: ec.SQSAccessKeyID, SecretAccessKey: ec.SQSSecretAccessKey,
		Prefix: ec.SQSQueuePrefix, MaxReceiveCount: ec.SQSMaxReceiveCount, CreateQueues: ec.SQSCreateQueues,
	}, event.QueueDatasets)
	if err != nil {
		return nil, fmt.Errorf("connect to SQS: %w", err)
	}
	handlers := events.DatasetHandlers(
		func(ctx context.Context, tenantID, importID string) (string, error) {
			return svc.RunImport(ctx, owner, tenantID, importID)
		},
		func(ctx context.Context, tenantID, jobID string) (string, error) {
			return svc.RunValidation(ctx, owner, tenantID, jobID)
		},
	)
	return events.NewConsumer(queue, postgres.NewInboxRepository(db), postgres.NewDeadLetterRepository(db), handlers,
		events.ConsumerConfig{
			Name: event.QueueDatasets, Owner: owner, BatchSize: ec.ConsumerBatch, Concurrency: min(ec.ConsumerConcurrency, dc.WorkerSlots),
			Visibility: max(ec.ConsumerVisibility, dc.Lease), Wait: ec.ConsumerWait, HandlerTimeout: max(ec.HandlerTimeout, dc.Lease),
			Retry: retry.Policy{BaseDelay: ec.ConsumerBackoffBase, MaxDelay: ec.ConsumerBackoffMax, MaxAttempts: ec.ConsumerMaxAttempts},
		}, log, registry)
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
