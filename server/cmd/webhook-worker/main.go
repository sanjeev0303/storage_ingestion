// Command webhook-worker sends organizations' webhook deliveries
// (docs/webhooks.md).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	storageregistry "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/logger"
	"github.com/sanjeev0303/object-storage-platform/internal/service/webhook"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "webhook worker failed:", err)
		os.Exit(1)
	}
}

func run() error {
	once := flag.Bool("once", false, "send one batch of due deliveries and exit")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return fmt.Errorf("load storage configuration: %w", err)
	}
	sealer, err := storageregistry.Sealer(storageCfg)
	if err != nil {
		return err
	}
	if sealer == nil {
		return webhook.ErrNotConfigured
	}
	concurrency, err := strconv.Atoi(getEnv("WEBHOOK_CONCURRENCY", "16"))
	if err != nil || concurrency < 1 || concurrency > 256 {
		return fmt.Errorf("WEBHOOK_CONCURRENCY must be between 1 and 256")
	}
	interval, err := time.ParseDuration(getEnv("WEBHOOK_POLL_INTERVAL", "5s"))
	if err != nil || interval < time.Second {
		return fmt.Errorf("WEBHOOK_POLL_INTERVAL must be a duration of at least 1s")
	}
	log := logger.New(cfg.AppEnv)

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStartup()
	db, err := postgres.Open(startupCtx, cfg.DatabaseUrl)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close()

	owner, err := workerID()
	if err != nil {
		return err
	}
	// Customers' URLs are requests to make this process connect somewhere:
	// the network guard checks every dial and follows no redirect.
	client := storageregistry.Policy(storageCfg).Client()
	sender := webhook.NewSender(postgres.NewWebhookRepository(db), sealer, client, owner, concurrency, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		n, err := sender.RunOnce(ctx)
		fmt.Println(n, "deliveries tried")
		return err
	}

	log.Info("webhook worker starting", "worker_id", owner, "concurrency", concurrency, "interval", interval.String())
	// Woken by queue_webhook (migration 000021) when a delivery commits.
	wake := make(chan struct{}, 1)
	go db.Listen(ctx, "webhook_deliveries", wake, log)
	sender.Run(ctx, interval, wake)
	log.Info("webhook worker stopped")
	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

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
