package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	httpapi "github.com/sanjeev0303/object-storage-platform/internal/handler/http"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/cdn/cloudfront"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	storageregistry "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/access"
	"github.com/sanjeev0303/object-storage-platform/internal/service/adoption"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
	"github.com/sanjeev0303/object-storage-platform/internal/service/copyengine"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	"github.com/sanjeev0303/object-storage-platform/internal/service/erasure"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/evidence"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
	ingestionservice "github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
	"github.com/sanjeev0303/object-storage-platform/internal/service/storageconn"
	"github.com/sanjeev0303/object-storage-platform/internal/service/streaming"
	"github.com/sanjeev0303/object-storage-platform/internal/service/tiering"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
	"github.com/sanjeev0303/object-storage-platform/internal/service/webhook"
)

// newIngestion builds the bulk-ingestion service behind the dataset and
// ingestion routes. Requeueing held processing is left to the upload
// worker, which runs processing.
func newIngestion(
	db *postgres.DB,
	uploads *postgres.UploadRepository,
	single *uploadservice.Service,
	multipart *uploadservice.MultipartService,
	storage *s3storage.ObjectStorage,
	bucket string,
	recorder interfaces.AuditRecorder,
	log *slog.Logger,
) (*ingestionservice.Service, error) {
	cfg, err := config.LoadIngestion()
	if err != nil {
		return nil, err
	}
	registry := metrics.New()
	registry.Publish("ingestion")
	return ingestionservice.NewService(ingestionservice.Dependencies{
		Repo: postgres.NewIngestionRepository(db), Uploads: single, Multipart: multipart, Sessions: uploads,
		Assets: postgres.NewAssetRepository(db), Jobs: postgres.NewProcessingJobRepository(db),
		Manifests: storage, Bucket: bucket, Log: log, Metrics: registry, Audit: recorder,
	}, cfg)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "API startup failed:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	// Invalid security settings stop the API here: there is no weaker
	// fallback and no development identity.
	sec, err := config.LoadSecurity()
	if err != nil {
		return fmt.Errorf("load security configuration: %w", err)
	}

	log := logger.New(cfg.AppEnv)

	startupCtx, cancelStartup := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
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

	objectStorage := s3storage.NewObjectStorage(s3Client)

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return fmt.Errorf("S3_BUCKET is required")
	}

	securityMetrics := metrics.New()
	auditService := audit.NewService(postgres.NewAuditRepository(db), log, securityMetrics, sec.Audit.Retention)
	limiter := ratelimit.New()
	identityService, err := identity.NewService(identity.Dependencies{
		Users: postgres.NewIdentityRepository(db), Tenancy: postgres.NewTenancyRepository(db),
		Audit: auditService, Limiter: limiter, Log: log,
	}, sec)
	if err != nil {
		return fmt.Errorf("configure identity: %w", err)
	}
	quotaService := quota.NewService(postgres.NewQuotaRepository(db), sec.Quotas, auditService, log)

	uploadRepository := postgres.NewUploadRepository(db)
	uploadService := uploadservice.NewService(
		uploadRepository,
		objectStorage,
		bucket,
		cfg.UploadMaxSize,
		cfg.UploadSessionTTL,
	).Guard(quotaService, auditService).ReportAssets(postgres.NewAssetRepository(db))

	multipartRepository := postgres.NewMultipartUploadRepository(db)

	multipartService, err := uploadservice.NewMultipartService(
		uploadRepository,
		multipartRepository,
		objectStorage,
		uploadservice.MultipartServiceConfig{
			Bucket:     bucket,
			MaxSize:    cfg.UploadMaxSize,
			SessionTTL: cfg.UploadSessionTTL,
			PartURLTTL: cfg.UploadURLTTL,
		},
	)
	if err != nil {
		log.Error("create multipart service", "error", err)
		return fmt.Errorf("create multipart service: %w", err)
	}
	multipartService.Guard(quotaService, auditService)

	routes, err := deliveryRoutes(db, objectStorage, bucket, log)
	if err != nil {
		return fmt.Errorf("configure delivery: %w", err)
	}
	// Access governance: API tokens authenticate through identity; links,
	// egress and classification are enforced by delivery.
	accessRepo := postgres.NewAccessRepository(db)
	identityService.WithTokens(accessRepo)
	routes.Access = access.NewService(accessRepo, auditService, log)
	routes.Delivery.Guard(identityService.PrincipalFor, auditService)
	routes.Delivery.Govern(routes.Access)
	routes.Lifecycle.Guard(auditService)
	routes.Ingestion, err = newIngestion(db, uploadRepository, uploadService, multipartService, objectStorage, bucket, auditService, log)
	if err != nil {
		return fmt.Errorf("configure ingestion: %w", err)
	}

	catalogConfig, err := config.LoadCatalog()
	if err != nil {
		return fmt.Errorf("load catalog configuration: %w", err)
	}
	catalogMetrics := metrics.New()
	lifecycle := routes.Lifecycle
	routes.Catalog, err = catalog.NewService(catalog.Dependencies{
		Repo: postgres.NewCatalogRepository(db),
		Delete: func(ctx context.Context, p model.Principal, assetID string) (bool, error) {
			result, err := lifecycle.RequestDeletion(ctx, p, assetID)
			return result.Created, err
		},
		Audit: auditService, Metrics: catalogMetrics, Log: log,
	}, catalog.Config(catalogConfig))
	if err != nil {
		return fmt.Errorf("configure catalog: %w", err)
	}
	deliveryBucket := bucket
	if dc, err := config.LoadDelivery(); err == nil && dc.Bucket != "" {
		deliveryBucket = dc.Bucket
	}
	// Storage connections (Phase 11). Uploads, delivery and lifecycle
	// reach a record's bytes through the connection it is bound to; the
	// platform's buckets work as before. Tenants' connections resolve only
	// with BYOS enabled and its credential keys configured.
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage connection configuration: %w", err)
	}
	storageMetrics := metrics.New()
	connections := postgres.NewStorageConnectionRepository(db)
	// Placement checks and residency reports place the platform's own
	// bucket where the operator configured it.
	if region := os.Getenv("S3_REGION"); region != "" {
		if err := connections.SetPlatformRegion(context.Background(), strings.ToLower(region)); err != nil {
			return err
		}
	}
	// The catalog reports the caller's default connection as its last
	// passing test found it; the platform's bucket when that is the default.
	routes.StorageCapabilities = func(r *http.Request) (interfaces.StorageCapabilities, error) {
		p, ok := middleware.PrincipalFrom(r.Context())
		if ok && p.TenantID != "" {
			id, err := connections.DefaultConnection(r.Context(), p.TenantID)
			if err != nil {
				return interfaces.StorageCapabilities{}, err
			}
			if !model.IsPlatformConnection(id) {
				c, err := connections.GetConnection(r.Context(), p.TenantID, id)
				if err != nil {
					return interfaces.StorageCapabilities{}, err
				}
				if c.Capabilities == nil {
					return interfaces.StorageCapabilities{VersioningStatus: "not tested"}, nil
				}
				return interfaces.StorageCapabilities{
					ProviderVersioning: c.Capabilities.ProviderVersioning, VersioningStatus: c.Capabilities.VersioningStatus,
				}, nil
			}
		}
		return objectStorage.Capabilities(r.Context(), deliveryBucket)
	}
	storageRegistry, err := storageregistry.ForProcess(storageCfg, objectStorage, bucket, deliveryBucket, config.PromotionPrefix(), connections, storageMetrics)
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	uploadService.Route(storageRegistry, storageRegistry)
	multipartService.Route(storageRegistry, storageRegistry)
	routes.Delivery.Route(storageRegistry)
	routes.Lifecycle.Route(storageRegistry)
	sealer, err := storageregistry.Sealer(storageCfg)
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	routes.StorageConnections, err = storageconn.NewService(storageconn.Dependencies{
		Connections: connections, Sealer: sealer, Clients: storageRegistry, Audit: auditService, Log: log,
		Health: connections, Alerts: accessRepo, Drift: connections,
	}, storageCfg, storageregistry.Policy(storageCfg))
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	storageRegistry.GuardPlacement(routes.StorageConnections)
	copyRepo := postgres.NewCopyRepository(db)
	routes.Copies = copyengine.NewService(copyengine.Dependencies{
		Store: copyRepo, Router: storageRegistry, Packs: postgres.NewPackRepository(db), Locks: postgres.NewObjectLockRepository(db),
		Posture: connections, Alerts: accessRepo, Audit: auditService,
	}, copyengine.Config{Owner: "api"}, log)
	routes.Delivery.FailOver(routes.Copies)

	// Managed datasets. The API records imports and validations; the
	// dataset worker runs them, so this service reads no object content.
	datasetConfig, err := config.LoadDatasets()
	if err != nil {
		return fmt.Errorf("load dataset configuration: %w", err)
	}
	datasetMetrics := metrics.New()
	datasetRepo := postgres.NewDatasetRepository(db)
	routes.Datasets, err = datasetservice.NewService(datasetservice.Dependencies{
		Repo: datasetRepo, Manifests: objectStorage, ManifestBucket: bucket, Quotas: quotaService,
		Audit: auditService, Metrics: datasetMetrics, Log: log, Placement: routes.StorageConnections, Protection: routes.Copies,
		Quality: postgres.NewQualityRepository(db),
	}, datasetConfig)
	if err != nil {
		return fmt.Errorf("configure datasets: %w", err)
	}
	signing, err := config.LoadLifecycle()
	if err != nil {
		return fmt.Errorf("load lifecycle configuration: %w", err)
	}
	routes.Streaming = streaming.New(streaming.Deps{Datasets: routes.Datasets, Delivery: routes.Delivery,
		Store: postgres.NewStreamingRepository(db), Packs: postgres.NewPackRepository(db), Audit: auditService,
		Signer: streaming.Signer{KeyID: signing.CertificateKeyID, Key: signing.CertificateKey}})
	routes.Tiering = tiering.New(tiering.Deps{Store: postgres.NewTieringRepository(db), Assets: postgres.NewAssetRepository(db),
		Router: storageRegistry, Datasets: routes.Datasets, Audit: auditService, Alerts: accessRepo, Log: log,
		Delete: func(ctx context.Context, p model.Principal, assetID string) error {
			_, err := lifecycle.RequestDeletionGoverned(ctx, p, assetID, "snapshot expiry")
			return err
		}}, tiering.Config{PlatformTiering: os.Getenv("PLATFORM_STORAGE_TIERING") == "true"})
	routes.Delivery.Tiered(routes.Tiering)
	routes.Evidence = evidence.New(evidence.Deps{Store: postgres.NewEvidenceRepository(db), Chains: postgres.NewAuditRepository(db),
		Audit: auditService, Router: storageRegistry, Datasets: routes.Datasets, Delivery: routes.Delivery, Streaming: routes.Streaming,
		Access: routes.Access, Holds: lifecycle, Alerts: accessRepo, TSA: os.Getenv("AUDIT_TSA_URL"), Log: log,
		Signer: evidence.Signer{KeyID: signing.CertificateKeyID, Key: signing.CertificateKey}})
	routes.Datasets.GateProvenance(routes.Evidence)
	routes.Erasure = erasure.New(erasure.Deps{Store: postgres.NewErasureRepository(db), Lifecycle: lifecycle, Catalog: routes.Catalog,
		Audit: auditService, Log: log, Signer: erasure.Signer{KeyID: signing.CertificateKeyID, Key: signing.CertificateKey}})

	dead := postgres.NewDeadLetterRepository(db)
	eventAdmin := events.NewAdmin(postgres.NewOutboxRepository(db), postgres.NewInboxRepository(db), dead, events.Checks{
		Uploads: uploadRepository, Assets: postgres.NewAssetRepository(db),
		Jobs: postgres.NewProcessingJobRepository(db), Deletions: postgres.NewDeletionRepository(db),
		Holds: postgres.NewLegalHoldRepository(db), Ingestion: postgres.NewIngestionRepository(db),
		Datasets: datasetRepo, Packs: postgres.NewPackRepository(db),
	})

	rates := make(map[string]ratelimit.Rule, len(sec.Rates))
	for name, rule := range sec.Rates {
		rates[name] = ratelimit.Rule{Requests: rule.Requests, Window: rule.Window}
	}
	routes.Resolver = middleware.BearerResolver[identity.Identity](identityService)
	routes.Identity, routes.Quotas, routes.Audit = identityService, quotaService, auditService
	routes.DeadLetters = events.NewGuardedAdmin(eventAdmin, dead, auditService)
	routes.Uploads, routes.Multipart = uploadService, multipartService
	routes.Quarantine = processing.NewQuarantine(postgres.NewAssetRepository(db), postgres.NewProcessingJobRepository(db),
		postgres.NewQuarantineRepository(db), postgres.NewProcessingEventRepository(db), auditService).
		WithChecks(postgres.NewContentCheckRepository(db))
	routes.Limiter, routes.Rates, routes.TrustProxyHeaders = limiter, rates, sec.Auth.TrustProxyHeaders
	// The security and catalog counters join the expvar listener started
	// below.
	securityMetrics.Publish("security")
	catalogMetrics.Publish("catalog")
	datasetMetrics.Publish("datasets")
	storageMetrics.Publish("storage")

	routes.Ready = db.Pool.Ping
	routes.Operations = db.OperationsStatus
	// Single sign-on: on with the credential keyring and AUTH_SSO_REDIRECT_URIS.
	routes.Identity.EnableSSO(postgres.NewSSORepository(db), sealer, storageregistry.Policy(storageCfg))
	// Webhook endpoints are managed here; cmd/webhook-worker delivers.
	routes.Webhooks = webhook.NewService(postgres.NewWebhookRepository(db), sealer, storageregistry.Policy(storageCfg), auditService)
	// Adoption jobs are created and read here; the processing worker runs them.
	adoptions := postgres.NewAdoptionRepository(db)
	routes.Adoptions = adoption.NewService(adoption.Dependencies{
		Store: adoptions, Router: storageRegistry, Net: storageregistry.Policy(storageCfg), Audit: auditService,
	}, adoption.Config{}, log)
	routes.Exports = adoption.Exporter{Versions: datasetRepo, Digests: adoptions, Assets: postgres.NewAssetRepository(db),
		Packs: postgres.NewPackRepository(db), Router: storageRegistry, Platform: objectStorage, Bucket: deliveryBucket, Audit: auditService}
	if routes.MetricsAddr != "" {
		stopMetrics := metrics.Serve(routes.MetricsAddr, db.Pool.Ping, log)
		defer stopMetrics()
	}
	srv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           server.NewRouter(routes),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// The single context lets the server stop cleanly when
	// Windows or the deployment enviromnent asks it to exit.
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	serverErrors := make(chan error, 1)

	go func() {
		log.Info("API server starting", "address", srv.Addr)

		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
			return
		}

		serverErrors <- nil
	}()

	select {
	case err := <-serverErrors:
		if err != nil {
			return fmt.Errorf("server HTTP: %w", err)
		}
		return nil

	case <-ctx.Done():
		log.Info("Shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(),
		cfg.ShutdownTimeout,
	)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Force-close connections if graceful shutdown times out.
		if closeErr := srv.Close(); closeErr != nil {
			log.Error("Force close failed", "error", closeErr)
		}

		return fmt.Errorf("graceful shutdown: %w", err)
	}

	log.Info("API server stopped")
	return nil
}

