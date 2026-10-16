package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config contains the settings needed to start the API server.
type Config struct {
	AppEnv          string
	HTTPPort        string
	DatabaseUrl     string
	ShutdownTimeout time.Duration
}

// Load reads configuration from environment variables.
// Defaults are useful for local development, while production
// deployments can supply their own values
func Load() (Config, error) {
	cfg := Config{
		AppEnv:          getEnv("APP_ENV", "development"),
		HTTPPort:        getEnv("HTTP_PORT", "8080"),
		DatabaseUrl:     getEnv("DATABASE_URL", ""),
		ShutdownTimeout: 10 * time.Second,
	}

	port, err := strconv.Atoi(cfg.HTTPPort)
	if err != nil || port < 1 || port > 65535 {
		return Config{}, fmt.Errorf(
			"HTTP_PORT must be a valid port between 1 and 65535",
		)
	}

	if cfg.DatabaseUrl == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	return cfg, nil
}

// Reconciliation contains the reconciliation worker's settings.
type Reconciliation struct {
	PollInterval    time.Duration
	BatchSize       int
	LeaseDuration   time.Duration
	GracePeriod     time.Duration
	MaxAttempts     int
	BackoffBase     time.Duration
	BackoffMax      time.Duration
	StorageProvider string
}

// LoadReconciliation reads the worker settings. Durations use Go syntax
// such as "15s" or "10m".
func LoadReconciliation() (Reconciliation, error) {
	cfg := Reconciliation{
		StorageProvider: getEnv("STORAGE_PROVIDER", "s3"),
	}

	durations := []struct {
		key      string
		fallback string
		target   *time.Duration
	}{
		{"RECONCILIATION_POLL_INTERVAL", "15s", &cfg.PollInterval},
		{"RECONCILIATION_LEASE_DURATION", "10m", &cfg.LeaseDuration},
		{"RECONCILIATION_GRACE_PERIOD", "2m", &cfg.GracePeriod},
		{"RECONCILIATION_BACKOFF_BASE", "30s", &cfg.BackoffBase},
		{"RECONCILIATION_BACKOFF_MAX", "30m", &cfg.BackoffMax},
	}
	for _, d := range durations {
		value, err := time.ParseDuration(getEnv(d.key, d.fallback))
		if err != nil || value <= 0 {
			return Reconciliation{}, fmt.Errorf("%s must be a positive duration such as 30s", d.key)
		}
		*d.target = value
	}

	ints := []struct {
		key      string
		fallback string
		target   *int
	}{
		{"RECONCILIATION_BATCH_SIZE", "10", &cfg.BatchSize},
		{"RECONCILIATION_MAX_ATTEMPTS", "8", &cfg.MaxAttempts},
	}
	for _, n := range ints {
		value, err := strconv.Atoi(getEnv(n.key, n.fallback))
		if err != nil || value <= 0 {
			return Reconciliation{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
		*n.target = value
	}

	if cfg.BackoffMax < cfg.BackoffBase {
		return Reconciliation{}, fmt.Errorf("RECONCILIATION_BACKOFF_MAX must not be less than RECONCILIATION_BACKOFF_BASE")
	}

	return cfg, nil
}

// Processing contains the processing worker's settings.
type Processing struct {
	PollInterval    time.Duration
	BatchSize       int
	Concurrency     int
	LeaseDuration   time.Duration
	MaxAttempts     int
	BackoffBase     time.Duration
	BackoffMax      time.Duration
	MaxFileBytes    int64
	AllowedTypes    []string
	Scanner         string // "none" or "clamav"
	ClamAVAddress   string
	ClamAVTimeout   time.Duration
	ScanPolicy      string // used when Scanner is "none"
	PromotionBucket string // empty: the quarantine bucket
	PromotionPrefix string
}

// LoadProcessing reads the processing worker settings.
func LoadProcessing() (Processing, error) {
	cfg := Processing{
		Scanner:         strings.ToLower(getEnv("SCANNER", "none")),
		ClamAVAddress:   getEnv("CLAMAV_ADDRESS", "127.0.0.1:3310"),
		ScanPolicy:      strings.ToLower(getEnv("SCAN_UNAVAILABLE_POLICY", "hold")),
		PromotionBucket: getEnv("PROMOTION_BUCKET", ""),
		PromotionPrefix: getEnv("PROMOTION_PREFIX", "assets"),
	}

	var err error
	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"PROCESSING_POLL_INTERVAL", "10s", &cfg.PollInterval},
		{"PROCESSING_LEASE_DURATION", "15m", &cfg.LeaseDuration},
		{"PROCESSING_BACKOFF_BASE", "30s", &cfg.BackoffBase},
		{"PROCESSING_BACKOFF_MAX", "30m", &cfg.BackoffMax},
		{"CLAMAV_TIMEOUT", "2m", &cfg.ClamAVTimeout},
	}
	for _, d := range durations {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Processing{}, fmt.Errorf("%s must be a positive duration such as 30s", d.key)
		}
	}

	ints := []struct {
		key, fallback string
		target        *int
	}{
		{"PROCESSING_BATCH_SIZE", "10", &cfg.BatchSize},
		{"PROCESSING_CONCURRENCY", "2", &cfg.Concurrency},
		{"PROCESSING_MAX_ATTEMPTS", "8", &cfg.MaxAttempts},
	}
	for _, n := range ints {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Processing{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}

	cfg.MaxFileBytes, err = strconv.ParseInt(getEnv("PROCESSING_MAX_FILE_BYTES", "5368709120"), 10, 64)
	if err != nil || cfg.MaxFileBytes <= 0 {
		return Processing{}, fmt.Errorf("PROCESSING_MAX_FILE_BYTES must be a positive integer")
	}

	for _, t := range strings.Split(getEnv("PROCESSING_ALLOWED_CONTENT_TYPES", ""), ",") {
		if t = strings.TrimSpace(t); t != "" {
			cfg.AllowedTypes = append(cfg.AllowedTypes, t)
		}
	}

	if cfg.Scanner != "none" && cfg.Scanner != "clamav" {
		return Processing{}, fmt.Errorf("SCANNER must be none or clamav")
	}
	switch cfg.ScanPolicy {
	case "hold", "reject":
	case "allow_unscanned":
		// Unscanned files reach delivery under this policy, so it is
		// refused anywhere but local development.
		if !strings.EqualFold(getEnv("APP_ENV", "development"), "development") {
			return Processing{}, fmt.Errorf("SCAN_UNAVAILABLE_POLICY=allow_unscanned is only allowed when APP_ENV=development")
		}
	default:
		return Processing{}, fmt.Errorf("SCAN_UNAVAILABLE_POLICY must be hold, reject or allow_unscanned")
	}
	if cfg.BackoffMax < cfg.BackoffBase {
		return Processing{}, fmt.Errorf("PROCESSING_BACKOFF_MAX must not be less than PROCESSING_BACKOFF_BASE")
	}

	return cfg, nil
}

