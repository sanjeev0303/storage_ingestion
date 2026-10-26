package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Packing is the small-file packing policy (cmd/pack-worker).
type Packing struct {
	// Which assets: published at least MinAge ago, of MinObjectBytes to
	// MaxObjectBytes.
	MinObjectBytes int64
	MaxObjectBytes int64
	MinAge         time.Duration
	// Packs are filled to TargetPackBytes, never past MaxPackBytes or
	// MaxEntries entries.
	TargetPackBytes int64
	MaxPackBytes    int64
	MaxEntries      int
	// SourceConcurrency is how many sources one pack reads ahead; each is
	// held in memory, so a pack writer holds at most SourceConcurrency ×
	// MaxObjectBytes.
	SourceConcurrency int
	// ActiveWriters is how many packs one worker builds at once;
	// TenantActivePacks how many of one tenant's packs are built at once,
	// across workers, so no tenant takes every writer.
	ActiveWriters     int
	TenantActivePacks int
	// TenantsPerCycle bounds the tenants planned per cycle; planning goes
	// round them in turn. Tenants, when set, is the only ones packed.
	TenantsPerCycle int
	Tenants         []string
	// Jobs: a lease renewed every Lease/3, at most MaxJobDuration per
	// attempt, MaxAttempts attempts with backoff between RetryBase and
	// RetryMax.
	Lease          time.Duration
	MaxJobDuration time.Duration
	MaxAttempts    int
	RetryBase      time.Duration
	RetryMax       time.Duration
	// After publication: sources are deleted SourceRetireAfter later. A
	// pack is rewritten without its deleted entries once they are
	// RewriteDeadRatio of it, or a deletion has waited MaxDeletionDelay.
	// Retired and failed packs' objects are deleted RemoveAfter later.
	SourceRetireAfter time.Duration
	RewriteDeadRatio  float64
	MaxDeletionDelay  time.Duration
	RemoveAfter       time.Duration
	PollInterval      time.Duration
	MetricsAddr       string
}

// LoadPacking reads and validates the packing policy.
func LoadPacking() (Packing, error) {
	cfg := Packing{MetricsAddr: getEnv("METRICS_ADDR", "")}
	for _, t := range strings.Split(getEnv("PACKING_TENANTS", ""), ",") {
		if t = strings.TrimSpace(t); t != "" {
			cfg.Tenants = append(cfg.Tenants, t)
		}
	}

	sizes := []struct {
		key, fallback string
		target        *int64
	}{
		{"PACKING_MIN_OBJECT_BYTES", "1", &cfg.MinObjectBytes},
		{"PACKING_MAX_OBJECT_BYTES", "1048576", &cfg.MaxObjectBytes},
		{"PACKING_TARGET_PACK_BYTES", "67108864", &cfg.TargetPackBytes},
		{"PACKING_MAX_PACK_BYTES", "268435456", &cfg.MaxPackBytes},
	}
	for _, s := range sizes {
		v, err := strconv.ParseInt(getEnv(s.key, s.fallback), 10, 64)
		if err != nil {
			return Packing{}, fmt.Errorf("%s must be a number of bytes", s.key)
		}
		*s.target = v
	}
	counts := []struct {
		key, fallback string
		target        *int
	}{
		{"PACKING_MAX_ENTRIES", "10000", &cfg.MaxEntries},
		{"PACKING_SOURCE_CONCURRENCY", "16", &cfg.SourceConcurrency},
		{"PACKING_ACTIVE_WRITERS", "2", &cfg.ActiveWriters},
		{"PACKING_TENANT_ACTIVE_PACKS", "1", &cfg.TenantActivePacks},
		{"PACKING_TENANTS_PER_CYCLE", "16", &cfg.TenantsPerCycle},
		{"PACKING_MAX_ATTEMPTS", "5", &cfg.MaxAttempts},
	}
	for _, c := range counts {
		v, err := strconv.Atoi(getEnv(c.key, c.fallback))
		if err != nil {
			return Packing{}, fmt.Errorf("%s must be a whole number", c.key)
		}
		*c.target = v
	}
	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"PACKING_MIN_AGE", "1h", &cfg.MinAge},
		{"PACKING_LEASE", "2m", &cfg.Lease},
		{"PACKING_MAX_JOB_DURATION", "30m", &cfg.MaxJobDuration},
		{"PACKING_RETRY_BASE", "30s", &cfg.RetryBase},
		{"PACKING_RETRY_MAX", "30m", &cfg.RetryMax},
		{"PACKING_SOURCE_RETIRE_AFTER", "24h", &cfg.SourceRetireAfter},
		{"PACKING_MAX_DELETION_DELAY", "24h", &cfg.MaxDeletionDelay},
		{"PACKING_REMOVE_AFTER", "1h", &cfg.RemoveAfter},
		{"PACKING_POLL_INTERVAL", "30s", &cfg.PollInterval},
	}
	for _, d := range durations {
		v, err := time.ParseDuration(getEnv(d.key, d.fallback))
		if err != nil {
			return Packing{}, fmt.Errorf("%s must be a duration such as 1h", d.key)
		}
		*d.target = v
	}
	ratio, err := strconv.ParseFloat(getEnv("PACKING_REWRITE_DEAD_RATIO", "0.5"), 64)
	if err != nil {
		return Packing{}, fmt.Errorf("PACKING_REWRITE_DEAD_RATIO must be a number")
	}
	cfg.RewriteDeadRatio = ratio
	return cfg, cfg.Validate()
}

