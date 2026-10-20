package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Security holds authentication, rate-limit, audit and quota settings.
// Invalid values stop the process at startup: there is no fallback to a
// weaker setting or to a development identity.
type Security struct {
	Auth   Auth
	Rates  map[string]RateRule
	Audit  Audit
	Quotas model.TenantQuota
}

// Auth configures sign-in and tokens.
type Auth struct {
	// Mode is "token": opaque bearer tokens backed by revocable sessions.
	// It is the only mode.
	Mode string
	// AccessTokenTTL is an access token's lifetime; RefreshTokenTTL the
	// session's, which refreshing does not extend.
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration

	PasswordMinLength int
	// Argon2id parameters, in KiB of memory, passes and lanes.
	Argon2Memory  uint32
	Argon2Time    uint32
	Argon2Threads uint8

	InvitationTTL time.Duration
	ActivationTTL time.Duration
	ResetTTL      time.Duration

	// Failed sign-ins allowed per email and source address, and per source
	// address alone, in LoginWindow.
	LoginAttempts   int
	LoginIPAttempts int
	LoginWindow     time.Duration

	// TrustProxyHeaders takes the client address from X-Forwarded-For.
	// Only behind a proxy that sets it; otherwise clients could pick their
	// own address.
	TrustProxyHeaders bool
}

// RateRule allows Requests per Window.
type RateRule struct {
	Requests int
	Window   time.Duration
}

// Audit configures the audit log.
type Audit struct {
	// Retention is how long audit events are kept before the purge may
	// remove them.
	Retention time.Duration
}

// Rate-limited operations and their default rules.
var defaultRates = map[string]string{
	"refresh":       "30/1m",
	"credentials":   "10/15m",
	"upload_init":   "300/1m",
	"upload_finish": "600/1m",
	"manifest":      "60/1m",
	"job_create":    "60/1m",
	"download_url":  "600/1m",
	"listing":       "600/1m",
	"admin":         "120/1m",
	"membership":    "120/1m",
	// Connection tests and credential rotations reach out to customer
	// endpoints with customer credentials: kept low, so a test cannot be
	// used to probe a network.
	"storage_test":  "10/1m",
	"storage_admin": "60/1m",
}

// LoadSecurity reads and validates the security settings.
func LoadSecurity() (Security, error) {
	var s Security
	var err error

	s.Auth.Mode = getEnv("AUTH_MODE", "token")
	durations := []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"AUTH_ACCESS_TOKEN_TTL", "15m", &s.Auth.AccessTokenTTL},
		{"AUTH_REFRESH_TOKEN_TTL", "720h", &s.Auth.RefreshTokenTTL},
		{"AUTH_INVITATION_TTL", "168h", &s.Auth.InvitationTTL},
		{"AUTH_ACTIVATION_TTL", "72h", &s.Auth.ActivationTTL},
		{"AUTH_RESET_TTL", "1h", &s.Auth.ResetTTL},
		{"AUTH_LOGIN_WINDOW", "15m", &s.Auth.LoginWindow},
		{"AUDIT_RETENTION", "17520h", &s.Audit.Retention},
	}
	for _, d := range durations {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Security{}, fmt.Errorf("%s must be a positive duration such as 15m", d.key)
		}
	}

	ints := []struct {
		key, fallback string
		target        *int
	}{
		{"AUTH_PASSWORD_MIN_LENGTH", "12", &s.Auth.PasswordMinLength},
		{"AUTH_LOGIN_ATTEMPTS", "5", &s.Auth.LoginAttempts},
		{"AUTH_LOGIN_IP_ATTEMPTS", "50", &s.Auth.LoginIPAttempts},
	}
	for _, n := range ints {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Security{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}

	memory, err := strconv.ParseUint(getEnv("AUTH_ARGON2_MEMORY_KIB", "65536"), 10, 32)
	if err != nil {
		return Security{}, fmt.Errorf("AUTH_ARGON2_MEMORY_KIB must be a number of KiB")
	}
	passes, err := strconv.ParseUint(getEnv("AUTH_ARGON2_TIME", "3"), 10, 32)
	if err != nil {
		return Security{}, fmt.Errorf("AUTH_ARGON2_TIME must be a number of passes")
	}
	threads, err := strconv.ParseUint(getEnv("AUTH_ARGON2_THREADS", "2"), 10, 8)
	if err != nil {
		return Security{}, fmt.Errorf("AUTH_ARGON2_THREADS must be a number of lanes")
	}
	s.Auth.Argon2Memory, s.Auth.Argon2Time, s.Auth.Argon2Threads = uint32(memory), uint32(passes), uint8(threads)
	s.Auth.TrustProxyHeaders = strings.EqualFold(getEnv("AUTH_TRUST_PROXY_HEADERS", "false"), "true")

	s.Rates = map[string]RateRule{}
	for op, fallback := range defaultRates {
		key := "RATE_LIMIT_" + strings.ToUpper(op)
		rule, err := parseRate(getEnv(key, fallback))
		if err != nil {
			return Security{}, fmt.Errorf("%s: %w", key, err)
		}
		s.Rates[op] = rule
	}

	if s.Quotas, err = loadQuotaDefaults(); err != nil {
		return Security{}, err
	}
	if err := s.Validate(); err != nil {
		return Security{}, err
	}
	return s, nil
}