// lifecycleService builds the asset lifecycle the owner routes use
// (delete, restore, lifecycle view). The API only records requests; the
// cleanup worker carries them out.
func lifecycleService(db *postgres.DB, storage *s3storage.ObjectStorage, quarantineBucket, deliveryBucket, prefix string, log *slog.Logger) (*assetservice.Service, error) {
	lc, err := config.LoadLifecycle()
	if err != nil {
		return nil, fmt.Errorf("load lifecycle configuration: %w", err)
	}
	return assetservice.NewService(assetservice.Dependencies{
		Assets: postgres.NewAssetRepository(db), Uploads: postgres.NewUploadRepository(db),
		Jobs: postgres.NewProcessingJobRepository(db), Quarantine: postgres.NewQuarantineRepository(db),
		Policies: postgres.NewRetentionPolicyRepository(db), Holds: postgres.NewLegalHoldRepository(db),
		Deletions: postgres.NewDeletionRepository(db), Events: postgres.NewLifecycleEventRepository(db),
		Cursors: postgres.NewCursorRepository(db), Storage: storage, Packs: postgres.NewPackRepository(db),
		Locks: postgres.NewObjectLockRepository(db), LockSync: postgres.NewLockSyncRepository(db), Governance: postgres.NewGovernanceRepository(db),
		Escrow: postgres.NewCopyRepository(db),
	}, assetservice.Config{
		Owner: "api", QuarantineBucket: quarantineBucket, DeliveryBucket: deliveryBucket, DeliveryPrefix: prefix,
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
		RecheckInterval: lc.RecheckInterval, MaxDeletionsPerRun: lc.MaxDeletionsPerRun,
	}, log)
}

