package config

import (
	"fmt"
	"strconv"
	"time"
)

// Datasets bounds managed datasets: pages, bulk changes, snapshots,
// manifest imports, validation jobs and lineage walks. Every limit is a
// ceiling the service enforces; tenant quotas may lower the manifest ones.
type Datasets struct {
	DefaultPageSize int
	MaxPageSize     int
	// MaxBulkMembers caps the members one request may add.
	MaxBulkMembers int
	// MaxSnapshotMembers caps a version's size.
	MaxSnapshotMembers int64

	MaxInlineManifestBytes int64
	MaxManifestBytes       int64
	MaxManifestLineBytes   int
	MaxManifestEntries     int64
	// ImportBatch is how many manifest lines one transaction stores.
	ImportBatch int
	// MaxImportErrors caps the rejected lines an import records; all are
	// counted.
	MaxImportErrors int64
	// ImportBudget is how long one worker pass reads an import before it
	// lets go, so a huge manifest cannot hold a worker.
	ImportBudget time.Duration

	ValidationBatch int
	// ContentConcurrency caps the objects one validation reads at once;
	// ObjectTimeout bounds each read.
	ContentConcurrency int
	ObjectTimeout      time.Duration
	ValidationBudget   time.Duration
	// MaxJobDuration fails a validation still unfinished this long after
	// it started.
	MaxJobDuration    time.Duration
	DefaultSampleSize int
	MaxSampleSize     int
	// MaxFindingsPerRule caps the findings one rule stores per job; the
	// rest are counted.
	MaxFindingsPerRule int64
	// DistributionTop is how many values a report lists per distribution.
	DistributionTop int

	// Per tenant: unfinished imports and validations, and validations the
	// workers run at once.
	MaxActiveImports     int
	MaxActiveValidations int
	RunningPerTenant     int
	Lease                time.Duration
	MaxAttempts          int
	BackoffBase          time.Duration
	BackoffMax           time.Duration
	MaxLineageDepth      int
	MaxLineageEdges      int
	LineageQueryTimeout  time.Duration
	PollInterval         time.Duration
	WorkerSlots          int
	MetricsAddr          string
}

