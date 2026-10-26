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
	// Upload limits shared by the API and the workers, so every process
	// judges a session by the same rules.
	UploadMaxSize    int64
	UploadSessionTTL time.Duration
	UploadURLTTL     time.Duration
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

	cfg.UploadMaxSize, err = strconv.ParseInt(getEnv("UPLOAD_MAX_SIZE_BYTES", "5368709120"), 10, 64)
	if err != nil || cfg.UploadMaxSize <= 0 {
		return Config{}, fmt.Errorf("UPLOAD_MAX_SIZE_BYTES must be a positive number of bytes")
	}
	// The largest object an S3-family or GCS bucket holds, and so the
	// largest any adapter can promote; refused here rather than after an
	// upload that size.
	if cfg.UploadMaxSize > 5<<40 {
		return Config{}, fmt.Errorf("UPLOAD_MAX_SIZE_BYTES must be at most 5 TiB (%d bytes)", int64(5<<40))
	}
	seconds := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"UPLOAD_SESSION_TTL_SECONDS", "1800", &cfg.UploadSessionTTL},
		{"UPLOAD_URL_TTL_SECONDS", "900", &cfg.UploadURLTTL},
	}
	for _, s := range seconds {
		n, err := strconv.Atoi(getEnv(s.key, s.fallback))
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive number of seconds", s.key)
		}
		*s.target = time.Duration(n) * time.Second
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

	DryRun       bool
	ScanPageSize int
	ScanPages    int
	OrphanMinAge time.Duration
	StuckAfter   time.Duration
	// SessionScan is provider (AWS S3), database (MinIO) or off.
	SessionScan     string
	SessionLookback time.Duration
	// DeleteUnrecorded: RECONCILIATION_UNRECORDED_ACTION=delete (default report).
	DeleteUnrecorded bool
	// MetricsAddr, when set, serves /debug/vars there (e.g. 127.0.0.1:9101).
	MetricsAddr string
}