// parseRate reads "requests/window", such as 120/1m.
func parseRate(s string) (RateRule, error) {
	count, window, ok := strings.Cut(s, "/")
	if !ok {
		return RateRule{}, fmt.Errorf("want requests/window, such as 120/1m")
	}
	n, err := strconv.Atoi(count)
	if err != nil || n <= 0 {
		return RateRule{}, fmt.Errorf("the request count must be a positive integer")
	}
	d, err := time.ParseDuration(window)
	if err != nil || d < time.Second {
		return RateRule{}, fmt.Errorf("the window must be a duration of at least 1s")
	}
	return RateRule{Requests: n, Window: d}, nil
}

// renamedQuotas are Phase 7 settings that became tenant quotas. A value
// left under an old name would be ignored, so it stops startup instead.
var renamedQuotas = map[string]string{
	"INGESTION_TENANT_STORAGE_QUOTA": "QUOTA_MAX_STORAGE_BYTES",
	"INGESTION_MAX_ACTIVE_JOBS":      "QUOTA_MAX_ACTIVE_JOBS",
	"INGESTION_MAX_PENDING_ENTRIES":  "QUOTA_MAX_PENDING_ENTRIES",
}

// loadQuotaDefaults reads the default per-tenant limits. Tenants'
// overrides in tenant_quotas take precedence.
func loadQuotaDefaults() (model.TenantQuota, error) {
	var q model.TenantQuota
	for old, now := range renamedQuotas {
		if getEnv(old, "") != "" {
			return q, fmt.Errorf("%s was renamed to %s", old, now)
		}
	}
	sizes := []struct {
		key, fallback string
		target        *int64
	}{
		{"QUOTA_MAX_STORAGE_BYTES", "1099511627776", &q.MaxStorageBytes},
		{"QUOTA_MAX_ASSETS", "10000000", &q.MaxAssets},
		{"QUOTA_MAX_OBJECT_BYTES", "53687091200", &q.MaxObjectBytes},
		{"QUOTA_MAX_PENDING_ENTRIES", "10000000", &q.MaxPendingEntries},
		{"QUOTA_MAX_MANIFEST_ENTRIES", "5000000", &q.MaxManifestEntries},
		{"QUOTA_MAX_MANIFEST_BYTES", "2147483648", &q.MaxManifestBytes},
	}
	for _, s := range sizes {
		v, err := strconv.ParseInt(getEnv(s.key, s.fallback), 10, 64)
		if err != nil || v <= 0 {
			return q, fmt.Errorf("%s must be a positive integer", s.key)
		}
		*s.target = v
	}
	ints := []struct {
		key, fallback string
		target        *int
	}{
		{"QUOTA_MAX_ACTIVE_UPLOADS", "200", &q.MaxActiveUploads},
		{"QUOTA_MAX_ACTIVE_MULTIPART", "50", &q.MaxActiveMultipart},
		{"QUOTA_MAX_ACTIVE_JOBS", "10", &q.MaxActiveJobs},
		{"QUOTA_MAX_PENDING_PROCESSING", "5000", &q.MaxPendingProcessing},
		{"QUOTA_MAX_REQUESTS_PER_MINUTE", "6000", &q.MaxRequestsPerMinute},
	}
	for _, n := range ints {
		v, err := strconv.Atoi(getEnv(n.key, n.fallback))
		if err != nil || v <= 0 {
			return q, fmt.Errorf("%s must be a positive integer", n.key)
		}
		*n.target = v
	}
	return q, nil
}

