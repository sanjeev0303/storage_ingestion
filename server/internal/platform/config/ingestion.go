package config

import (
	"fmt"
	"strconv"
	"time"
)

// Ingestion contains the bulk-ingestion limits: manifest and path rules,
// upload strategy, concurrency caps, quotas and worker timing.
type Ingestion struct {
	// SmallFileThreshold: files up to this size use one presigned PUT;
	// larger files use multipart upload.
	SmallFileThreshold int64
	MaxObjectBytes     int64
	// MaxManifestBytes caps a staged manifest; MaxInlineManifestBytes caps
	// one sent in the request body.
	MaxManifestBytes       int64
	MaxInlineManifestBytes int64
	MaxEntriesPerJob       int
	MaxLineBytes           int
	MaxPathBytes           int
	MaxSegmentBytes        int
	MaxPathDepth           int
	MaxMetadataBytes       int
	MaxMetadataKeys        int

	// Tenant defaults, read from the QUOTA_* settings every quota uses;
	// tenant_quotas overrides them per tenant.
	TenantStorageQuota int64
	MaxActiveJobs      int
	MaxPendingEntries  int64

	// Upload slots: global, per tenant, per job. A tenant's share also
	// shrinks to a fair share of the global slots under contention.
	GlobalUploadSlots int
	TenantUploadSlots int
	JobUploadSlots    int
	// TenantMultipartSessions caps a tenant's open multipart uploads, a
	// subset of its slots; MaxPartURLs caps part URLs per request.
	TenantMultipartSessions int
	MaxPartURLs             int
	MaxClaim                int

	// TenantProcessingBacklog: once this many of a tenant's entries wait
	// for processing, claims grant nothing until processing catches up.
	TenantProcessingBacklog int64

	MaxAttempts        int
	MaxManualRetries   int
	InsertBatch        int
	WorkerBatch        int
	StorageConcurrency int
	ParseConcurrency   int

	SlotLease         time.Duration
	UploadURLTTL      time.Duration
	RetryBackoffBase  time.Duration
	RetryBackoffMax   time.Duration
	PollInterval      time.Duration
	ManifestLease     time.Duration
	ParseBudget       time.Duration
	OutcomeSyncAfter  time.Duration
	ReconcileInterval time.Duration
	StorageTimeout    time.Duration

	MetricsAddr string
}

