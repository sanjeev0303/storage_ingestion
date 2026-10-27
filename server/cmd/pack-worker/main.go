// Command pack-worker packs small published assets into larger physical
// objects on their own storage connection (Phase 13): it plans packs,
// writes and verifies them, publishes them, and later retires the sources
// and the packs nothing is served from. With EVENT_QUEUE=sqs it also
// consumes the packing queue; the poll keeps running as the safety net.
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
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	packingservice "github.com/sanjeev0303/object-storage-platform/internal/service/packing"
	packingworker "github.com/sanjeev0303/object-storage-platform/internal/worker/packing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pack worker failed:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "run one packing cycle, print what it did as JSON and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	pc, err := config.LoadPacking()
	if err != nil {
		return fmt.Errorf("load packing configuration: %w", err)
	}
	ec, err := config.LoadEvents()
	if err != nil {
		return fmt.Errorf("load event configuration: %w", err)
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
	// Published assets, and so packs, live in the delivery bucket.
	deliveryBucket := bucket
	if delivery, err := config.LoadDelivery(); err == nil && delivery.Bucket != "" {
		deliveryBucket = delivery.Bucket
	}

	registry := metrics.New()
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage connection configuration: %w", err)
	}
	router, err := storageregistry.ForProcess(storageCfg, storage, bucket, deliveryBucket, config.PromotionPrefix(),
		postgres.NewStorageConnectionRepository(db), registry)
	if err != nil {
		return fmt.Errorf("configure storage connections: %w", err)
	}
	owner, err := workerID()
	if err != nil {
		return err
	}
	svc, err := packingservice.NewService(packingservice.Dependencies{
		Packs: postgres.NewPackRepository(db), Assets: postgres.NewAssetRepository(db), Router: router, Metrics: registry, Log: log,
	}, packingservice.Config{Packing: pc, Owner: owner})
	if err != nil {
		return fmt.Errorf("create packing service: %w", err)
	}

	if *once {
		stats, err := svc.RunOnce(context.Background())
		if printErr := printJSON(stats); printErr != nil {
			return printErr
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if pc.MetricsAddr != "" {
		registry.Publish("packing")
		stopMetrics := metrics.Serve(pc.MetricsAddr, db.Pool.Ping, log)
		defer stopMetrics()
	}
	// The consumer is waited for on shutdown, so no handler is cut off while
	// the database it uses is closed.
	var wg sync.WaitGroup
	if ec.Queue == "sqs" {
		consumer, err := packingConsumer(startupCtx, db, ec, pc, svc, owner, log, registry)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			consumer.Run(ctx)
		}()
		log.Info("packing consumer started", "queue", event.QueuePacking)
	}

	log.Info("pack worker started", "owner", owner, "poll_interval", pc.PollInterval, "writers", pc.ActiveWriters,
		"target_pack_bytes", pc.TargetPackBytes, "max_object_bytes", pc.MaxObjectBytes, "min_age", pc.MinAge)
	packingworker.Run(ctx, svc, pc.PollInterval, log)
	wg.Wait()
	log.Info("pack worker stopped")
	return nil
}

// packingConsumer connects to the queue and builds the packing queue's
// consumer. Building a pack takes up to MaxJobDuration, so the handler's
// timeout is at least that.
func packingConsumer(ctx context.Context, db *postgres.DB, ec config.Events, pc config.Packing, svc *packingservice.Service,
	owner string, log *slog.Logger, registry *metrics.Registry) (*events.Consumer, error) {
	queue, err := sqsqueue.New(ctx, sqsqueue.Config{
		Region: ec.SQSRegion, Endpoint: ec.SQSEndpoint,
		AccessKeyID: ec.SQSAccessKeyID, SecretAccessKey: ec.SQSSecretAccessKey,
		Prefix: ec.SQSQueuePrefix, MaxReceiveCount: ec.SQSMaxReceiveCount, CreateQueues: ec.SQSCreateQueues,
	}, event.QueuePacking)
	if err != nil {
		return nil, fmt.Errorf("connect to SQS: %w", err)
	}
	return events.NewConsumer(queue, postgres.NewInboxRepository(db), postgres.NewDeadLetterRepository(db), events.PackingHandlers(svc.BuildPack),
		events.ConsumerConfig{
			Name: event.QueuePacking, Owner: owner, BatchSize: ec.ConsumerBatch, Concurrency: min(ec.ConsumerConcurrency, pc.ActiveWriters),
			Visibility: max(ec.ConsumerVisibility, pc.MaxJobDuration), Wait: ec.ConsumerWait, HandlerTimeout: max(ec.HandlerTimeout, pc.MaxJobDuration),
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
