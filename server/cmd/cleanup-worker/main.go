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
	"github.com/sanjeev0303/object-storage-platform/internal/service/access"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
	cleanupservice "github.com/sanjeev0303/object-storage-platform/internal/service/cleanup"
	"github.com/sanjeev0303/object-storage-platform/internal/service/erasure"
	eventservice "github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/evidence"
	"github.com/sanjeev0303/object-storage-platform/internal/service/storageconn"
	"github.com/sanjeev0303/object-storage-platform/internal/service/tiering"
	cleanupworker "github.com/sanjeev0303/object-storage-platform/internal/worker/cleanup"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup worker failed:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "run a single cycle, print its report as JSON and exit")
	dryRun := flag.Bool("dry-run", false, "report what a cycle would do and change nothing")
	tenant := flag.String("tenant", "", "limit every phase to one tenant")
	phaseList := flag.String("phases", "all", "phases to run: retention,deletions,scan,sweep,missing or all")
	maxDeletions := flag.Int("max-deletions", 0, "cap the deletions of one cycle (default LIFECYCLE_MAX_DELETIONS)")
	consumeOnce := flag.Bool("consume-once", false, "handle one batch of lifecycle events, print its stats as JSON and exit")
	flag.Parse()

	phases, err := cleanupworker.ParsePhases(*phaseList)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	lc, err := config.LoadLifecycle()
	if err != nil {
		return fmt.Errorf("load lifecycle configuration: %w", err)
	}
	ec, err := config.LoadEvents()
	if err != nil {
		return fmt.Errorf("load event configuration: %w", err)
	}
	sec, err := config.LoadSecurity()
	if err != nil {
		return fmt.Errorf("load security configuration: %w", err)
	}
	catalogConfig, err := config.LoadCatalog()
	if err != nil {
		return fmt.Errorf("load catalog configuration: %w", err)
	}
	if *maxDeletions > 0 {
		lc.MaxDeletionsPerRun = *maxDeletions
	}

	log := logger.New(cfg.AppEnv)
	if *once || *consumeOnce {
		// stdout carries the JSON report; logs go to stderr.
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
	client, err := s3storage.NewClient(startupCtx, storageConfig)
	if err != nil {
		return fmt.Errorf("initialize S3 client: %w", err)
	}
	storage := s3storage.NewObjectStorage(client)

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return fmt.Errorf("S3_BUCKET is required")
	}
	deliveryBucket := lc.PromotionBucket
	if deliveryBucket == "" {
		deliveryBucket = bucket
	}

	owner, err := workerID()
	if err != nil {
		return fmt.Errorf("create worker ID: %w", err)
	}
	registry := metrics.New()
	// Records bound to tenants' storage connections are reached through
	// those connections (Phase 11); without BYOS they do not resolve.
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage connection configuration: %w", err)
	}
	connectionRepo := postgres.NewStorageConnectionRepository(db)
	router, err := storageregistry.ForProcess(storageCfg, storage, bucket, deliveryBucket, lc.PromotionPrefix,
		connectionRepo, registry)
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	sealer, err := storageregistry.Sealer(storageCfg)
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	// Bucket posture and out-of-band changes, checked on a schedule.
	storageHealth, err := storageconn.NewService(storageconn.Dependencies{
		Connections: connectionRepo, Sealer: sealer, Clients: router, Log: log,
		Health: connectionRepo, Alerts: postgres.NewAccessRepository(db), Drift: connectionRepo,
	}, storageCfg, storageregistry.Policy(storageCfg))
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}

	assets := postgres.NewAssetRepository(db)
	uploads := postgres.NewUploadRepository(db)
	jobs := postgres.NewProcessingJobRepository(db)
	holds := postgres.NewLegalHoldRepository(db)
	events := postgres.NewLifecycleEventRepository(db)
	cursors := postgres.NewCursorRepository(db)
	packs := postgres.NewPackRepository(db)

	lifecycle, err := assetservice.NewService(assetservice.Dependencies{
		Assets: assets, Uploads: uploads, Jobs: jobs, Quarantine: postgres.NewQuarantineRepository(db),
		Policies: postgres.NewRetentionPolicyRepository(db), Holds: holds, Deletions: postgres.NewDeletionRepository(db),
		Events: events, Cursors: cursors, Storage: storage, Metrics: registry, Router: router, Packs: packs,
		Locks: postgres.NewObjectLockRepository(db), Shared: assets, LockSync: postgres.NewLockSyncRepository(db), Governance: postgres.NewGovernanceRepository(db),
		Escrow: postgres.NewCopyRepository(db),
	}, assetservice.Config{
		Owner: owner, QuarantineBucket: bucket, DeliveryBucket: deliveryBucket, DeliveryPrefix: lc.PromotionPrefix,
		Platform: assetservice.PlatformRetention{
			DefaultMinRetention:        lc.DefaultMinRetention,
			DefaultMaxAge:              lc.DefaultMaxAge,
			DefaultDeletionGrace:       lc.DefaultDeletionGrace,
			DefaultQuarantineRetention: lc.QuarantineRetention,
			MinRetentionFloor:          lc.MinRetentionFloor,
			DeletionGraceFloor:         lc.DeletionGraceFloor,
			QuarantineRetentionFloor:   lc.QuarantineRetention,
			ComplianceMaxRetention:     lc.ComplianceMaxRetention,
		},
		Signer:    assetservice.Signer{KeyID: lc.CertificateKeyID, Key: lc.CertificateKey},
		BatchSize: lc.BatchSize, Concurrency: lc.Concurrency, Lease: lc.LeaseDuration,
		Retry:           retry.Policy{BaseDelay: lc.BackoffBase, MaxDelay: lc.BackoffMax, MaxAttempts: lc.MaxAttempts},
		RecheckInterval: lc.RecheckInterval, ExpireAssets: lc.ExpireAssets, MaxDeletionsPerRun: lc.MaxDeletionsPerRun,
		TenantID: *tenant, DryRun: *dryRun,
	}, log)
	if err != nil {
		return fmt.Errorf("create lifecycle service: %w", err)
	}
	// Deletions the worker makes for the catalog are audited like a
	// member's.
	auditService := audit.NewService(postgres.NewAuditRepository(db), log, registry, sec.Audit.Retention)
	lifecycle.Guard(auditService)
	catalogService, err := catalog.NewService(catalog.Dependencies{
		Repo: postgres.NewCatalogRepository(db),
		Delete: func(ctx context.Context, p model.Principal, assetID string) (bool, error) {
			result, err := lifecycle.RequestDeletion(ctx, p, assetID)
			return result.Created, err
		},
		Audit: auditService, Metrics: registry, Log: log,
	}, catalog.Config(catalogConfig))
	if err != nil {
		return fmt.Errorf("configure catalog: %w", err)
	}

	cleanup, err := cleanupservice.NewService(cleanupservice.Dependencies{
		Storage: storage, Inventory: postgres.NewInventoryRepository(db), Cursors: cursors, Uploads: uploads,
		Assets: assets, Jobs: jobs, Holds: holds, Events: events, Metrics: registry,
		Router: router, Connections: postgres.NewStorageConnectionRepository(db), Packs: packs, Shared: assets,
	}, cleanupservice.Config{
		Owner: owner, QuarantineBucket: bucket, DeliveryBucket: deliveryBucket, DeliveryPrefix: lc.PromotionPrefix,
		TenantID: *tenant, PageSize: lc.ScanPageSize, PagesPerRun: lc.ScanPages, Lease: lc.LeaseDuration,
		MinAge: lc.OrphanMinAge, MinObservations: lc.OrphanObservations, SafetyInterval: lc.OrphanSafetyInterval,
		FailedUploadRetention: lc.FailedUploadRetention, DeleteOrphans: lc.DeleteOrphans, DeleteUnrecorded: lc.DeleteUnrecorded,
		MaxDeletionsPerRun: lc.MaxDeletionsPerRun, MaxAttempts: lc.MaxAttempts, MaxScanErrors: lc.MaxScanErrors,
		CheckBatch: lc.CheckBatch, DryRun: *dryRun,
	}, log)
	if err != nil {
		return fmt.Errorf("create cleanup service: %w", err)
	}

	// Say plainly what this worker may delete, and whether the buckets
	// keep versions (deleting by key on a versioned bucket keeps the data).
	for _, b := range []string{bucket, deliveryBucket} {
		status, err := storage.VersioningStatus(startupCtx, b)
		if err != nil {
			log.Warn("could not read bucket versioning", "bucket", b, "error", err)
			continue
		}
		if status == "" {
			status = "off"
		}
		log.Info("bucket versioning", "bucket", b, "status", status)
		if deliveryBucket == bucket {
			break
		}
	}
	log.Info("cleanup worker starting",
		"worker_id", owner, "dry_run", *dryRun, "tenant", *tenant, "phases", *phaseList,
		"expiration", mode(lc.ExpireAssets), "orphan_action", mode(lc.DeleteOrphans),
		"orphan_unrecorded", mode(lc.DeleteUnrecorded), "max_deletions_per_cycle", lc.MaxDeletionsPerRun)
	if lc.DeleteUnrecorded {
		log.Warn("LIFECYCLE_ORPHAN_UNRECORDED=delete: objects with no record at all may be removed; " +
			"only safe when this database is the bucket's sole writer")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		report, err := cleanupworker.RunCycle(ctx, lifecycle, cleanup, phases)
		if printErr := printJSON(report); printErr != nil {
			return printErr
		}
		return err
	}

	// Lifecycle events run due deletions and resume held ones at once; the
	// cycles below stay as the safety net. A dry run, a single-tenant run
	// or one without deletions leaves the events to a full worker.
	var consumer *eventservice.Consumer
	if ec.Queue == "sqs" && !*dryRun && *tenant == "" && phases.Deletions {
		handlers := eventservice.LifecycleHandlers(lifecycle.RunDeletion, lifecycle.ResumeAfterHold)
		for eventType, h := range eventservice.CatalogHandlers(catalogService.RetireReplaced) {
			handlers[eventType] = h
		}
		consumer, err = eventConsumer(ctx, db, ec, event.QueueLifecycle, owner, handlers, log, registry)
		if err != nil {
			return err
		}
	}
	if *consumeOnce {
		if consumer == nil {
			return errors.New("consuming events needs EVENT_QUEUE=sqs, no -dry-run or -tenant, and the deletions phase")
		}
		stats, err := consumer.RunOnce(ctx)
		if err != nil {
			return err
		}
		return printJSON(stats)
	}

	if lc.MetricsAddr != "" {
		registry.Publish("lifecycle")
		stopMetrics := metrics.Serve(lc.MetricsAddr, db.Pool.Ping, log)
		defer stopMetrics()
	}
	var wg sync.WaitGroup
	if consumer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			consumer.Run(ctx)
		}()
		log.Info("consuming lifecycle events", "visibility", ec.ConsumerVisibility.String(),
			"concurrency", ec.ConsumerConcurrency, "max_attempts", ec.ConsumerMaxAttempts)
	}
	// Read anomalies, against each member's own history, every five minutes.
	detector := access.NewService(postgres.NewAccessRepository(db), nil, log)
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			if stats, err := detector.Detect(ctx); err != nil {
				log.Warn("read anomaly detection failed", "error", err)
			} else if stats.Alerts > 0 {
				log.Warn("read anomalies", "members", stats.Members, "alerts", stats.Alerts)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			if stats, err := storageHealth.CheckDue(ctx, storageCfg.PostureInterval, 50); err != nil {
				log.Warn("posture checks failed", "error", err)
			} else if stats.Checked > 0 || stats.ExpiredCredentials > 0 {
				log.Info("posture checked", "connections", stats.Checked, "failing", stats.Failing, "errors", stats.Errors,
					"expired_credentials", stats.ExpiredCredentials)
			}
			if stats, err := storageHealth.ScanDrift(ctx, storageCfg.DriftInterval, 20, storageCfg.DriftBudget); err != nil {
				log.Warn("drift scan failed", "error", err)
			} else if stats.Connections > 0 {
				log.Info("drift scanned", "connections", stats.Connections, "objects", stats.Checked, "found", stats.Found, "errors", stats.Errors)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	// Tier rules and archive restores (docs/tiering.md).
	tiers := tiering.New(tiering.Deps{Store: postgres.NewTieringRepository(db), Assets: postgres.NewAssetRepository(db),
		Router: router, Log: log}, tiering.Config{PlatformTiering: os.Getenv("PLATFORM_STORAGE_TIERING") == "true"})
	every := os.Getenv("TIERING_INTERVAL")
	if every == "" {
		every = "10m"
	}
	tierEvery, err := time.ParseDuration(every)
	if err != nil || tierEvery < time.Minute {
		return fmt.Errorf("TIERING_INTERVAL must be a duration of at least 1m")
	}
	go func() {
		ticker := time.NewTicker(tierEvery)
		defer ticker.Stop()
		for {
			if stats, err := tiers.RunOnce(ctx); err != nil {
				log.Warn("tiering pass failed", "error", err)
			} else if stats != (tiering.Stats{}) {
				log.Info("tiering pass", "moved", stats.Moved, "failed", stats.Failed, "restores_started", stats.RestoresStarted,
					"restores_ready", stats.RestoresReady, "restores_failed", stats.RestoresFailed, "restores_expired", stats.RestoresExpired)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	// Audit chain anchors, scheduled reviews and training-summary reminders
	// (docs/evidence.md). Each organization's settings pace its anchors.
	proofs := evidence.New(evidence.Deps{Store: postgres.NewEvidenceRepository(db), Chains: postgres.NewAuditRepository(db),
		Router: router, Alerts: postgres.NewAccessRepository(db), TSA: os.Getenv("AUDIT_TSA_URL"), Log: log,
		Signer: evidence.Signer{KeyID: lc.CertificateKeyID, Key: lc.CertificateKey}})
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			if n, err := proofs.AnchorAll(ctx); err != nil {
				log.Warn("anchoring audit chains failed", "error", err)
			} else if n > 0 {
				log.Info("audit chains anchored", "chains", n)
			}
			if n, err := proofs.DueReviews(ctx); err != nil {
				log.Warn("building audit reviews failed", "error", err)
			} else if n > 0 {
				log.Info("audit reviews built", "reviews", n)
			}
			if n, err := proofs.RemindSummaries(ctx); err != nil {
				log.Warn("training summary reminders failed", "error", err)
			} else if n > 0 {
				log.Info("training summaries due", "models", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	// Erasures: follows each file's deletion, and certifies the erasure
	// once every file is gone (docs/erasure.md).
	erasures := erasure.New(erasure.Deps{Store: postgres.NewErasureRepository(db), Lifecycle: lifecycle, Catalog: catalogService,
		Audit: auditService, Log: log, Signer: erasure.Signer{KeyID: lc.CertificateKeyID, Key: lc.CertificateKey}})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			if n, err := erasures.Progress(ctx); err != nil {
				log.Warn("erasure progress failed", "error", err)
			} else if n > 0 {
				log.Info("erasures completed", "erasures", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	cleanupworker.Run(ctx, lifecycle, cleanup, phases, lc.PollInterval, log)
	wg.Wait()
	log.Info("cleanup worker stopped")
	return nil
}

// eventConsumer connects to the queue and builds its consumer.
func eventConsumer(ctx context.Context, db *postgres.DB, ec config.Events, queueName, owner string,
	handlers map[string]eventservice.Handler, log *slog.Logger, registry *metrics.Registry) (*eventservice.Consumer, error) {
	queue, err := sqsqueue.New(ctx, sqsqueue.Config{
		Region: ec.SQSRegion, Endpoint: ec.SQSEndpoint,
		AccessKeyID: ec.SQSAccessKeyID, SecretAccessKey: ec.SQSSecretAccessKey,
		Prefix: ec.SQSQueuePrefix, MaxReceiveCount: ec.SQSMaxReceiveCount, CreateQueues: ec.SQSCreateQueues,
	}, queueName)
	if err != nil {
		return nil, fmt.Errorf("connect to SQS: %w", err)
	}
	return eventservice.NewConsumer(queue, postgres.NewInboxRepository(db), postgres.NewDeadLetterRepository(db), handlers,
		eventservice.ConsumerConfig{
			Name: queueName, Owner: owner, BatchSize: ec.ConsumerBatch, Concurrency: ec.ConsumerConcurrency,
			Visibility: ec.ConsumerVisibility, Wait: ec.ConsumerWait, HandlerTimeout: ec.HandlerTimeout,
			Retry: retry.Policy{BaseDelay: ec.ConsumerBackoffBase, MaxDelay: ec.ConsumerBackoffMax, MaxAttempts: ec.ConsumerMaxAttempts},
		}, log, registry)
}

func mode(destructive bool) string {
	if destructive {
		return "delete"
	}
	return "report"
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
