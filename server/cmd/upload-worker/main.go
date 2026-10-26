// Command upload-worker runs bulk ingestion's background work: reading
// staged manifests, expiring abandoned upload leases, retrying entries,
// recording processing outcomes, advancing cancellations and reconciling
// counters. With EVENT_QUEUE=sqs it also consumes the ingestion queue; the
// polls keep running as the safety net either way.
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
	"sync"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	sqsqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/sqs"
	storageregistry "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
	uploadworker "github.com/sanjeev0303/object-storage-platform/internal/worker/upload"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "upload worker failed:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "run every sweep once, print what each did as JSON and exit")
	consumeOnce := flag.Bool("consume-once", false, "handle one batch of ingestion events, print its stats as JSON and exit")
	reconcile := flag.Bool("reconcile", false, "recount job counters and tenant usage, print the report as JSON and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ic, err := config.LoadIngestion()
	if err != nil {
		return fmt.Errorf("load ingestion configuration: %w", err)
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
	if *once || *consumeOnce || *reconcile {
		// stdout carries the JSON output; logs go to stderr.
		log = logger.NewTo(os.Stderr, cfg.AppEnv)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
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
	s3Client, err := s3storage.NewClient(startupCtx, storageConfig)
	if err != nil {
		return fmt.Errorf("initialize S3 client: %w", err)
	}
	storage := s3storage.NewObjectStorage(s3Client)
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return fmt.Errorf("S3_BUCKET is required")
	}

	uploads := postgres.NewUploadRepository(db)
	assets := postgres.NewAssetRepository(db)
	jobs := postgres.NewProcessingJobRepository(db)
	processingEvents := postgres.NewProcessingEventRepository(db)
	multipart, err := uploadservice.NewMultipartService(uploads, postgres.NewMultipartUploadRepository(db), storage,
		uploadservice.MultipartServiceConfig{
			Bucket: bucket, MaxSize: cfg.UploadMaxSize, SessionTTL: cfg.UploadSessionTTL, PartURLTTL: cfg.UploadURLTTL,
		})
	if err != nil {
		return fmt.Errorf("create multipart service: %w", err)
	}

	registry := metrics.New()
	// Records bound to tenants' storage connections are reached through
	// those connections (Phase 11); without BYOS they do not resolve.
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage connection configuration: %w", err)
	}
	router, err := storageregistry.ForProcess(storageCfg, storage, bucket, bucket, config.PromotionPrefix(),
		postgres.NewStorageConnectionRepository(db), registry)
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	multipart.Route(router, router)

	auditService := audit.NewService(postgres.NewAuditRepository(db), log, registry, sec.Audit.Retention)
	quotas := quota.NewService(postgres.NewQuotaRepository(db), sec.Quotas, auditService, log)
	svc, err := ingestion.NewService(ingestion.Dependencies{
		Repo:      postgres.NewIngestionRepository(db),
		Uploads:   uploadservice.NewService(uploads, storage, bucket, cfg.UploadMaxSize, cfg.UploadSessionTTL).Route(router, router),
		Multipart: multipart, Sessions: uploads, Assets: assets, Jobs: jobs,
		Requeue: func(ctx context.Context, tenantID, assetID, actor string) error {
			return requeue(ctx, assets, jobs, processingEvents, tenantID, assetID, actor)
		},
		Manifests: storage, Bucket: bucket, Log: log, Metrics: registry, Audit: auditService,
	}, ic)
	if err != nil {
		return fmt.Errorf("create ingestion service: %w", err)
	}

	owner, err := workerID()
	if err != nil {
		return err
	}

	switch {
	case *once:
		return printJSON(uploadworker.RunOnce(context.Background(), svc, owner, log))
	case *reconcile:
		report, err := svc.Reconcile(context.Background())
		if err != nil {
			return err
		}
		usage, err := quotas.ReconcileAll(context.Background())
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ingestion": report, "storage_usage": usage})
	case *consumeOnce:
		if ec.Queue != "sqs" {
			return fmt.Errorf("-consume-once needs EVENT_QUEUE=sqs")
		}
		consumer, err := ingestionConsumer(startupCtx, db, ec, svc, owner, log, registry)
		if err != nil {
			return err
		}
		stats, err := consumer.RunOnce(context.Background())
		if err != nil {
			return err
		}
		return printJSON(stats)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if ic.MetricsAddr != "" {
		registry.Publish("ingestion")
		stopMetrics := metrics.Serve(ic.MetricsAddr, db.Pool.Ping, log)
		defer stopMetrics()
	}
	// The consumer is waited for on shutdown, so no handler is cut off while
	// the database it uses is closed.
	var wg sync.WaitGroup
	if ec.Queue == "sqs" {
		consumer, err := ingestionConsumer(startupCtx, db, ec, svc, owner, log, registry)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			consumer.Run(ctx)
		}()
		log.Info("ingestion consumer started", "queue", event.QueueIngestion)
	}

	log.Info("upload worker started", "owner", owner, "poll_interval", ic.PollInterval,
		"global_upload_slots", ic.GlobalUploadSlots, "small_file_threshold", ic.SmallFileThreshold)
	uploadworker.Run(ctx, svc, quotas, owner, ic.PollInterval, ic.ReconcileInterval, log)
	wg.Wait()
	log.Info("upload worker stopped")
	return nil
}