// Validate checks the settings fit together and are strong enough.
func (s Security) Validate() error {
	a := s.Auth
	switch {
	case a.Mode != "token":
		// The development header identity no longer exists; nothing
		// unauthenticated may be configured in its place.
		return fmt.Errorf("AUTH_MODE must be token")
	case a.AccessTokenTTL < time.Minute || a.AccessTokenTTL > 24*time.Hour:
		return fmt.Errorf("AUTH_ACCESS_TOKEN_TTL must be between 1m and 24h")
	case a.RefreshTokenTTL <= a.AccessTokenTTL || a.RefreshTokenTTL > 90*24*time.Hour:
		return fmt.Errorf("AUTH_REFRESH_TOKEN_TTL must be longer than the access token TTL and at most 2160h")
	case a.PasswordMinLength < 8 || a.PasswordMinLength > 128:
		return fmt.Errorf("AUTH_PASSWORD_MIN_LENGTH must be between 8 and 128")
	case a.Argon2Memory < 19*1024:
		// OWASP's minimum for Argon2id is 19 MiB with 2 passes.
		return fmt.Errorf("AUTH_ARGON2_MEMORY_KIB must be at least 19456")
	case a.Argon2Time < 2:
		return fmt.Errorf("AUTH_ARGON2_TIME must be at least 2")
	case a.Argon2Threads < 1:
		return fmt.Errorf("AUTH_ARGON2_THREADS must be at least 1")
	case a.ResetTTL > 24*time.Hour:
		return fmt.Errorf("AUTH_RESET_TTL must be at most 24h")
	case a.InvitationTTL > 30*24*time.Hour || a.ActivationTTL > 30*24*time.Hour:
		return fmt.Errorf("AUTH_INVITATION_TTL and AUTH_ACTIVATION_TTL must be at most 720h")
	case a.LoginAttempts > a.LoginIPAttempts:
		return fmt.Errorf("AUTH_LOGIN_ATTEMPTS must not exceed AUTH_LOGIN_IP_ATTEMPTS")
	case s.Audit.Retention < 24*time.Hour:
		return fmt.Errorf("AUDIT_RETENTION must be at least 24h")
	case s.Quotas.MaxActiveMultipart > s.Quotas.MaxActiveUploads:
		return fmt.Errorf("QUOTA_MAX_ACTIVE_MULTIPART must not exceed QUOTA_MAX_ACTIVE_UPLOADS")
	case s.Quotas.MaxObjectBytes > s.Quotas.MaxStorageBytes:
		return fmt.Errorf("QUOTA_MAX_OBJECT_BYTES must not exceed QUOTA_MAX_STORAGE_BYTES")
	}
	for op := range defaultRates {
		if _, ok := s.Rates[op]; !ok {
			return fmt.Errorf("no rate limit for %s", op)
		}
	}
	return nil
}
