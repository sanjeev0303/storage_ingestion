// Command lifecycle-admin is the operator's tool for retention policies,
// legal holds, deletions and lifecycle records. Every change is audited
// with the -actor given.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	storageregistry "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	s3storage "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/s3"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
)

const usage = `usage: lifecycle-admin <command> [flags]

commands:
  policy-set      store a new retention policy version
  policy-history  list a scope's policy versions
  hold-place      place a legal hold
  hold-release    release a legal hold
  holds           list a tenant's legal holds
  delete          soft-delete an asset (operator request)
  restore         undo a deletion that has not started
  inspect         show an asset's policy, holds, deletions and events
  requeue         give a FAILED deletion a fresh attempt budget
  scans           list recent inventory scans
  candidates      list orphan candidates
  events          list a tenant's lifecycle events
  compaction-runs list recent compaction runs

Run "lifecycle-admin <command> -h" for a command's flags.`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "help" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "lifecycle-admin:", err)
		os.Exit(1)
	}
}

func run(command string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	db, err := postgres.Open(ctx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	flags := flag.NewFlagSet(command, flag.ExitOnError)
	tenant := flags.String("tenant", "", "tenant ID")
	actor := flags.String("actor", defaultActor(), "who is acting (recorded in the audit)")
	limit := flags.Int("limit", 50, "maximum rows to list")

	switch command {
	case "policy-set":
		scope := flags.String("scope", model.ScopeTenant, "TENANT, DATASET or ASSET")
		key := flags.String("key", "", "dataset name or asset ID (not for TENANT)")
		minRetention := flags.String("min-retention", "", "keep at least this long after creation, e.g. 30d (empty: inherit)")
		maxAge := flags.String("max-age", "", "expire this long after creation, 0 for never (empty: inherit)")
		grace := flags.String("grace", "", "how long a deletion stays restorable (empty: inherit)")
		quarantine := flags.String("quarantine-retention", "", "keep rejected uploads this long (empty: inherit)")
		effective := flags.String("effective", "", "RFC 3339 time the version takes effect (default now)")
		reason := flags.String("reason", "", "why the policy changes (required)")
		_ = flags.Parse(args)

		policy := model.RetentionPolicy{TenantID: *tenant, Scope: strings.ToUpper(*scope), ScopeKey: *key, CreatedBy: *actor, Reason: *reason}
		for _, setting := range []struct {
			text   string
			target **time.Duration
		}{
			{*minRetention, &policy.MinRetention}, {*maxAge, &policy.MaxAge},
			{*grace, &policy.DeletionGrace}, {*quarantine, &policy.QuarantineRetention},
		} {
			if *setting.target, err = optionalDuration(setting.text); err != nil {
				return err
			}
		}
		if *effective != "" {
			if policy.EffectiveFrom, err = time.Parse(time.RFC3339, *effective); err != nil {
				return fmt.Errorf("-effective must be an RFC 3339 time: %w", err)
			}
		}
		service, err := lifecycleService(ctx, db)
		if err != nil {
			return err
		}
		stored, err := service.SetPolicy(ctx, policy)
		if err != nil {
			return err
		}
		return printJSON(stored)

	case "policy-history":
		scope := flags.String("scope", model.ScopeTenant, "TENANT, DATASET or ASSET")
		key := flags.String("key", "", "dataset name or asset ID")
		_ = flags.Parse(args)
		versions, err := postgres.NewRetentionPolicyRepository(db).History(ctx, *tenant, strings.ToUpper(*scope), *key)
		if err != nil {
			return err
		}
		return printJSON(versions)

	case "hold-place":
		scope := flags.String("scope", model.ScopeAsset, "TENANT, DATASET or ASSET")
		key := flags.String("key", "", "dataset name or asset ID (not for TENANT)")
		reason := flags.String("reason", "", "why the hold is placed (required)")
		_ = flags.Parse(args)
		service, err := lifecycleService(ctx, db)
		if err != nil {
			return err
		}
		hold, err := service.PlaceHold(ctx, model.LegalHold{
			TenantID: *tenant, Scope: strings.ToUpper(*scope), ScopeKey: *key, Reason: *reason, PlacedBy: *actor,
		})
		if err != nil {
			return err
		}
		return printJSON(hold)

	case "hold-release":
		id := flags.String("id", "", "hold ID")
		_ = flags.Parse(args)
		service, err := lifecycleService(ctx, db)
		if err != nil {
			return err
		}
		if err := service.ReleaseHold(ctx, *tenant, *id, *actor); err != nil {
			return err
		}
		return printJSON(map[string]string{"released": *id})

	case "holds":
		all := flags.Bool("all", false, "include released holds")
		_ = flags.Parse(args)
		holds, err := postgres.NewLegalHoldRepository(db).List(ctx, *tenant, *all)
		if err != nil {
			return err
		}
		return printJSON(holds)

	case "delete":
		asset := flags.String("asset", "", "asset ID")
		note := flags.String("note", "", "why the asset is deleted")
		_ = flags.Parse(args)
		service, err := lifecycleService(ctx, db)
		if err != nil {
			return err
		}
		result, err := service.OperatorDelete(ctx, *tenant, *asset, *actor, *note)
		if err != nil {
			return err
		}
		return printJSON(result)

	case "restore":
		asset := flags.String("asset", "", "asset ID")
		_ = flags.Parse(args)
		service, err := lifecycleService(ctx, db)
		if err != nil {
			return err
		}
		op, err := service.OperatorRestore(ctx, *tenant, *asset, *actor)
		if err != nil {
			return err
		}
		return printJSON(op)

	case "inspect":
		asset := flags.String("asset", "", "asset ID")
		_ = flags.Parse(args)
		service, err := lifecycleService(ctx, db)
		if err != nil {
			return err
		}
		inspection, err := service.Inspect(ctx, *tenant, *asset)
		if err != nil {
			return err
		}
		return printJSON(inspection)

	case "requeue":
		operation := flags.String("operation", "", "deletion operation ID")
		_ = flags.Parse(args)
		service, err := lifecycleService(ctx, db)
		if err != nil {
			return err
		}
		op, err := service.Requeue(ctx, *tenant, *operation, *actor)
		if err != nil {
			return err
		}
		return printJSON(op)

	case "scans":
		_ = flags.Parse(args)
		scans, err := postgres.NewInventoryRepository(db).RecentScans(ctx, *limit)
		if err != nil {
			return err
		}
		return printJSON(scans)

	case "candidates":
		status := flags.String("status", "", "MARKED, FAILED, DELETED or CLEARED (default: all)")
		_ = flags.Parse(args)
		candidates, err := postgres.NewInventoryRepository(db).ListCandidates(ctx, strings.ToUpper(*status), *limit)
		if err != nil {
			return err
		}
		return printJSON(candidates)

	case "events":
		asset := flags.String("asset", "", "only this asset's events")
		_ = flags.Parse(args)
		events, err := postgres.NewLifecycleEventRepository(db).List(ctx, *tenant, *asset, *limit)
		if err != nil {
			return err
		}
		return printJSON(events)

	case "compaction-runs":
		_ = flags.Parse(args)
		runs, err := postgres.NewCompactor(db).RecentRuns(ctx, *limit)
		if err != nil {
			return err
		}
		return printJSON(runs)
	}
	return fmt.Errorf("unknown command %q\n\n%s", command, usage)
}

// lifecycleService builds the service with the same platform floors the
// worker uses, so a policy the worker would refuse is refused here too.
func lifecycleService(ctx context.Context, db *postgres.DB) (*assetservice.Service, error) {
	lc, err := config.LoadLifecycle()
	if err != nil {
		return nil, fmt.Errorf("load lifecycle configuration: %w", err)
	}
	storageConfig, err := s3storage.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("load object storage configuration: %w", err)
	}
	client, err := s3storage.NewClient(ctx, storageConfig)
	if err != nil {
		return nil, fmt.Errorf("initialize S3 client: %w", err)
	}
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return nil, errors.New("S3_BUCKET is required")
	}
	deliveryBucket := lc.PromotionBucket
	if deliveryBucket == "" {
		deliveryBucket = bucket
	}
	platformStorage := s3storage.NewObjectStorage(client)
	// Records bound to tenants' storage connections are reached through
	// those connections (Phase 11); without BYOS they do not resolve.
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return nil, fmt.Errorf("load storage connection configuration: %w", err)
	}
	router, err := storageregistry.ForProcess(storageCfg, platformStorage, bucket, deliveryBucket, lc.PromotionPrefix,
		postgres.NewStorageConnectionRepository(db), nil)
	if err != nil {
		return nil, fmt.Errorf("configure storage connections: %w", err)
	}

	return assetservice.NewService(assetservice.Dependencies{
		Assets: postgres.NewAssetRepository(db), Uploads: postgres.NewUploadRepository(db),
		Jobs: postgres.NewProcessingJobRepository(db), Quarantine: postgres.NewQuarantineRepository(db),
		Policies: postgres.NewRetentionPolicyRepository(db), Holds: postgres.NewLegalHoldRepository(db),
		Deletions: postgres.NewDeletionRepository(db), Events: postgres.NewLifecycleEventRepository(db),
		Cursors: postgres.NewCursorRepository(db), Storage: platformStorage, Router: router,
		Packs: postgres.NewPackRepository(db),
	}, assetservice.Config{
		Owner: "lifecycle-admin", QuarantineBucket: bucket, DeliveryBucket: deliveryBucket, DeliveryPrefix: lc.PromotionPrefix,
		Platform: assetservice.PlatformRetention{
			DefaultMinRetention:        lc.DefaultMinRetention,
			DefaultMaxAge:              lc.DefaultMaxAge,
			DefaultDeletionGrace:       lc.DefaultDeletionGrace,
			DefaultQuarantineRetention: lc.QuarantineRetention,
			MinRetentionFloor:          lc.MinRetentionFloor,
			DeletionGraceFloor:         lc.DeletionGraceFloor,
			QuarantineRetentionFloor:   lc.QuarantineRetention,
		},
		BatchSize: lc.BatchSize, Concurrency: lc.Concurrency, Lease: lc.LeaseDuration,
		Retry:           retry.Policy{BaseDelay: lc.BackoffBase, MaxDelay: lc.BackoffMax, MaxAttempts: lc.MaxAttempts},
		RecheckInterval: lc.RecheckInterval, MaxDeletionsPerRun: lc.MaxDeletionsPerRun,
	}, logger.NewTo(os.Stderr, "development"))
}

// optionalDuration reads a policy setting: empty inherits, otherwise a Go
// duration or a whole number of days such as "30d".
func optionalDuration(text string) (*time.Duration, error) {
	if text == "" {
		return nil, nil
	}
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid duration %q", text)
		}
		d := time.Duration(n) * 24 * time.Hour
		return &d, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		return nil, fmt.Errorf("invalid duration %q: %w", text, err)
	}
	return &d, nil
}

func defaultActor() string {
	for _, key := range []string{"USERNAME", "USER"} {
		if name := os.Getenv(key); name != "" {
			return name
		}
	}
	return "operator"
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