// LoadReconciliation reads the worker settings. Durations use Go syntax
// such as "15s" or "10m".
func LoadReconciliation() (Reconciliation, error) {
	cfg := Reconciliation{
		StorageProvider:  getEnv("STORAGE_PROVIDER", "s3"),
		DryRun:           strings.EqualFold(getEnv("RECONCILIATION_DRY_RUN", "false"), "true"),
		MetricsAddr:      getEnv("METRICS_ADDR", ""),
		SessionScan:      strings.ToLower(getEnv("RECONCILIATION_SESSION_SCAN", "provider")),
		DeleteUnrecorded: strings.EqualFold(getEnv("RECONCILIATION_UNRECORDED_ACTION", "report"), "delete"),
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
		{"RECONCILIATION_ORPHAN_MIN_AGE", "24h", &cfg.OrphanMinAge},
		{"RECONCILIATION_STUCK_AFTER", "1h", &cfg.StuckAfter},
		{"RECONCILIATION_SESSION_LOOKBACK", "168h", &cfg.SessionLookback},
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
		{"RECONCILIATION_SCAN_PAGE_SIZE", "100", &cfg.ScanPageSize},
		{"RECONCILIATION_SCAN_PAGES", "5", &cfg.ScanPages},
	}
	for _, n := range ints {
		value, err := strconv.Atoi(getEnv(n.key, n.fallback))
		if err != nil || value <= 0 {
			return Reconciliation{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
		*n.target = value
	}

	if cfg.SessionScan != "provider" && cfg.SessionScan != "database" && cfg.SessionScan != "off" {
		return Reconciliation{}, fmt.Errorf("RECONCILIATION_SESSION_SCAN must be provider, database or off")
	}
	if cfg.ScanPageSize > 1000 {
		return Reconciliation{}, fmt.Errorf("RECONCILIATION_SCAN_PAGE_SIZE must be at most 1000")
	}
	if cfg.BackoffMax < cfg.BackoffBase {
		return Reconciliation{}, fmt.Errorf("RECONCILIATION_BACKOFF_MAX must not be less than RECONCILIATION_BACKOFF_BASE")
	}

	return cfg, nil
}

// Processing contains the processing worker's settings.
type Processing struct {
	PollInterval  time.Duration
	BatchSize     int
	Concurrency   int
	LeaseDuration time.Duration
	// RenewEvery renews a claimed job's lease; MaxJobDuration bounds how
	// long renewal keeps one job, from its claim.
	RenewEvery      time.Duration
	MaxJobDuration  time.Duration
	MaxAttempts     int
	BackoffBase     time.Duration
	BackoffMax      time.Duration
	MaxFileBytes    int64
	AllowedTypes    []string
	DeniedTypes     []string // added to the built-in deny list
	UnknownType     string   // "allow" or "reject"
	Scanner         string   // "none" or "clamav"
	ClamAVAddress   string
	ClamAVTimeout   time.Duration
	ScanPolicy      string // used when Scanner is "none"
	PromotionBucket string // empty: the quarantine bucket
	PromotionPrefix string

	QuarantineRetention time.Duration

	// Archive inspection is off unless ArchiveInspection is set.
	ArchiveInspection  bool
	ArchiveMaxBytes    int64
	ArchiveMaxEntries  int
	ArchiveMaxExpanded int64
	ArchiveMaxRatio    int64
	MetricsAddr        string
}

// LoadProcessing reads the processing worker settings.
func LoadProcessing() (Processing, error) {
	cfg := Processing{
		Scanner:         strings.ToLower(getEnv("SCANNER", "none")),
		ClamAVAddress:   getEnv("CLAMAV_ADDRESS", "127.0.0.1:3310"),
		ScanPolicy:      strings.ToLower(getEnv("SCAN_UNAVAILABLE_POLICY", "hold")),
		PromotionBucket: getEnv("PROMOTION_BUCKET", ""),
		PromotionPrefix: PromotionPrefix(),
		UnknownType:     strings.ToLower(getEnv("PROCESSING_UNKNOWN_TYPE_ACTION", "allow")),

		ArchiveInspection: strings.EqualFold(getEnv("ARCHIVE_INSPECTION_ENABLED", "false"), "true"),
		MetricsAddr:       getEnv("METRICS_ADDR", ""),
	}

	var err error
	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"PROCESSING_POLL_INTERVAL", "10s", &cfg.PollInterval},
		{"PROCESSING_LEASE_DURATION", "1m", &cfg.LeaseDuration},
		{"PROCESSING_MAX_JOB_DURATION", "30m", &cfg.MaxJobDuration},
		{"PROCESSING_BACKOFF_BASE", "30s", &cfg.BackoffBase},
		{"PROCESSING_BACKOFF_MAX", "30m", &cfg.BackoffMax},
		{"CLAMAV_TIMEOUT", "2m", &cfg.ClamAVTimeout},
		{"QUARANTINE_RETENTION", "720h", &cfg.QuarantineRetention},
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
		{"ARCHIVE_MAX_ENTRIES", "10000", &cfg.ArchiveMaxEntries},
	}
	for _, n := range ints {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Processing{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}

	int64s := []struct {
		key, fallback string
		target        *int64
	}{
		{"PROCESSING_MAX_FILE_BYTES", "5368709120", &cfg.MaxFileBytes},
		{"ARCHIVE_MAX_BYTES", "536870912", &cfg.ArchiveMaxBytes},
		{"ARCHIVE_MAX_UNCOMPRESSED_BYTES", "2147483648", &cfg.ArchiveMaxExpanded},
		{"ARCHIVE_MAX_RATIO", "100", &cfg.ArchiveMaxRatio},
	}
	for _, n := range int64s {
		if *n.target, err = strconv.ParseInt(getEnv(n.key, n.fallback), 10, 64); err != nil || *n.target <= 0 {
			return Processing{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}

	cfg.AllowedTypes = splitList(getEnv("PROCESSING_ALLOWED_CONTENT_TYPES", ""))
	cfg.DeniedTypes = splitList(getEnv("PROCESSING_DENIED_CONTENT_TYPES", ""))

	if cfg.UnknownType != "allow" && cfg.UnknownType != "reject" {
		return Processing{}, fmt.Errorf("PROCESSING_UNKNOWN_TYPE_ACTION must be allow or reject")
	}
	cfg.RenewEvery = cfg.LeaseDuration / 3
	if raw := getEnv("PROCESSING_LEASE_RENEW_INTERVAL", ""); raw != "" {
		if cfg.RenewEvery, err = time.ParseDuration(raw); err != nil || cfg.RenewEvery <= 0 {
			return Processing{}, fmt.Errorf("PROCESSING_LEASE_RENEW_INTERVAL must be a positive duration such as 20s")
		}
	}
	if 2*cfg.RenewEvery > cfg.LeaseDuration {
		return Processing{}, fmt.Errorf("PROCESSING_LEASE_RENEW_INTERVAL must be at most half of PROCESSING_LEASE_DURATION")
	}
	if cfg.MaxJobDuration < cfg.LeaseDuration {
		return Processing{}, fmt.Errorf("PROCESSING_MAX_JOB_DURATION must not be shorter than PROCESSING_LEASE_DURATION")
	}
	if cfg.ClamAVTimeout >= cfg.MaxJobDuration {
		return Processing{}, fmt.Errorf("CLAMAV_TIMEOUT must be shorter than PROCESSING_MAX_JOB_DURATION")
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
	// URLMode is "storage" (presigned, not revocable) or "token"
	// (revocable API links) when a request names no mode.
	URLMode string
	// StallTimeout ends a proxied download when no bytes move for this long.
	StallTimeout time.Duration
	// MaxStreams caps concurrent proxied downloads.
	MaxStreams int
	// MetricsAddr, when set, serves /debug/vars there.
	MetricsAddr string

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
		Prefix:            PromotionPrefix(),
		ProxyEnabled:      strings.EqualFold(getEnv("DELIVERY_PROXY_ENABLED", "true"), "true"),
		CDNEnabled:        strings.EqualFold(getEnv("DELIVERY_CDN_ENABLED", "false"), "true"),
		CDNBaseURL:        getEnv("DELIVERY_CDN_BASE_URL", ""),
		CDNKeyPairID:      getEnv("DELIVERY_CDN_KEY_PAIR_ID", ""),
		CDNPrivateKeyFile: getEnv("DELIVERY_CDN_PRIVATE_KEY_FILE", ""),
		URLMode:           strings.ToLower(getEnv("DELIVERY_URL_MODE", "storage")),
		MetricsAddr:       getEnv("METRICS_ADDR", ""),
	}

	var err error
	if cfg.URLMode != "storage" && cfg.URLMode != "token" {
		return Delivery{}, fmt.Errorf("DELIVERY_URL_MODE must be storage or token")
	}
	// Token links are served by the proxy routes.
	if cfg.URLMode == "token" && !cfg.ProxyEnabled {
		return Delivery{}, fmt.Errorf("DELIVERY_URL_MODE=token requires DELIVERY_PROXY_ENABLED=true")
	}
	if cfg.StallTimeout, err = time.ParseDuration(getEnv("DELIVERY_STALL_TIMEOUT", "30s")); err != nil || cfg.StallTimeout <= 0 {
		return Delivery{}, fmt.Errorf("DELIVERY_STALL_TIMEOUT must be a positive duration such as 30s")
	}
	if cfg.MaxStreams, err = strconv.Atoi(getEnv("DELIVERY_MAX_STREAMS", "64")); err != nil || cfg.MaxStreams <= 0 {
		return Delivery{}, fmt.Errorf("DELIVERY_MAX_STREAMS must be a positive integer")
	}
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

// splitList reads a comma-separated list, dropping empty entries.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// PromotionPrefix is where the platform publishes objects in its delivery
// bucket (PROMOTION_PREFIX, "assets" by default). Every process that
// reaches platform objects reads it here, so none can disagree.
func PromotionPrefix() string {
	prefix := strings.Trim(getEnv("PROMOTION_PREFIX", "assets"), "/")
	if prefix == "" {
		return "assets"
	}
	return prefix
}

func getEnv(key, fallback string) string {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return fallback
	}

	return value
}
