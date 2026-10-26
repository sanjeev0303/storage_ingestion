// Command catalog-admin is the operator's tool for the object catalog:
// reconciliation against assets and storage, catalog sizes, and the
// storage provider's capabilities. It works with direct database access,
// so whoever runs it is a platform operator; repairs are audited.
//
// reconcile is a dry run unless -repair is given.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	storageregistry "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
)

const usage = `usage: catalog-admin <command> [flags]

commands:
  reconcile     compare the catalog with assets and storage (-tenant, -repair, -storage, -batch, -max-storage-checks)
  stats         catalog sizes: objects, versions, table and index bytes
  capabilities  the delivery bucket's provider capabilities

Run "catalog-admin <command> -h" for a command's flags.`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "catalog-admin:", err)
		os.Exit(1)
	}
}

func run(command string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	tenant := flags.String("tenant", "", "tenant to reconcile (default: every tenant)")
	repair := flags.Bool("repair", false, "repair what can be repaired (default: report only)")
	checkStorage := flags.Bool("storage", false, "also compare published versions with storage and list the published prefix")
	batch := flags.Int("batch", 500, "rows per page")
	maxChecks := flags.Int("max-storage-checks", 10000, "most stored objects checked and listed per tenant")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	sec, err := config.LoadSecurity()
	if err != nil {
		return fmt.Errorf("load security configuration: %w", err)
	}
	catalogConfig, err := config.LoadCatalog()
	if err != nil {
		return fmt.Errorf("load catalog configuration: %w", err)
	}
	lc, err := config.LoadLifecycle()
	if err != nil {
		return fmt.Errorf("load lifecycle configuration: %w", err)
	}
	db, err := postgres.Open(ctx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()
	storageConfig, err := s3storage.LoadConfig()
	if err != nil {
		return fmt.Errorf("load object storage configuration: %w", err)
	}
	client, err := s3storage.NewClient(ctx, storageConfig)
	if err != nil {
		return fmt.Errorf("initialize S3 client: %w", err)
	}
	storage := s3storage.NewObjectStorage(client)
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return errors.New("S3_BUCKET is required")
	}
	deliveryBucket := lc.PromotionBucket
	if deliveryBucket == "" {
		deliveryBucket = bucket
	}

	switch command {
	case "capabilities":
		c, err := storage.Capabilities(ctx, deliveryBucket)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"bucket": deliveryBucket, "logical_versioning": true,
			"provider_versioning": c.ProviderVersioning, "provider_versioning_status": c.VersioningStatus})

	case "stats":
		stats, err := postgres.NewCatalogRepository(db).Stats(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, stats)

	case "reconcile":
		log := logger.NewTo(os.Stderr, cfg.AppEnv)
		// Records bound to tenants' storage connections are reached through
		// those connections (Phase 11); without BYOS they do not resolve.
		storageCfg, err := config.LoadStorage()
		if err != nil {
			return fmt.Errorf("load storage connection configuration: %w", err)
		}
		router, err := storageregistry.ForProcess(storageCfg, storage, bucket, deliveryBucket, lc.PromotionPrefix,
			postgres.NewStorageConnectionRepository(db), nil)
		if err != nil {
			return fmt.Errorf("configure storage connections: %w", err)
		}

		auditService := audit.NewService(postgres.NewAuditRepository(db), log, nil, sec.Audit.Retention)
		packs := postgres.NewPackRepository(db)
		// Replaced versions of objects without versioning are deleted
		// through the lifecycle, as the worker would.
		lifecycle, err := assetservice.NewService(assetservice.Dependencies{
			Assets: postgres.NewAssetRepository(db), Uploads: postgres.NewUploadRepository(db),
			Jobs: postgres.NewProcessingJobRepository(db), Quarantine: postgres.NewQuarantineRepository(db),
			Policies: postgres.NewRetentionPolicyRepository(db), Holds: postgres.NewLegalHoldRepository(db),
			Deletions: postgres.NewDeletionRepository(db), Events: postgres.NewLifecycleEventRepository(db),
			Cursors: postgres.NewCursorRepository(db), Storage: storage, Router: router, Packs: packs,
		}, assetservice.Config{
			Owner: "catalog-admin", QuarantineBucket: bucket, DeliveryBucket: deliveryBucket, DeliveryPrefix: lc.PromotionPrefix,
			Platform: assetservice.PlatformRetention{
				DefaultMinRetention: lc.DefaultMinRetention, DefaultMaxAge: lc.DefaultMaxAge,
				DefaultDeletionGrace: lc.DefaultDeletionGrace, DefaultQuarantineRetention: lc.QuarantineRetention,
				MinRetentionFloor: lc.MinRetentionFloor, DeletionGraceFloor: lc.DeletionGraceFloor,
				QuarantineRetentionFloor: lc.QuarantineRetention,
			},
			BatchSize: lc.BatchSize, Concurrency: lc.Concurrency, Lease: lc.LeaseDuration,
			Retry:           retry.Policy{BaseDelay: lc.BackoffBase, MaxDelay: lc.BackoffMax, MaxAttempts: lc.MaxAttempts},
			RecheckInterval: lc.RecheckInterval, MaxDeletionsPerRun: lc.MaxDeletionsPerRun,
		}, log)
		if err != nil {
			return err
		}
		lifecycle.Guard(auditService)
		repo := postgres.NewCatalogRepository(db)
		catalogService, err := catalog.NewService(catalog.Dependencies{
			Repo: repo,
			Delete: func(ctx context.Context, p model.Principal, assetID string) (bool, error) {
				result, err := lifecycle.RequestDeletion(ctx, p, assetID)
				return result.Created, err
			},
			Audit: auditService, Log: log,
		}, catalog.Config(catalogConfig))
		if err != nil {
			return err
		}
		reconciler := catalog.NewReconciler(repo, catalogService, storage, deliveryBucket, lc.PromotionPrefix, auditService, nil, log).Route(router).Packed(packs)
		report, err := reconciler.Reconcile(ctx, catalog.ReconcileOptions{
			TenantID: *tenant, DryRun: !*repair, BatchSize: *batch, CheckStorage: *checkStorage, MaxStorageChecks: *maxChecks,
		})
		if err != nil {
			return err
		}
		return printJSON(out, report)
	}
	return fmt.Errorf("unknown command %q\n%s", command, usage)
}

func printJSON(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