// requeue gives a held processing job a fresh budget, as the processing
// service's Requeue does: only for an asset still waiting to be processed,
// and recorded in the processing audit trail.
func requeue(ctx context.Context, assets *postgres.AssetRepository, jobs *postgres.ProcessingJobRepository,
	trail *postgres.ProcessingEventRepository, tenantID, assetID, actor string) error {
	asset, err := assets.GetByID(ctx, tenantID, assetID)
	if err != nil {
		return err
	}
	if asset.Status != model.AssetStatusQuarantined && asset.Status != model.AssetStatusProcessing {
		return model.ErrInvalidAssetState
	}
	job, err := jobs.Requeue(ctx, tenantID, assetID, time.Now().UTC())
	if err != nil {
		return err
	}
	return trail.Record(ctx, model.ProcessingEvent{
		TenantID: job.TenantID, AssetID: job.AssetID, JobID: job.ID,
		Event: "requeued", Actor: actor, CreatedAt: time.Now().UTC(),
	})
}

// ingestionConsumer connects to the queue and builds the ingestion
// queue's consumer.
func ingestionConsumer(ctx context.Context, db *postgres.DB, ec config.Events, svc *ingestion.Service, owner string,
	log *slog.Logger, registry *metrics.Registry) (*events.Consumer, error) {
	queue, err := sqsqueue.New(ctx, sqsqueue.Config{
		Region: ec.SQSRegion, Endpoint: ec.SQSEndpoint,
		AccessKeyID: ec.SQSAccessKeyID, SecretAccessKey: ec.SQSSecretAccessKey,
		Prefix: ec.SQSQueuePrefix, MaxReceiveCount: ec.SQSMaxReceiveCount, CreateQueues: ec.SQSCreateQueues,
	}, event.QueueIngestion)
	if err != nil {
		return nil, fmt.Errorf("connect to SQS: %w", err)
	}
	handlers := events.IngestionHandlers(
		func(ctx context.Context, _, jobID string) (string, error) {
			return svc.ReadStagedManifest(ctx, owner, jobID)
		},
		svc.OnUploadProcessed,
	)
	return events.NewConsumer(queue, postgres.NewInboxRepository(db), postgres.NewDeadLetterRepository(db), handlers,
		events.ConsumerConfig{
			Name: event.QueueIngestion, Owner: owner, BatchSize: ec.ConsumerBatch, Concurrency: ec.ConsumerConcurrency,
			Visibility: ec.ConsumerVisibility, Wait: ec.ConsumerWait, HandlerTimeout: ec.HandlerTimeout,
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
