package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	sqsqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/sqs"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/scanner/clamav"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	processingservice "github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/promotion"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
	processingworker "github.com/sanjeev0303/object-storage-platform/internal/worker/processing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "processing worker startup failed:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "run a single processing cycle, print its stats as JSON and exit")
	consumeOnce := flag.Bool("consume-once", false, "handle one batch of processing events, print its stats as JSON and exit")
	inspect := flag.String("inspect", "", "print TENANT/ASSET's job, quarantine records and events as JSON and exit")
	requeue := flag.String("requeue", "", "give TENANT/ASSET's EXHAUSTED job a fresh attempt budget and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	workerCfg, err := config.LoadProcessing()
	if err != nil {
		return fmt.Errorf("load processing configuration: %w", err)
	}
	ec, err := config.LoadEvents()
	if err != nil {
		return fmt.Errorf("load event configuration: %w", err)
	}

	log := logger.New(cfg.AppEnv)
	if *once || *consumeOnce || *inspect != "" || *requeue != "" {
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
	destinationBucket := workerCfg.PromotionBucket
	if destinationBucket == "" {
		destinationBucket = bucket
	}

	uploads := postgres.NewUploadRepository(db)
	assets := postgres.NewAssetRepository(db)

	// Verification reuses the upload service's integrity check.
	multipartService, err := uploadservice.NewMultipartService(
		uploads,
		postgres.NewMultipartUploadRepository(db),
		storage,
		uploadservice.MultipartServiceConfig{
			Bucket:     bucket,
			MaxSize:    cfg.UploadMaxSize,
			SessionTTL: cfg.UploadSessionTTL,
			PartURLTTL: cfg.UploadURLTTL,
		},
	)
	if err != nil {
		return fmt.Errorf("create multipart service: %w", err)
	}

	promoter, err := promotion.NewService(assets, storage, promotion.Config{
		SourceBucket:      bucket,
		DestinationBucket: destinationBucket,
		Prefix:            workerCfg.PromotionPrefix,
	})
	if err != nil {
		return fmt.Errorf("create promotion service: %w", err)
	}

	validators := []interfaces.FileValidator{processingservice.BasicValidator{}, processingservice.TypeValidator{}}
	if workerCfg.ArchiveInspection {
		validators = append(validators, processingservice.ArchiveValidator{Limits: processingservice.ArchiveLimits{
			MaxArchiveBytes:      workerCfg.ArchiveMaxBytes,
			MaxEntries:           workerCfg.ArchiveMaxEntries,
			MaxUncompressedBytes: workerCfg.ArchiveMaxExpanded,
			MaxRatio:             workerCfg.ArchiveMaxRatio,
		}})
	}

	registry := metrics.New()
	deps := processingservice.Dependencies{
		Jobs:       postgres.NewProcessingJobRepository(db),
		Assets:     assets,
		Uploads:    uploads,
		Quarantine: postgres.NewQuarantineRepository(db),
		Events:     postgres.NewProcessingEventRepository(db),
		Policies:   postgres.NewContentPolicyRepository(db),
		// Legal holds stop quarantine purges; a tenant policy may keep
		// rejected uploads longer than QUARANTINE_RETENTION.
		Holds: postgres.NewLegalHoldRepository(db),
		Retention: assetservice.NewPolicyResolver(postgres.NewRetentionPolicyRepository(db), assetservice.PlatformRetention{
			DefaultQuarantineRetention: workerCfg.QuarantineRetention,
			QuarantineRetentionFloor:   workerCfg.QuarantineRetention,
		}),
		Verifier:   multipartService,
		Storage:    storage,
		Validators: validators,
		Promoter:   promoter,
		Metrics:    registry,
	}
	if workerCfg.Scanner == "clamav" {
		deps.Scanner = clamav.New(workerCfg.ClamAVAddress, workerCfg.ClamAVTimeout)
	}

	owner, err := workerID()
	if err != nil {
		return fmt.Errorf("create worker ID: %w", err)
	}

	service, err := processingservice.NewService(deps, processingservice.Config{
		Owner:       owner,
		Bucket:      bucket,
		BatchSize:   workerCfg.BatchSize,
		Concurrency: workerCfg.Concurrency,
		Lease:       workerCfg.LeaseDuration,
		Policy: model.ContentPolicy{
			AllowedTypes:      workerCfg.AllowedTypes,
			DeniedTypes:       workerCfg.DeniedTypes,
			MaxSizeBytes:      workerCfg.MaxFileBytes,
			UnknownTypeAction: workerCfg.UnknownType,
		},
		ScanPolicy:          processingservice.ScanPolicy(workerCfg.ScanPolicy),
		ScanTimeout:         workerCfg.ClamAVTimeout,
		QuarantineRetention: workerCfg.QuarantineRetention,
		Retry: retry.Policy{
			BaseDelay:   workerCfg.BackoffBase,
			MaxDelay:    workerCfg.BackoffMax,
			MaxAttempts: workerCfg.MaxAttempts,
		},
	}, log)
	if err != nil {
		return fmt.Errorf("create processing service: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *inspect != "" {
		tenantID, assetID, err := splitAssetRef(*inspect)
		if err != nil {
			return err
		}
		inspection, err := service.Inspect(ctx, tenantID, assetID)
		if err != nil {
			return fmt.Errorf("inspect asset: %w", err)
		}
		return printJSON(inspection)
	}
	if *requeue != "" {
		tenantID, assetID, err := splitAssetRef(*requeue)
		if err != nil {
			return err
		}
		job, err := service.Requeue(ctx, tenantID, assetID, "operator:cli")
		if err != nil {
			return fmt.Errorf("requeue asset: %w", err)
		}
		return printJSON(job)
	}

	if deps.Scanner != nil {
		pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
		if err := deps.Scanner.Ping(pingCtx); err != nil {
			registry.Set("scanner_up", 0)
			log.Warn("malware scanner is not answering; scans will retry and assets stay quarantined",
				"scanner", workerCfg.Scanner, "address", workerCfg.ClamAVAddress, "error", err)
		} else {
			registry.Set("scanner_up", 1)
			log.Info("malware scanner is healthy", "scanner", workerCfg.Scanner)
		}
		cancelPing()
	}

	if *once {
		stats, err := service.RunOnce(ctx)
		if err != nil {
			return err
		}
		return printJSON(stats)
	}

	// upload.completed events start processing at once; the poll below
	// stays as the safety net for anything an event did not reach.
	var consumer *events.Consumer
	if ec.Queue == "sqs" {
		handlers := events.ProcessingHandlers(service.Intake)
		consumer, err = eventConsumer(ctx, db, ec, event.QueueProcessing, owner, handlers, log, registry)
		if err != nil {
			return err
		}
	}
	if *consumeOnce {
		if consumer == nil {
			return errors.New("EVENT_QUEUE=sqs is required to consume events")
		}
		stats, err := consumer.RunOnce(ctx)
		if err != nil {
			return err
		}
		return printJSON(stats)
	}

	if workerCfg.MetricsAddr != "" {
		registry.Publish("processing")
		// expvar serves /debug/vars on the default mux.
		go func() {
			if err := http.ListenAndServe(workerCfg.MetricsAddr, nil); err != nil {
				log.Error("metrics listener stopped", "error", err)
			}
		}()
	}

	log.Info("processing worker starting",
		"worker_id", owner,
		"scanner", workerCfg.Scanner,
		"scan_unavailable_policy", workerCfg.ScanPolicy,
		"concurrency", workerCfg.Concurrency,
		"batch_size", workerCfg.BatchSize,
		"lease", workerCfg.LeaseDuration.String(),
		"archive_inspection", workerCfg.ArchiveInspection,
		"unknown_type_action", workerCfg.UnknownType,
		"quarantine_retention", workerCfg.QuarantineRetention.String(),
	)
	if workerCfg.Scanner == "none" {
		log.Warn("no malware scanner configured; assets will not be promoted unless the policy allows it",
			"scan_unavailable_policy", workerCfg.ScanPolicy)
	}

	var wg sync.WaitGroup
	if consumer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			consumer.Run(ctx)
		}()
		log.Info("consuming processing events", "visibility", ec.ConsumerVisibility.String(),
			"concurrency", ec.ConsumerConcurrency, "max_attempts", ec.ConsumerMaxAttempts)
	}
	processingworker.Run(ctx, service, workerCfg.PollInterval, log)
	wg.Wait()
	log.Info("processing worker stopped")
	return nil
}