// deliveryRoutes builds the delivery and lifecycle services. Requests are
// authenticated by the router's bearer resolver.
func deliveryRoutes(
	db *postgres.DB,
	storage *s3storage.ObjectStorage,
	quarantineBucket string,
	log *slog.Logger,
) (server.Routes, error) {
	deliveryCfg, err := config.LoadDelivery()
	if err != nil {
		return server.Routes{}, err
	}

	bucket := deliveryCfg.Bucket
	if bucket == "" {
		bucket = quarantineBucket
	}
	registry := metrics.New()
	serviceCfg := delivery.Config{
		Bucket:     bucket,
		Prefix:     deliveryCfg.Prefix,
		DefaultTTL: deliveryCfg.DefaultTTL,
		MaxTTL:     deliveryCfg.MaxTTL,
		URLMode:    deliveryCfg.URLMode,
		Metrics:    registry,
	}
	if deliveryCfg.CDNEnabled {
		keyPEM, err := os.ReadFile(deliveryCfg.CDNPrivateKeyFile)
		if err != nil {
			return server.Routes{}, fmt.Errorf("read CDN private key file: %w", err)
		}
		signer, err := cloudfront.NewSigner(deliveryCfg.CDNKeyPairID, keyPEM)
		if err != nil {
			return server.Routes{}, err
		}
		serviceCfg.CDN = &delivery.CDNConfig{BaseURL: deliveryCfg.CDNBaseURL, Signer: signer}
	}

	// Token links stream through the proxy routes; without them a token
	// request is refused rather than answered with a dead link.
	var tokens interfaces.DownloadTokenRepository
	if deliveryCfg.ProxyEnabled {
		tokens = postgres.NewDownloadTokenRepository(db)
	}

	service, err := delivery.NewService(
		postgres.NewAssetRepository(db),
		storage,
		postgres.NewDeliveryAuditRepository(db),
		tokens,
		serviceCfg,
		log,
	)
	if err != nil {
		return server.Routes{}, err
	}
	service.Packed(postgres.NewPackRepository(db))

	// Signing is local, so this proves the credentials and signer are
	// usable before the first request needs them. The URL is discarded.
	checkCtx, cancelCheck := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = storage.PresignGet(checkCtx, bucket, deliveryCfg.Prefix+"/startup-check", time.Minute, interfaces.DownloadOptions{})
	cancelCheck()
	if err != nil {
		return server.Routes{}, fmt.Errorf("download URL signing is not usable: %w", err)
	}

	if deliveryCfg.MetricsAddr != "" {
		registry.Publish("delivery")
	}

	lifecycle, err := lifecycleService(db, storage, quarantineBucket, bucket, deliveryCfg.Prefix, log)
	if err != nil {
		return server.Routes{}, err
	}

	return server.Routes{
		Delivery:     service,
		ProxyEnabled: deliveryCfg.ProxyEnabled,
		Lifecycle:    lifecycle,
		Stream: httpapi.StreamConfig{
			StallTimeout: deliveryCfg.StallTimeout,
			MaxStreams:   deliveryCfg.MaxStreams,
			Metrics:      registry,
		},
		Log:         log,
		MetricsAddr: deliveryCfg.MetricsAddr,
	}, nil
}
