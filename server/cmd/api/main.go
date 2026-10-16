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

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/cdn/cloudfront"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

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

	uploadRepository := postgres.NewUploadRepository(db)
	uploadService := uploadservice.NewService(
		uploadRepository,
		objectStorage,
		bucket,
		5*1024*1024*1024, // 5 GiB
		30*time.Minute,
	)

	multipartRepository := postgres.NewMultipartUploadRepository(db)

	multipartService, err := uploadservice.NewMultipartService(
		uploadRepository,
		multipartRepository,
		objectStorage,
		uploadservice.MultipartServiceConfig{
			Bucket:     bucket,
			MaxSize:    5 * 1024 * 1024 * 1024, // 5 GiB application limit
			SessionTTL: 30 * time.Minute,
			PartURLTTL: 15 * time.Minute,
		},
	)
	if err != nil {
		log.Error("create multipart service", "error", err)
		return fmt.Errorf("create multipart service: %w", err)
	}

	routes, err := deliveryRoutes(cfg, db, objectStorage, bucket, log)
	if err != nil {
		return fmt.Errorf("configure delivery: %w", err)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           server.NewRouter(uploadService, multipartService, routes),
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

// deliveryRoutes builds the delivery service and decides how delivery
// requests are authenticated.
func deliveryRoutes(
	cfg config.Config,
	db *postgres.DB,
	storage *s3storage.ObjectStorage,
	quarantineBucket string,
	log *slog.Logger,
) (server.DeliveryRoutes, error) {
	deliveryCfg, err := config.LoadDelivery()
	if err != nil {
		return server.DeliveryRoutes{}, err
	}

	bucket := deliveryCfg.Bucket
	if bucket == "" {
		bucket = quarantineBucket
	}
	serviceCfg := delivery.Config{
		Bucket:     bucket,
		Prefix:     deliveryCfg.Prefix,
		DefaultTTL: deliveryCfg.DefaultTTL,
		MaxTTL:     deliveryCfg.MaxTTL,
	}
	if deliveryCfg.CDNEnabled {
		keyPEM, err := os.ReadFile(deliveryCfg.CDNPrivateKeyFile)
		if err != nil {
			return server.DeliveryRoutes{}, fmt.Errorf("read CDN private key file: %w", err)
		}
		signer, err := cloudfront.NewSigner(deliveryCfg.CDNKeyPairID, keyPEM)
		if err != nil {
			return server.DeliveryRoutes{}, err
		}
		serviceCfg.CDN = &delivery.CDNConfig{BaseURL: deliveryCfg.CDNBaseURL, Signer: signer}
	}

	service, err := delivery.NewService(
		postgres.NewAssetRepository(db),
		storage,
		postgres.NewDeliveryAuditRepository(db),
		serviceCfg,
		log,
	)
	if err != nil {
		return server.DeliveryRoutes{}, err
	}

	// No real authentication exists yet. The placeholder identity is used
	// only in development; anywhere else delivery answers 401.
	var identity middleware.Resolver
	if strings.EqualFold(cfg.AppEnv, "development") {
		identity = middleware.DevelopmentIdentity("local-tenant", "local-user")
		log.Warn("delivery uses the development placeholder identity; not production authentication")
	} else {
		log.Warn("no authentication is configured; delivery endpoints will answer 401")
	}

	return server.DeliveryRoutes{
		Service:      service,
		Identity:     identity,
		ProxyEnabled: deliveryCfg.ProxyEnabled,
		Log:          log,
	}, nil
}