// LoadDatasets reads DATASET_* and refuses limits that would leave a
// request or a job unbounded.
func LoadDatasets() (Datasets, error) {
	var c Datasets
	var err error
	ints := []struct {
		key, fallback string
		target        *int
	}{
		{"DATASET_DEFAULT_PAGE_SIZE", "100", &c.DefaultPageSize},
		{"DATASET_MAX_PAGE_SIZE", "1000", &c.MaxPageSize},
		{"DATASET_MAX_BULK_MEMBERS", "1000", &c.MaxBulkMembers},
		{"DATASET_MANIFEST_MAX_LINE_BYTES", "65536", &c.MaxManifestLineBytes},
		{"DATASET_IMPORT_BATCH", "500", &c.ImportBatch},
		{"DATASET_VALIDATION_BATCH", "500", &c.ValidationBatch},
		{"DATASET_CONTENT_CONCURRENCY", "4", &c.ContentConcurrency},
		{"DATASET_DEFAULT_SAMPLE_SIZE", "100", &c.DefaultSampleSize},
		{"DATASET_MAX_SAMPLE_SIZE", "10000", &c.MaxSampleSize},
		{"DATASET_DISTRIBUTION_TOP", "50", &c.DistributionTop},
		{"DATASET_MAX_ACTIVE_IMPORTS", "10", &c.MaxActiveImports},
		{"DATASET_MAX_ACTIVE_VALIDATIONS", "10", &c.MaxActiveValidations},
		{"DATASET_RUNNING_PER_TENANT", "2", &c.RunningPerTenant},
		{"DATASET_MAX_ATTEMPTS", "5", &c.MaxAttempts},
		{"DATASET_LINEAGE_MAX_DEPTH", "10", &c.MaxLineageDepth},
		{"DATASET_LINEAGE_MAX_EDGES", "10000", &c.MaxLineageEdges},
		{"DATASET_WORKER_SLOTS", "2", &c.WorkerSlots},
	}
	for _, n := range ints {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Datasets{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}
	int64s := []struct {
		key, fallback string
		target        *int64
	}{
		{"DATASET_MAX_SNAPSHOT_MEMBERS", "5000000", &c.MaxSnapshotMembers},
		{"DATASET_MAX_INLINE_MANIFEST_BYTES", "8388608", &c.MaxInlineManifestBytes},
		{"DATASET_MAX_MANIFEST_BYTES", "1073741824", &c.MaxManifestBytes},
		{"DATASET_MAX_MANIFEST_ENTRIES", "10000000", &c.MaxManifestEntries},
		{"DATASET_MAX_IMPORT_ERRORS", "10000", &c.MaxImportErrors},
		{"DATASET_MAX_FINDINGS_PER_RULE", "10000", &c.MaxFindingsPerRule},
	}
	for _, n := range int64s {
		if *n.target, err = strconv.ParseInt(getEnv(n.key, n.fallback), 10, 64); err != nil || *n.target <= 0 {
			return Datasets{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}
	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"DATASET_IMPORT_BUDGET", "1m", &c.ImportBudget},
		{"DATASET_OBJECT_TIMEOUT", "1m", &c.ObjectTimeout},
		{"DATASET_VALIDATION_BUDGET", "1m", &c.ValidationBudget},
		{"DATASET_MAX_JOB_DURATION", "6h", &c.MaxJobDuration},
		{"DATASET_LEASE", "3m", &c.Lease},
		{"DATASET_BACKOFF_BASE", "5s", &c.BackoffBase},
		{"DATASET_BACKOFF_MAX", "5m", &c.BackoffMax},
		{"DATASET_LINEAGE_QUERY_TIMEOUT", "5s", &c.LineageQueryTimeout},
		{"DATASET_POLL_INTERVAL", "5s", &c.PollInterval},
	}
	for _, d := range durations {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Datasets{}, fmt.Errorf("%s must be a positive duration", d.key)
		}
	}
	c.MetricsAddr = getEnv("DATASET_METRICS_ADDR", "")

	switch {
	case c.MaxPageSize > 10000:
		return Datasets{}, fmt.Errorf("DATASET_MAX_PAGE_SIZE must be at most 10000")
	case c.DefaultPageSize > c.MaxPageSize:
		return Datasets{}, fmt.Errorf("DATASET_DEFAULT_PAGE_SIZE must not exceed DATASET_MAX_PAGE_SIZE")
	case c.MaxBulkMembers > 10000:
		return Datasets{}, fmt.Errorf("DATASET_MAX_BULK_MEMBERS must be at most 10000")
	case c.MaxInlineManifestBytes > c.MaxManifestBytes:
		return Datasets{}, fmt.Errorf("DATASET_MAX_INLINE_MANIFEST_BYTES must not exceed DATASET_MAX_MANIFEST_BYTES")
	case c.MaxInlineManifestBytes > 64<<20:
		return Datasets{}, fmt.Errorf("DATASET_MAX_INLINE_MANIFEST_BYTES must be at most 64 MiB")
	case c.MaxManifestLineBytes > 1<<20:
		return Datasets{}, fmt.Errorf("DATASET_MANIFEST_MAX_LINE_BYTES must be at most 1 MiB")
	case c.ImportBatch > 10000, c.ValidationBatch > 10000:
		return Datasets{}, fmt.Errorf("DATASET_IMPORT_BATCH and DATASET_VALIDATION_BATCH must be at most 10000")
	case c.ContentConcurrency > 64:
		return Datasets{}, fmt.Errorf("DATASET_CONTENT_CONCURRENCY must be at most 64")
	case c.DefaultSampleSize > c.MaxSampleSize:
		return Datasets{}, fmt.Errorf("DATASET_DEFAULT_SAMPLE_SIZE must not exceed DATASET_MAX_SAMPLE_SIZE")
	case c.DistributionTop > 1000:
		return Datasets{}, fmt.Errorf("DATASET_DISTRIBUTION_TOP must be at most 1000")
	case c.MaxLineageDepth > 50:
		return Datasets{}, fmt.Errorf("DATASET_LINEAGE_MAX_DEPTH must be at most 50")
	case c.MaxLineageEdges > 100000:
		return Datasets{}, fmt.Errorf("DATASET_LINEAGE_MAX_EDGES must be at most 100000")
	case c.Lease < 10*time.Second:
		return Datasets{}, fmt.Errorf("DATASET_LEASE must be at least 10s")
	case c.ImportBudget >= c.Lease || c.ValidationBudget >= c.Lease:
		// A pass renews its lease after every batch; the budget keeps one
		// pass shorter than a lease anyway.
		return Datasets{}, fmt.Errorf("DATASET_IMPORT_BUDGET and DATASET_VALIDATION_BUDGET must be shorter than DATASET_LEASE")
	case c.BackoffBase > c.BackoffMax:
		return Datasets{}, fmt.Errorf("DATASET_BACKOFF_BASE must not exceed DATASET_BACKOFF_MAX")
	}
	return c, nil
}
