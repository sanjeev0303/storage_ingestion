package config

import (
	"strings"
	"testing"
	"time"
)

func TestSecurityDefaultsAreValid(t *testing.T) {
	s, err := LoadSecurity()
	if err != nil {
		t.Fatal(err)
	}
	if s.Auth.Mode != "token" || s.Auth.AccessTokenTTL != 15*time.Minute || s.Auth.Argon2Memory < 19*1024 {
		t.Fatalf("unexpected defaults %+v", s.Auth)
	}
	if s.Rates["refresh"].Requests != 30 || s.Rates["refresh"].Window != time.Minute {
		t.Fatalf("refresh rate %+v", s.Rates["refresh"])
	}
	if s.Quotas.MaxActiveUploads != 200 || s.Quotas.MaxObjectBytes > s.Quotas.MaxStorageBytes {
		t.Fatalf("quota defaults %+v", s.Quotas)
	}
}

// Any invalid security setting stops startup: there is no fallback to a
// weaker value, and no development identity to fall back to.
func TestInvalidSecuritySettingsFailStartup(t *testing.T) {
	for _, c := range []struct {
		key, value, want string
	}{
		{"AUTH_MODE", "development", "AUTH_MODE"},
		{"AUTH_MODE", "header", "AUTH_MODE"},
		{"AUTH_ACCESS_TOKEN_TTL", "10s", "AUTH_ACCESS_TOKEN_TTL"},
		{"AUTH_ACCESS_TOKEN_TTL", "forever", "AUTH_ACCESS_TOKEN_TTL"},
		{"AUTH_REFRESH_TOKEN_TTL", "5m", "AUTH_REFRESH_TOKEN_TTL"},
		{"AUTH_PASSWORD_MIN_LENGTH", "4", "AUTH_PASSWORD_MIN_LENGTH"},
		{"AUTH_ARGON2_MEMORY_KIB", "1024", "AUTH_ARGON2_MEMORY_KIB"},
		{"AUTH_ARGON2_TIME", "1", "AUTH_ARGON2_TIME"},
		{"AUTH_ARGON2_THREADS", "0", "AUTH_ARGON2_THREADS"},
		{"AUTH_RESET_TTL", "48h", "AUTH_RESET_TTL"},
		{"AUTH_LOGIN_ATTEMPTS", "0", "AUTH_LOGIN_ATTEMPTS"},
		{"AUTH_LOGIN_ATTEMPTS", "500", "AUTH_LOGIN_ATTEMPTS"},
		{"AUDIT_RETENTION", "1h", "AUDIT_RETENTION"},
		{"RATE_LIMIT_REFRESH", "lots", "RATE_LIMIT_REFRESH"},
		{"RATE_LIMIT_ADMIN", "10/1ms", "RATE_LIMIT_ADMIN"},
		{"RATE_LIMIT_LISTING", "0/1m", "RATE_LIMIT_LISTING"},
		{"QUOTA_MAX_STORAGE_BYTES", "-1", "QUOTA_MAX_STORAGE_BYTES"},
		{"QUOTA_MAX_ACTIVE_MULTIPART", "500", "QUOTA_MAX_ACTIVE_MULTIPART"},
		{"QUOTA_MAX_OBJECT_BYTES", "2199023255552", "QUOTA_MAX_OBJECT_BYTES"},
		{"INGESTION_TENANT_STORAGE_QUOTA", "1024", "QUOTA_MAX_STORAGE_BYTES"},
	} {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			t.Setenv(c.key, c.value)
			_, err := LoadSecurity()
			if err == nil {
				t.Fatalf("%s=%s must stop startup", c.key, c.value)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the error must name %s: %v", c.want, err)
			}
		})
	}
}

func TestSecurityValidateNeedsEveryRateLimit(t *testing.T) {
	s, err := LoadSecurity()
	if err != nil {
		t.Fatal(err)
	}
	delete(s.Rates, "credentials")
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("a missing rate limit must be refused: %v", err)
	}
}
