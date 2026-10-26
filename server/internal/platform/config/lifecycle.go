package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Lifecycle contains the cleanup worker settings: retention, deletion,
// orphan cleanup and missing-object checks. Every destructive behaviour
// that is not an explicit request defaults to report-only.
type Lifecycle struct {
	PollInterval       time.Duration
	BatchSize          int
	Concurrency        int
	LeaseDuration      time.Duration
	MaxAttempts        int
	BackoffBase        time.Duration
	BackoffMax         time.Duration
	RecheckInterval    time.Duration
	MaxDeletionsPerRun int
	// ExpireAssets: LIFECYCLE_EXPIRATION=delete (default report).
	ExpireAssets bool

	// Platform retention: defaults for tenants without a policy, and the
	// floors no policy may go below.
	DefaultMinRetention  time.Duration
	DefaultMaxAge        time.Duration
	DefaultDeletionGrace time.Duration
	MinRetentionFloor    time.Duration
	DeletionGraceFloor   time.Duration
	// QuarantineRetention is both the default and the floor.
	QuarantineRetention time.Duration

	// DeleteOrphans: LIFECYCLE_ORPHAN_ACTION=delete (default report).
	DeleteOrphans bool
	// DeleteUnrecorded: LIFECYCLE_ORPHAN_UNRECORDED=delete (default report).
	DeleteUnrecorded      bool
	OrphanMinAge          time.Duration
	OrphanSafetyInterval  time.Duration
	OrphanObservations    int
	FailedUploadRetention time.Duration
	ScanPageSize          int
	ScanPages             int
	MaxScanErrors         int
	CheckBatch            int

	PromotionBucket string // empty: the quarantine bucket
	PromotionPrefix string
	MetricsAddr     string
}

// LoadLifecycle reads the cleanup worker settings.
func LoadLifecycle() (Lifecycle, error) {
	cfg := Lifecycle{
		PromotionBucket: getEnv("PROMOTION_BUCKET", ""),
		PromotionPrefix: PromotionPrefix(),
		MetricsAddr:     getEnv("METRICS_ADDR", ""),
	}

	var err error
	if cfg.ExpireAssets, err = action("LIFECYCLE_EXPIRATION"); err != nil {
		return Lifecycle{}, err
	}
	if cfg.DeleteOrphans, err = action("LIFECYCLE_ORPHAN_ACTION"); err != nil {
		return Lifecycle{}, err
	}
	if cfg.DeleteUnrecorded, err = action("LIFECYCLE_ORPHAN_UNRECORDED"); err != nil {
		return Lifecycle{}, err
	}

	positive := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"LIFECYCLE_POLL_INTERVAL", "1m", &cfg.PollInterval},
		{"LIFECYCLE_LEASE_DURATION", "5m", &cfg.LeaseDuration},
		{"LIFECYCLE_BACKOFF_BASE", "30s", &cfg.BackoffBase},
		{"LIFECYCLE_BACKOFF_MAX", "30m", &cfg.BackoffMax},
		{"LIFECYCLE_RECHECK_INTERVAL", "1h", &cfg.RecheckInterval},
		{"QUARANTINE_RETENTION", "720h", &cfg.QuarantineRetention},
		{"LIFECYCLE_ORPHAN_MIN_AGE", "24h", &cfg.OrphanMinAge},
		{"LIFECYCLE_ORPHAN_SAFETY_INTERVAL", "24h", &cfg.OrphanSafetyInterval},
		{"LIFECYCLE_FAILED_UPLOAD_RETENTION", "720h", &cfg.FailedUploadRetention},
	}
	for _, d := range positive {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Lifecycle{}, fmt.Errorf("%s must be a positive duration such as 30s", d.key)
		}
	}

	// Zero is meaningful for these: no minimum retention, never expire.
	nonNegative := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"RETENTION_DEFAULT_MIN_RETENTION", "0s", &cfg.DefaultMinRetention},
		{"RETENTION_DEFAULT_MAX_AGE", "0s", &cfg.DefaultMaxAge},
		{"RETENTION_DEFAULT_DELETION_GRACE", "168h", &cfg.DefaultDeletionGrace},
		{"RETENTION_MIN_RETENTION_FLOOR", "0s", &cfg.MinRetentionFloor},
		{"RETENTION_DELETION_GRACE_FLOOR", "1h", &cfg.DeletionGraceFloor},
	}
	for _, d := range nonNegative {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target < 0 {
			return Lifecycle{}, fmt.Errorf("%s must be a duration of zero or more, such as 168h", d.key)
		}
	}

	ints := []struct {
		key, fallback string
		target        *int
	}{
		{"LIFECYCLE_BATCH_SIZE", "100", &cfg.BatchSize},
		{"LIFECYCLE_CONCURRENCY", "2", &cfg.Concurrency},
		{"LIFECYCLE_MAX_ATTEMPTS", "8", &cfg.MaxAttempts},
		{"LIFECYCLE_MAX_DELETIONS", "100", &cfg.MaxDeletionsPerRun},
		{"LIFECYCLE_ORPHAN_OBSERVATIONS", "2", &cfg.OrphanObservations},
		{"LIFECYCLE_SCAN_PAGE_SIZE", "500", &cfg.ScanPageSize},
		{"LIFECYCLE_SCAN_PAGES", "10", &cfg.ScanPages},
		{"LIFECYCLE_MAX_SCAN_ERRORS", "5", &cfg.MaxScanErrors},
		{"LIFECYCLE_CHECK_BATCH", "200", &cfg.CheckBatch},
	}
	for _, n := range ints {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Lifecycle{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}

	switch {
	case cfg.BackoffMax < cfg.BackoffBase:
		return Lifecycle{}, fmt.Errorf("LIFECYCLE_BACKOFF_MAX must not be less than LIFECYCLE_BACKOFF_BASE")
	case cfg.DefaultMinRetention < cfg.MinRetentionFloor:
		return Lifecycle{}, fmt.Errorf("RETENTION_DEFAULT_MIN_RETENTION must not be below RETENTION_MIN_RETENTION_FLOOR")
	case cfg.DefaultDeletionGrace < cfg.DeletionGraceFloor:
		return Lifecycle{}, fmt.Errorf("RETENTION_DEFAULT_DELETION_GRACE must not be below RETENTION_DELETION_GRACE_FLOOR")
	case cfg.OrphanObservations < 2:
		return Lifecycle{}, fmt.Errorf("LIFECYCLE_ORPHAN_OBSERVATIONS must be at least 2")
	case cfg.ScanPageSize > 1000:
		return Lifecycle{}, fmt.Errorf("LIFECYCLE_SCAN_PAGE_SIZE must be at most 1000")
	}
	return cfg, nil
}