// LoadIngestion reads the bulk-ingestion settings.
func LoadIngestion() (Ingestion, error) {
	cfg := Ingestion{MetricsAddr: getEnv("METRICS_ADDR", "")}

	var err error
	sizes := []struct {
		key, fallback string
		target        *int64
	}{
		{"INGESTION_SMALL_FILE_THRESHOLD", "16777216", &cfg.SmallFileThreshold},
		{"INGESTION_MAX_OBJECT_BYTES", "53687091200", &cfg.MaxObjectBytes},
		{"INGESTION_MAX_MANIFEST_BYTES", "2147483648", &cfg.MaxManifestBytes},
		{"INGESTION_MAX_INLINE_MANIFEST_BYTES", "16777216", &cfg.MaxInlineManifestBytes},
		{"QUOTA_MAX_STORAGE_BYTES", "1099511627776", &cfg.TenantStorageQuota},
		{"QUOTA_MAX_PENDING_ENTRIES", "10000000", &cfg.MaxPendingEntries},
		{"INGESTION_TENANT_PROCESSING_BACKLOG", "2000", &cfg.TenantProcessingBacklog},
	}
	for _, s := range sizes {
		if *s.target, err = strconv.ParseInt(getEnv(s.key, s.fallback), 10, 64); err != nil || *s.target <= 0 {
			return Ingestion{}, fmt.Errorf("%s must be a positive integer", s.key)
		}
	}

	ints := []struct {
		key, fallback string
		target        *int
	}{
		{"INGESTION_MAX_ENTRIES_PER_JOB", "5000000", &cfg.MaxEntriesPerJob},
		{"INGESTION_MAX_LINE_BYTES", "16384", &cfg.MaxLineBytes},
		{"INGESTION_MAX_PATH_BYTES", "1024", &cfg.MaxPathBytes},
		{"INGESTION_MAX_SEGMENT_BYTES", "255", &cfg.MaxSegmentBytes},
		{"INGESTION_MAX_PATH_DEPTH", "64", &cfg.MaxPathDepth},
		{"INGESTION_MAX_METADATA_BYTES", "2048", &cfg.MaxMetadataBytes},
		{"INGESTION_MAX_METADATA_KEYS", "32", &cfg.MaxMetadataKeys},
		{"QUOTA_MAX_ACTIVE_JOBS", "10", &cfg.MaxActiveJobs},
		{"INGESTION_GLOBAL_UPLOAD_SLOTS", "256", &cfg.GlobalUploadSlots},
		{"INGESTION_TENANT_UPLOAD_SLOTS", "64", &cfg.TenantUploadSlots},
		{"INGESTION_JOB_UPLOAD_SLOTS", "32", &cfg.JobUploadSlots},
		{"INGESTION_TENANT_MULTIPART_SESSIONS", "16", &cfg.TenantMultipartSessions},
		{"INGESTION_MAX_PART_URLS", "100", &cfg.MaxPartURLs},
		{"INGESTION_MAX_CLAIM", "100", &cfg.MaxClaim},
		{"INGESTION_MAX_ATTEMPTS", "5", &cfg.MaxAttempts},
		{"INGESTION_MAX_MANUAL_RETRIES", "3", &cfg.MaxManualRetries},
		{"INGESTION_INSERT_BATCH", "1000", &cfg.InsertBatch},
		{"INGESTION_WORKER_BATCH", "500", &cfg.WorkerBatch},
		{"INGESTION_STORAGE_CONCURRENCY", "32", &cfg.StorageConcurrency},
		{"INGESTION_PARSE_CONCURRENCY", "2", &cfg.ParseConcurrency},
	}
	for _, n := range ints {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Ingestion{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}

	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"INGESTION_SLOT_LEASE", "15m", &cfg.SlotLease},
		{"INGESTION_UPLOAD_URL_TTL", "15m", &cfg.UploadURLTTL},
		{"INGESTION_RETRY_BACKOFF_BASE", "30s", &cfg.RetryBackoffBase},
		{"INGESTION_RETRY_BACKOFF_MAX", "30m", &cfg.RetryBackoffMax},
		{"INGESTION_POLL_INTERVAL", "5s", &cfg.PollInterval},
		{"INGESTION_MANIFEST_LEASE", "2m", &cfg.ManifestLease},
		{"INGESTION_PARSE_BUDGET", "5m", &cfg.ParseBudget},
		{"INGESTION_OUTCOME_SYNC_AFTER", "2m", &cfg.OutcomeSyncAfter},
		{"INGESTION_RECONCILE_INTERVAL", "10m", &cfg.ReconcileInterval},
		{"INGESTION_STORAGE_TIMEOUT", "30s", &cfg.StorageTimeout},
	}
	for _, d := range durations {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Ingestion{}, fmt.Errorf("%s must be a positive duration such as 30s", d.key)
		}
	}

	if err := cfg.Validate(); err != nil {
		return Ingestion{}, err
	}
	return cfg, nil
}

// Validate checks that the limits fit together.
func (c Ingestion) Validate() error {
	switch {
	case c.SmallFileThreshold < 1<<20 || c.SmallFileThreshold > 5<<30:
		// One PUT can store at most 5 GiB.
		return fmt.Errorf("INGESTION_SMALL_FILE_THRESHOLD must be between 1 MiB and 5 GiB")
	case c.MaxObjectBytes < c.SmallFileThreshold:
		return fmt.Errorf("INGESTION_MAX_OBJECT_BYTES must not be below INGESTION_SMALL_FILE_THRESHOLD")
	case c.MaxInlineManifestBytes > c.MaxManifestBytes:
		return fmt.Errorf("INGESTION_MAX_INLINE_MANIFEST_BYTES must not exceed INGESTION_MAX_MANIFEST_BYTES")
	case c.TenantUploadSlots > c.GlobalUploadSlots || c.JobUploadSlots > c.TenantUploadSlots:
		return fmt.Errorf("upload slots must not grow from global to tenant to job")
	case c.TenantMultipartSessions > c.TenantUploadSlots:
		return fmt.Errorf("INGESTION_TENANT_MULTIPART_SESSIONS must not exceed INGESTION_TENANT_UPLOAD_SLOTS")
	case c.MaxPartURLs > 10000:
		return fmt.Errorf("INGESTION_MAX_PART_URLS must be at most 10000, the multipart part limit")
	case c.MaxLineBytes < c.MaxPathBytes+c.MaxMetadataBytes:
		return fmt.Errorf("INGESTION_MAX_LINE_BYTES must hold the longest path plus the largest metadata")
	case c.RetryBackoffMax < c.RetryBackoffBase:
		return fmt.Errorf("INGESTION_RETRY_BACKOFF_MAX must not be less than its base")
	case c.UploadURLTTL > c.SlotLease:
		return fmt.Errorf("INGESTION_UPLOAD_URL_TTL must not outlive INGESTION_SLOT_LEASE")
	}
	return nil
}
