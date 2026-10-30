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
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	sqsqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/sqs"
	storageregistry "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/copyengine"
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
	// Records bound to tenants' storage connections are reached through
	// those connections (Phase 11); without BYOS they do not resolve.
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage connection configuration: %w", err)
	}
	router, err := storageregistry.ForProcess(storageCfg, storage, bucket, contentBucket, config.PromotionPrefix(),
		postgres.NewStorageConnectionRepository(db), registry)
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	auditService := audit.NewService(postgres.NewAuditRepository(db), log, registry, sec.Audit.Retention)
	svc, err := datasetservice.NewService(datasetservice.Dependencies{
		Repo: postgres.NewDatasetRepository(db), Manifests: storage, ManifestBucket: bucket,
		Content: storage, ContentBucket: contentBucket, Router: router, Packs: postgres.NewPackRepository(db),
		Quotas: quota.NewService(postgres.NewQuotaRepository(db), sec.Quotas, auditService, log),
		Audit:  auditService, Metrics: registry, Log: log, Quality: postgres.NewQualityRepository(db),
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
		stopMetrics := metrics.Serve(dc.MetricsAddr, db.Pool.Ping, log)
		defer stopMetrics()
	}
	// The consumer is waited for on shutdown, so no handler is cut off while
	// the database it uses is closed.
	var wg sync.WaitGroup
	if ec.Queue == "sqs" {
		consumer, err := datasetConsumer(startupCtx, db, ec, dc, svc, owner, log, registry)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			consumer.Run(ctx)
		}()
		log.Info("dataset consumer started", "queue", event.QueueDatasets)
	}

	// The copy engine: jobs, replicas, scrubs and schedules.
	massDeletions := 100
	if v, err := strconv.Atoi(os.Getenv("COPY_MASS_DELETION_ALERT")); err == nil && v >= 0 {
		massDeletions = v
	}
	copies := copyengine.NewService(copyengine.Dependencies{
		Store: postgres.NewCopyRepository(db), Router: router, Packs: postgres.NewPackRepository(db),
		Locks: postgres.NewObjectLockRepository(db), Posture: postgres.NewStorageConnectionRepository(db),
		Alerts: postgres.NewAccessRepository(db), Audit: auditService,
	}, copyengine.Config{Owner: owner, MassDeletions: massDeletions}, log)
	wg.Add(1)
	go func() {
		defer wg.Done()
		runCopies(ctx, copies, log)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		runQuality(ctx, svc, owner, log)
	}()

	log.Info("dataset worker started", "owner", owner, "poll_interval", dc.PollInterval, "slots", dc.WorkerSlots)
	datasetworker.Run(ctx, svc, owner, dc.WorkerSlots, dc.PollInterval, log)
	wg.Wait()
	log.Info("dataset worker stopped")
	return nil
}

// runCopies advances copy jobs, then makes replicas, scrubs and runs the
// schedules, every 30 seconds until ctx ends.
func runCopies(ctx context.Context, copies *copyengine.Service, log *slog.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		for i := 0; i < 20 && ctx.Err() == nil; i++ {
			found, err := copies.RunOnce(ctx)
			if err != nil {
				log.Warn("copy job pass failed", "error", err)
			}
			if !found || err != nil {
				break
			}
		}
		if stats, err := copies.Replicate(ctx); err != nil {
			log.Warn("replication pass failed", "error", err)
		} else if stats.Copied+stats.Failed > 0 {
			log.Info("replicated", "copied", stats.Copied, "failed", stats.Failed, "bytes", stats.Bytes)
		}
		if stats, err := copies.Scrub(ctx); err != nil {
			log.Warn("scrub pass failed", "error", err)
		} else if stats.Damaged > 0 {
			log.Warn("scrub found damage", "checked", stats.Checked, "damaged", stats.Damaged, "repaired", stats.Repaired)
		}
		if err := copies.RunSchedules(ctx); err != nil {
			log.Warn("copy schedules failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runQuality profiles queued versions one after another, and compares
// runs' evaluation inputs with their training inputs, until ctx ends.
func runQuality(ctx context.Context, svc *datasetservice.Service, owner string, log *slog.Logger) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil {
			found, err := svc.ProfileOnce(ctx, owner)
			if err != nil {
				log.Warn("profiling failed", "error", err)
			}
			if !found || err != nil {
				break
			}
		}
		if n, err := svc.CheckRunsOnce(ctx); err != nil {
			log.Warn("leakage checks failed", "error", err)
		} else if n > 0 {
			log.Info("runs checked for leakage", "runs", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