// eventConsumer connects to the queue and builds its consumer.
func eventConsumer(ctx context.Context, db *postgres.DB, ec config.Events, queueName, owner string,
	handlers map[string]events.Handler, log *slog.Logger, registry *metrics.Registry) (*events.Consumer, error) {
	queue, err := sqsqueue.New(ctx, sqsqueue.Config{
		Region: ec.SQSRegion, Endpoint: ec.SQSEndpoint,
		AccessKeyID: ec.SQSAccessKeyID, SecretAccessKey: ec.SQSSecretAccessKey,
		Prefix: ec.SQSQueuePrefix, MaxReceiveCount: ec.SQSMaxReceiveCount, CreateQueues: ec.SQSCreateQueues,
	}, queueName)
	if err != nil {
		return nil, fmt.Errorf("connect to SQS: %w", err)
	}
	return events.NewConsumer(queue, postgres.NewInboxRepository(db), postgres.NewDeadLetterRepository(db), handlers,
		events.ConsumerConfig{
			Name: queueName, Owner: owner, BatchSize: ec.ConsumerBatch, Concurrency: ec.ConsumerConcurrency,
			Visibility: ec.ConsumerVisibility, Wait: ec.ConsumerWait, HandlerTimeout: ec.HandlerTimeout,
			Retry: retry.Policy{BaseDelay: ec.ConsumerBackoffBase, MaxDelay: ec.ConsumerBackoffMax, MaxAttempts: ec.ConsumerMaxAttempts},
		}, log, registry)
}

func splitAssetRef(ref string) (string, string, error) {
	tenantID, assetID, ok := strings.Cut(ref, "/")
	if !ok || tenantID == "" || assetID == "" {
		return "", "", fmt.Errorf("expected TENANT/ASSET, got %q", ref)
	}
	return tenantID, assetID, nil
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
