package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	compactionservice "github.com/sanjeev0303/object-storage-platform/internal/service/compaction"
	compactionworker "github.com/sanjeev0303/object-storage-platform/internal/worker/compaction"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "compaction worker failed:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "run every rule once, print the results as JSON and exit")
	dryRun := flag.Bool("dry-run", false, "count what would be removed and change nothing")
	rules := flag.String("rules", "", "comma-separated rules to run (default: all)")
	list := flag.Bool("list", false, "print the rules and exit")
	flag.Parse()

	if *list {
		return printJSON(compactionservice.Rules)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	cc, err := config.LoadCompaction()
	if err != nil {
		return fmt.Errorf("load compaction configuration: %w", err)
	}

	log := logger.New(cfg.AppEnv)
	if *once {
		log = logger.NewTo(os.Stderr, cfg.AppEnv)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()
	db, err := postgres.Open(startupCtx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	var only []string
	for _, name := range strings.Split(*rules, ",") {
		if name = strings.TrimSpace(name); name != "" {
			only = append(only, name)
		}
	}

	// Compaction deletes only with COMPACTION_ENABLED=true; otherwise
	// every run is a dry run.
	registry := metrics.New()
	service, err := compactionservice.NewService(postgres.NewCompactor(db), compactionservice.Config{
		BatchSize: cc.BatchSize, MaxBatches: cc.MaxBatches, BatchPause: cc.BatchPause,
		AuditRetention: cc.AuditRetention, OperationalRetention: cc.OperationalRetention, IdempotencyTTL: cc.IdempotencyTTL,
		Only: only, DryRun: *dryRun || !cc.Enabled,
	}, log, registry)
	if err != nil {
		return fmt.Errorf("create compaction service: %w", err)
	}
	log.Info("compaction worker starting", "dry_run", *dryRun || !cc.Enabled, "audit_retention", cc.AuditRetention.String(),
		"operational_retention", cc.OperationalRetention.String(), "idempotency_ttl", cc.IdempotencyTTL.String(),
		"batch_size", cc.BatchSize, "max_batches", cc.MaxBatches)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		results, err := service.RunOnce(ctx)
		if printErr := printJSON(results); printErr != nil {
			return printErr
		}
		return err
	}

	if cc.MetricsAddr != "" {
		registry.Publish("compaction")
		stopMetrics := metrics.Serve(cc.MetricsAddr, db.Pool.Ping, log)
		defer stopMetrics()
	}
	compactionworker.Run(ctx, service, cc.PollInterval, log)
	log.Info("compaction worker stopped")
	return nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