// Compaction contains the compaction worker settings.
type Compaction struct {
	PollInterval         time.Duration
	BatchSize            int
	MaxBatches           int
	BatchPause           time.Duration
	AuditRetention       time.Duration
	OperationalRetention time.Duration
	IdempotencyTTL       time.Duration
	// Enabled: COMPACTION_ENABLED=true. Off, every run is a dry run.
	Enabled     bool
	MetricsAddr string
}

// LoadCompaction reads the compaction worker settings. The retention
// floors themselves are enforced by the compaction service.
func LoadCompaction() (Compaction, error) {
	cfg := Compaction{
		Enabled:     strings.EqualFold(getEnv("COMPACTION_ENABLED", "false"), "true"),
		MetricsAddr: getEnv("METRICS_ADDR", ""),
	}

	var err error
	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"COMPACTION_POLL_INTERVAL", "1h", &cfg.PollInterval},
		{"COMPACTION_AUDIT_RETENTION", "8760h", &cfg.AuditRetention},
		{"COMPACTION_OPERATIONAL_RETENTION", "720h", &cfg.OperationalRetention},
		{"COMPACTION_IDEMPOTENCY_TTL", "168h", &cfg.IdempotencyTTL},
	}
	for _, d := range durations {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Compaction{}, fmt.Errorf("%s must be a positive duration such as 720h", d.key)
		}
	}
	if cfg.BatchPause, err = time.ParseDuration(getEnv("COMPACTION_BATCH_PAUSE", "0s")); err != nil || cfg.BatchPause < 0 {
		return Compaction{}, fmt.Errorf("COMPACTION_BATCH_PAUSE must be a duration of zero or more")
	}
	if cfg.BatchSize, err = strconv.Atoi(getEnv("COMPACTION_BATCH_SIZE", "1000")); err != nil || cfg.BatchSize <= 0 {
		return Compaction{}, fmt.Errorf("COMPACTION_BATCH_SIZE must be a positive integer")
	}
	if cfg.MaxBatches, err = strconv.Atoi(getEnv("COMPACTION_MAX_BATCHES", "100")); err != nil || cfg.MaxBatches <= 0 {
		return Compaction{}, fmt.Errorf("COMPACTION_MAX_BATCHES must be a positive integer")
	}
	return cfg, nil
}

// action reads a report|delete switch; report is the default.
func action(key string) (bool, error) {
	switch strings.ToLower(getEnv(key, "report")) {
	case "report":
		return false, nil
	case "delete":
		return true, nil
	}
	return false, fmt.Errorf("%s must be report or delete", key)
}