// Validate checks the policy's limits against each other.
func (c Packing) Validate() error {
	switch {
	case c.MinObjectBytes < 1 || c.MaxObjectBytes < c.MinObjectBytes:
		return fmt.Errorf("PACKING_MIN_OBJECT_BYTES must be at least 1 and at most PACKING_MAX_OBJECT_BYTES")
	case c.MaxObjectBytes > 64<<20:
		return fmt.Errorf("PACKING_MAX_OBJECT_BYTES must be at most 64 MiB: each source is held in memory while packed")
	case c.SourceConcurrency < 1 || c.SourceConcurrency > 256:
		return fmt.Errorf("PACKING_SOURCE_CONCURRENCY must be between 1 and 256")
	case int64(c.SourceConcurrency)*c.MaxObjectBytes > 1<<30:
		return fmt.Errorf("PACKING_SOURCE_CONCURRENCY × PACKING_MAX_OBJECT_BYTES must be at most 1 GiB per pack writer")
	case c.TargetPackBytes < c.MaxObjectBytes || c.MaxPackBytes < c.TargetPackBytes:
		return fmt.Errorf("PACKING_TARGET_PACK_BYTES must be at least PACKING_MAX_OBJECT_BYTES and at most PACKING_MAX_PACK_BYTES")
	case c.MaxPackBytes > 5<<30:
		return fmt.Errorf("PACKING_MAX_PACK_BYTES must be at most 5 GiB")
	case c.MaxEntries < 2 || c.MaxEntries > 100_000:
		return fmt.Errorf("PACKING_MAX_ENTRIES must be between 2 and 100000")
	case c.ActiveWriters < 1 || c.TenantActivePacks < 1 || c.TenantsPerCycle < 1:
		return fmt.Errorf("PACKING_ACTIVE_WRITERS, PACKING_TENANT_ACTIVE_PACKS and PACKING_TENANTS_PER_CYCLE must be at least 1")
	case c.MinAge < 0:
		return fmt.Errorf("PACKING_MIN_AGE must not be negative")
	case c.Lease < 3*time.Second || c.MaxJobDuration < c.Lease:
		return fmt.Errorf("PACKING_LEASE must be at least 3s and PACKING_MAX_JOB_DURATION at least the lease")
	case c.MaxAttempts < 1 || c.RetryBase <= 0 || c.RetryMax < c.RetryBase:
		return fmt.Errorf("PACKING_MAX_ATTEMPTS must be at least 1, and PACKING_RETRY_MAX at least PACKING_RETRY_BASE")
	case c.SourceRetireAfter < 0 || c.MaxDeletionDelay <= 0 || c.RemoveAfter < 0 || c.PollInterval <= 0:
		return fmt.Errorf("PACKING_SOURCE_RETIRE_AFTER and PACKING_REMOVE_AFTER must not be negative; PACKING_MAX_DELETION_DELAY and PACKING_POLL_INTERVAL must be positive")
	case c.RewriteDeadRatio <= 0 || c.RewriteDeadRatio > 1:
		return fmt.Errorf("PACKING_REWRITE_DEAD_RATIO must be above 0 and at most 1")
	}
	return nil
}
