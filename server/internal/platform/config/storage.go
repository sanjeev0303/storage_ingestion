package config

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
)

// Storage configures storage connections (Phase 11): whether
// organizations may bring their own storage, the keys their credentials
// are sealed with, the network policy for their endpoints, and the
// bounds on provider clients.
type Storage struct {
	// BYOSEnabled lets organizations add their own storage. Off, only the
	// platform-managed connection exists and no key is needed.
	BYOSEnabled bool
	// CredentialKeys seal credentials, by version; CredentialKeyVersion is
	// the one new credentials are sealed with. Older versions stay for
	// opening what they sealed.
	CredentialKeys       map[int][]byte
	CredentialKeyVersion int
	// AllowPrivateEndpoints permits plain-http, loopback and private
	// endpoints, for MinIO and emulators. Development only.
	AllowPrivateEndpoints bool
	// Clients: how many provider clients are kept, and for how long one is
	// reused before it is rebuilt from the stored credentials.
	ClientCacheSize int
	ClientCacheTTL  time.Duration
	// ConnectionCacheTTL is how long a process trusts a connection row it
	// read; a disablement elsewhere takes effect within it.
	ConnectionCacheTTL time.Duration
	// OperationTimeout bounds one provider call; TestTimeout a connection
	// test.
	OperationTimeout time.Duration
	TestTimeout      time.Duration
	// MaxConcurrentPerConnection bounds provider calls in flight to one
	// connection, so one slow provider cannot hold every worker.
	MaxConcurrentPerConnection int
	// TransferPartBytes is the buffer of a streaming copy between
	// connections: the most of one object held in memory.
	TransferPartBytes int64
	// PostureInterval is how often each connection's bucket posture is
	// checked; DriftInterval how often the drift scan visits it, checking
	// up to DriftBudget objects (docs/storage-posture.md).
	PostureInterval time.Duration
	DriftInterval   time.Duration
	DriftBudget     int
}

// LoadStorage reads STORAGE_* and BYOS_ENABLED. With BYOS enabled, a
// missing, malformed or reused credential key stops the process.
func LoadStorage() (Storage, error) {
	s := Storage{
		BYOSEnabled:           strings.EqualFold(getEnv("BYOS_ENABLED", "false"), "true"),
		AllowPrivateEndpoints: strings.EqualFold(getEnv("STORAGE_ALLOW_PRIVATE_ENDPOINTS", "false"), "true"),
	}
	var err error
	for _, n := range []struct {
		key, fallback string
		target        *int
	}{
		{"STORAGE_CLIENT_CACHE_SIZE", "256", &s.ClientCacheSize},
		{"STORAGE_MAX_CONCURRENT_PER_CONNECTION", "32", &s.MaxConcurrentPerConnection},
		{"STORAGE_DRIFT_BUDGET", "2000", &s.DriftBudget},
	} {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Storage{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}
	for _, d := range []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"STORAGE_CLIENT_CACHE_TTL", "15m", &s.ClientCacheTTL},
		{"STORAGE_CONNECTION_CACHE_TTL", "10s", &s.ConnectionCacheTTL},
		{"STORAGE_OPERATION_TIMEOUT", "2m", &s.OperationTimeout},
		{"STORAGE_TEST_TIMEOUT", "20s", &s.TestTimeout},
		{"STORAGE_POSTURE_INTERVAL", "24h", &s.PostureInterval},
		{"STORAGE_DRIFT_INTERVAL", "6h", &s.DriftInterval},
	} {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Storage{}, fmt.Errorf("%s must be a positive duration", d.key)
		}
	}
	if s.TransferPartBytes, err = strconv.ParseInt(getEnv("STORAGE_TRANSFER_PART_BYTES", "16777216"), 10, 64); err != nil ||
		s.TransferPartBytes < 5<<20 || s.TransferPartBytes > 256<<20 {
		return Storage{}, fmt.Errorf("STORAGE_TRANSFER_PART_BYTES must be between 5 MiB and 256 MiB")
	}
	switch {
	case s.ClientCacheSize > 10000:
		return Storage{}, fmt.Errorf("STORAGE_CLIENT_CACHE_SIZE must be at most 10000")
	case s.ClientCacheTTL > 24*time.Hour:
		return Storage{}, fmt.Errorf("STORAGE_CLIENT_CACHE_TTL must be at most 24h: rotated credentials must not live on")
	case s.ConnectionCacheTTL > 5*time.Minute:
		return Storage{}, fmt.Errorf("STORAGE_CONNECTION_CACHE_TTL must be at most 5m")
	case s.MaxConcurrentPerConnection > 1024:
		return Storage{}, fmt.Errorf("STORAGE_MAX_CONCURRENT_PER_CONNECTION must be at most 1024")
	case s.AllowPrivateEndpoints && !strings.EqualFold(getEnv("APP_ENV", "development"), "development"):
		return Storage{}, fmt.Errorf("STORAGE_ALLOW_PRIVATE_ENDPOINTS is for APP_ENV=development only")
	}
	if !s.BYOSEnabled {
		return s, nil
	}

	spec := os.Getenv("STORAGE_CREDENTIAL_KEYS")
	if spec == "" {
		return Storage{}, fmt.Errorf("BYOS_ENABLED needs STORAGE_CREDENTIAL_KEYS (<version>:<base64 of 32 random bytes>)")
	}
	if s.CredentialKeys, err = secretbox.ParseKeys(spec); err != nil {
		return Storage{}, fmt.Errorf("STORAGE_CREDENTIAL_KEYS: %v", err)
	}
	versions := make([]int, 0, len(s.CredentialKeys))
	for v := range s.CredentialKeys {
		versions = append(versions, v)
	}
	s.CredentialKeyVersion = slices.Max(versions)
	if raw := getEnv("STORAGE_CREDENTIAL_KEY_VERSION", ""); raw != "" {
		if s.CredentialKeyVersion, err = strconv.Atoi(raw); err != nil {
			return Storage{}, fmt.Errorf("STORAGE_CREDENTIAL_KEY_VERSION must be a key version")
		}
	}
	if _, err := secretbox.NewKeyring(s.CredentialKeys, s.CredentialKeyVersion); err != nil {
		return Storage{}, fmt.Errorf("STORAGE_CREDENTIAL_KEYS: %v", err)
	}
	// A dedicated key: never one of the deployment's other secrets.
	for _, name := range []string{"POSTGRES_PASSWORD", "DATABASE_URL", "S3_SECRET_ACCESS_KEY", "SQS_SECRET_ACCESS_KEY"} {
		other := os.Getenv(name)
		if other == "" {
			continue
		}
		for _, key := range s.CredentialKeys {
			// A short value turns up inside a random key's base64 by chance
			// ("x" often does), which says nothing about reuse.
			if bytes.Equal(key, []byte(other)) || (len(other) >= 16 && strings.Contains(spec, other)) {
				return Storage{}, fmt.Errorf("STORAGE_CREDENTIAL_KEYS must not reuse %s", name)
			}
		}
	}
	return s, nil
}

// Keyring builds the credential keyring.
func (s Storage) Keyring() (*secretbox.Keyring, error) {
	return secretbox.NewKeyring(s.CredentialKeys, s.CredentialKeyVersion)
}
