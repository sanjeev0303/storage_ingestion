package config

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// No adapter can store or promote more than 5 TiB in one object, so a
// larger upload limit is refused at startup, not after an upload.
func TestUploadLimitAboveFiveTiBIsRefused(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://config-test@127.0.0.1:1/none")
	t.Setenv("UPLOAD_MAX_SIZE_BYTES", strconv.FormatInt(5<<40+1, 10))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "5 TiB") {
		t.Fatalf("above 5 TiB: %v", err)
	}
	t.Setenv("UPLOAD_MAX_SIZE_BYTES", strconv.FormatInt(5<<40, 10))
	if _, err := Load(); err != nil && strings.Contains(err.Error(), "UPLOAD_MAX_SIZE_BYTES") {
		t.Fatalf("5 TiB itself is allowed: %v", err)
	}
}

// Every process reads the platform's publication prefix the same way.
func TestEveryLoaderAgreesOnThePromotionPrefix(t *testing.T) {
	for env, want := range map[string]string{"": "assets", "published/": "published", "/": "assets"} {
		t.Setenv("PROMOTION_PREFIX", env)
		delivery, err1 := LoadDelivery()
		lifecycle, err2 := LoadLifecycle()
		processing, err3 := LoadProcessing()
		if err := errors.Join(err1, err2, err3); err != nil {
			t.Fatal(err)
		}
		if PromotionPrefix() != want || delivery.Prefix != want || lifecycle.PromotionPrefix != want || processing.PromotionPrefix != want {
			t.Fatalf("PROMOTION_PREFIX=%q: %q %q %q %q, want %q", env, PromotionPrefix(), delivery.Prefix,
				lifecycle.PromotionPrefix, processing.PromotionPrefix, want)
		}
	}
}

func TestDeliveryDefaultsArePrivateAndBounded(t *testing.T) {
	t.Setenv("DELIVERY_CDN_ENABLED", "")
	cfg, err := LoadDelivery()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CDNEnabled || cfg.DefaultTTL.Minutes() != 5 || cfg.MaxTTL.Minutes() != 15 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
}

func TestCDNCannotBeEnabledWithoutSigning(t *testing.T) {
	t.Setenv("DELIVERY_CDN_ENABLED", "true")
	t.Setenv("DELIVERY_CDN_BASE_URL", "https://cdn.example.test")
	if _, err := LoadDelivery(); err == nil {
		t.Fatal("a CDN without a signing key must be refused")
	}
}

func TestDeliveryTTLMustNotBeShorterThanItsDefault(t *testing.T) {
	t.Setenv("DELIVERY_URL_TTL", "10m")
	t.Setenv("DELIVERY_URL_MAX_TTL", "5m")
	if _, err := LoadDelivery(); err == nil {
		t.Fatal("max below default must be refused")
	}
}

func TestDeliveryStreamingSettings(t *testing.T) {
	cfg, err := LoadDelivery()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URLMode != "storage" || cfg.StallTimeout <= 0 || cfg.MaxStreams <= 0 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}

	for key, value := range map[string]string{
		"DELIVERY_URL_MODE":      "public",
		"DELIVERY_STALL_TIMEOUT": "-1s",
		"DELIVERY_MAX_STREAMS":   "0",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := LoadDelivery(); err == nil {
				t.Fatalf("%s=%s must be refused", key, value)
			}
		})
	}

	t.Setenv("DELIVERY_URL_MODE", "token")
	t.Setenv("DELIVERY_PROXY_ENABLED", "false")
	if _, err := LoadDelivery(); err == nil {
		t.Fatal("token links without the proxy routes must be refused")
	}
}

func TestEventDefaultsKeepTheQueueBackstopAboveTheConsumerBudget(t *testing.T) {
	cfg, err := LoadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Queue != "" || cfg.SQSMaxReceiveCount <= cfg.ConsumerMaxAttempts || cfg.ConsumerBatch > 10 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}

	t.Setenv("SQS_MAX_RECEIVE_COUNT", "8")
	t.Setenv("EVENT_MAX_ATTEMPTS", "8")
	if _, err := LoadEvents(); err == nil {
		t.Fatal("the queue's redrive must not fire before the consumer's own budget runs out")
	}
}

func TestEventQueueEndpointMustBeLocalOrHTTPS(t *testing.T) {
	t.Setenv("SQS_ENDPOINT", "http://sqs.example.com")
	if _, err := LoadEvents(); err == nil {
		t.Fatal("plain HTTP is only for a local queue")
	}
	t.Setenv("SQS_ENDPOINT", "http://127.0.0.1:9324")
	t.Setenv("EVENT_QUEUE", "sqs")
	if cfg, err := LoadEvents(); err != nil || cfg.Queue != "sqs" {
		t.Fatalf("a local ElasticMQ endpoint is fine: %v", err)
	}
}

func TestPackingDefaultsAreValidAndLimitsAreChecked(t *testing.T) {
	cfg, err := LoadPacking()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxObjectBytes != 1<<20 || cfg.TargetPackBytes != 64<<20 || cfg.MaxEntries != 10000 || cfg.TenantActivePacks != 1 {
		t.Fatalf("defaults %+v", cfg)
	}
	for key, value := range map[string]string{
		"PACKING_MAX_OBJECT_BYTES":    "134217728",
		"PACKING_SOURCE_CONCURRENCY":  "0",
		"PACKING_TARGET_PACK_BYTES":   "1024",
		"PACKING_MAX_PACK_BYTES":      "10737418240",
		"PACKING_MAX_ENTRIES":         "1",
		"PACKING_REWRITE_DEAD_RATIO":  "1.5",
		"PACKING_LEASE":               "1s",
		"PACKING_TENANT_ACTIVE_PACKS": "0",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := LoadPacking(); err == nil || !strings.Contains(err.Error(), "PACKING_") {
				t.Fatalf("%s=%s must be refused: %v", key, value, err)
			}
		})
	}
	t.Setenv("PACKING_SOURCE_CONCURRENCY", "64")
	t.Setenv("PACKING_MAX_OBJECT_BYTES", "33554432")
	t.Setenv("PACKING_TARGET_PACK_BYTES", "67108864")
	if _, err := LoadPacking(); err == nil || !strings.Contains(err.Error(), "1 GiB") {
		t.Fatalf("64 sources of 32 MiB in memory must be refused: %v", err)
	}
}
