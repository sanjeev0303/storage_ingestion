package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Events contains the event pipeline settings: the queue, the outbox
// publisher, the consumers and event reconciliation.
type Events struct {
	// Queue is EVENT_QUEUE: "sqs" publishes and consumes events; "" (the
	// default) leaves the work to the pollers alone. Events are written
	// to the outbox either way.
	Queue string

	SQSRegion          string
	SQSEndpoint        string // empty: AWS
	SQSAccessKeyID     string
	SQSSecretAccessKey string
	SQSQueuePrefix     string
	// SQSCreateQueues creates missing queues at startup (local only).
	SQSCreateQueues bool
	// SQSMaxReceiveCount moves a message to its dead-letter queue after
	// this many receives. It must exceed EVENT_MAX_ATTEMPTS, so the
	// consumer's own budget runs out first.
	SQSMaxReceiveCount int

	PublishInterval    time.Duration
	PublishBatch       int
	PublishConcurrency int
	PublishLease       time.Duration
	PublishTimeout     time.Duration
	PublishMaxAttempts int
	PublishBackoffBase time.Duration
	PublishBackoffMax  time.Duration

	ConsumerBatch       int
	ConsumerConcurrency int
	ConsumerVisibility  time.Duration
	ConsumerWait        time.Duration
	HandlerTimeout      time.Duration
	ConsumerMaxAttempts int
	ConsumerBackoffBase time.Duration
	ConsumerBackoffMax  time.Duration

	ReconcileInterval time.Duration
	StuckAfter        time.Duration
	ReconcileWindow   time.Duration
	MaxRepublish      int
	ReconcileBatch    int

	MetricsAddr string
}

// LoadEvents reads the event pipeline settings.
func LoadEvents() (Events, error) {
	cfg := Events{
		Queue:              strings.ToLower(getEnv("EVENT_QUEUE", "")),
		SQSRegion:          getEnv("SQS_REGION", getEnv("S3_REGION", "us-east-1")),
		SQSEndpoint:        getEnv("SQS_ENDPOINT", ""),
		SQSAccessKeyID:     getEnv("SQS_ACCESS_KEY_ID", ""),
		SQSSecretAccessKey: getEnv("SQS_SECRET_ACCESS_KEY", ""),
		SQSQueuePrefix:     getEnv("SQS_QUEUE_PREFIX", "storage-"),
		SQSCreateQueues:    strings.EqualFold(getEnv("SQS_CREATE_QUEUES", "false"), "true"),
		MetricsAddr:        getEnv("METRICS_ADDR", ""),
	}
	if cfg.Queue != "" && cfg.Queue != "sqs" {
		return Events{}, fmt.Errorf("EVENT_QUEUE must be sqs or empty")
	}
	if cfg.SQSEndpoint != "" {
		parsed, err := url.Parse(cfg.SQSEndpoint)
		local := err == nil && parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1")
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !local) {
			return Events{}, fmt.Errorf("SQS_ENDPOINT must be an absolute HTTPS URL, or HTTP on localhost")
		}
	}
	if (cfg.SQSAccessKeyID == "") != (cfg.SQSSecretAccessKey == "") {
		return Events{}, fmt.Errorf("SQS_ACCESS_KEY_ID and SQS_SECRET_ACCESS_KEY must be set together")
	}

	var err error
	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"OUTBOX_POLL_INTERVAL", "1s", &cfg.PublishInterval},
		{"OUTBOX_LEASE", "1m", &cfg.PublishLease},
		{"OUTBOX_PUBLISH_TIMEOUT", "10s", &cfg.PublishTimeout},
		{"OUTBOX_BACKOFF_BASE", "2s", &cfg.PublishBackoffBase},
		{"OUTBOX_BACKOFF_MAX", "5m", &cfg.PublishBackoffMax},
		{"EVENT_VISIBILITY", "1m", &cfg.ConsumerVisibility},
		{"EVENT_HANDLER_TIMEOUT", "15m", &cfg.HandlerTimeout},
		{"EVENT_BACKOFF_BASE", "5s", &cfg.ConsumerBackoffBase},
		{"EVENT_BACKOFF_MAX", "15m", &cfg.ConsumerBackoffMax},
		{"EVENT_RECONCILE_INTERVAL", "1m", &cfg.ReconcileInterval},
		{"EVENT_STUCK_AFTER", "15m", &cfg.StuckAfter},
		{"EVENT_RECONCILE_WINDOW", "72h", &cfg.ReconcileWindow},
	}
	for _, d := range durations {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Events{}, fmt.Errorf("%s must be a positive duration such as 30s", d.key)
		}
	}
	if cfg.ConsumerWait, err = time.ParseDuration(getEnv("EVENT_WAIT", "10s")); err != nil || cfg.ConsumerWait < 0 || cfg.ConsumerWait > 20*time.Second {
		return Events{}, fmt.Errorf("EVENT_WAIT must be a duration from 0s to 20s")
	}

	ints := []struct {
		key, fallback string
		target        *int
	}{
		{"SQS_MAX_RECEIVE_COUNT", "20", &cfg.SQSMaxReceiveCount},
		{"OUTBOX_BATCH_SIZE", "50", &cfg.PublishBatch},
		{"OUTBOX_CONCURRENCY", "4", &cfg.PublishConcurrency},
		{"OUTBOX_MAX_ATTEMPTS", "10", &cfg.PublishMaxAttempts},
		{"EVENT_BATCH_SIZE", "10", &cfg.ConsumerBatch},
		{"EVENT_CONCURRENCY", "4", &cfg.ConsumerConcurrency},
		{"EVENT_MAX_ATTEMPTS", "8", &cfg.ConsumerMaxAttempts},
		{"EVENT_MAX_REPUBLISH", "3", &cfg.MaxRepublish},
		{"EVENT_RECONCILE_BATCH", "100", &cfg.ReconcileBatch},
	}
	for _, n := range ints {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Events{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}

	switch {
	case cfg.SQSMaxReceiveCount <= cfg.ConsumerMaxAttempts:
		return Events{}, fmt.Errorf("SQS_MAX_RECEIVE_COUNT must be greater than EVENT_MAX_ATTEMPTS")
	case cfg.ConsumerBatch > 10:
		return Events{}, fmt.Errorf("EVENT_BATCH_SIZE must be at most 10")
	case cfg.ConsumerVisibility < 10*time.Second || cfg.ConsumerVisibility > 12*time.Hour:
		return Events{}, fmt.Errorf("EVENT_VISIBILITY must be between 10s and 12h")
	// The publisher keeps 10s of its lease to record each outcome.
	case cfg.PublishLease <= cfg.PublishTimeout+10*time.Second:
		return Events{}, fmt.Errorf("OUTBOX_LEASE must be longer than OUTBOX_PUBLISH_TIMEOUT plus 10s")
	case cfg.PublishBackoffMax < cfg.PublishBackoffBase || cfg.ConsumerBackoffMax < cfg.ConsumerBackoffBase:
		return Events{}, fmt.Errorf("a backoff maximum must not be less than its base")
	case cfg.ConsumerBackoffMax > 12*time.Hour:
		return Events{}, fmt.Errorf("EVENT_BACKOFF_MAX must be at most 12h, the longest SQS can hide a message")
	case cfg.ReconcileWindow <= cfg.StuckAfter:
		return Events{}, fmt.Errorf("EVENT_RECONCILE_WINDOW must be longer than EVENT_STUCK_AFTER")
	}
	return cfg, nil
}