// Delivery contains the asset delivery settings.
type Delivery struct {
	// Bucket and Prefix must match where promotion writes (PROMOTION_*).
	Bucket       string // empty: S3_BUCKET
	Prefix       string
	DefaultTTL   time.Duration
	MaxTTL       time.Duration
	ProxyEnabled bool

	CDNEnabled bool
	CDNBaseURL string
	// CDNKeyPairID and CDNPrivateKeyFile sign CDN URLs. The key is read
	// from a file so it never sits in the environment or the logs.
	CDNKeyPairID      string
	CDNPrivateKeyFile string
}

// LoadDelivery reads the delivery settings.
func LoadDelivery() (Delivery, error) {
	cfg := Delivery{
		Bucket:            getEnv("PROMOTION_BUCKET", ""),
		Prefix:            getEnv("PROMOTION_PREFIX", "assets"),
		ProxyEnabled:      strings.EqualFold(getEnv("DELIVERY_PROXY_ENABLED", "true"), "true"),
		CDNEnabled:        strings.EqualFold(getEnv("DELIVERY_CDN_ENABLED", "false"), "true"),
		CDNBaseURL:        getEnv("DELIVERY_CDN_BASE_URL", ""),
		CDNKeyPairID:      getEnv("DELIVERY_CDN_KEY_PAIR_ID", ""),
		CDNPrivateKeyFile: getEnv("DELIVERY_CDN_PRIVATE_KEY_FILE", ""),
	}

	var err error
	if cfg.DefaultTTL, err = time.ParseDuration(getEnv("DELIVERY_URL_TTL", "5m")); err != nil || cfg.DefaultTTL <= 0 {
		return Delivery{}, fmt.Errorf("DELIVERY_URL_TTL must be a positive duration such as 5m")
	}
	if cfg.MaxTTL, err = time.ParseDuration(getEnv("DELIVERY_URL_MAX_TTL", "15m")); err != nil || cfg.MaxTTL < cfg.DefaultTTL {
		return Delivery{}, fmt.Errorf("DELIVERY_URL_MAX_TTL must be a duration no shorter than DELIVERY_URL_TTL")
	}

	// A CDN is only ever used with signed URLs; there is no public mode.
	if cfg.CDNEnabled && (cfg.CDNBaseURL == "" || cfg.CDNKeyPairID == "" || cfg.CDNPrivateKeyFile == "") {
		return Delivery{}, fmt.Errorf("DELIVERY_CDN_ENABLED requires DELIVERY_CDN_BASE_URL, DELIVERY_CDN_KEY_PAIR_ID and DELIVERY_CDN_PRIVATE_KEY_FILE")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return fallback
	}

	return value
}
