package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
	memoryrepo "github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

type harness struct {
	repo     *memoryrepo.StorageConnectionRepository
	keyring  *secretbox.Keyring
	registry *Registry
	clock    time.Time
	builds   int
	secrets  []model.StorageSecrets
	closed   int
	metrics  *metrics.Registry
}

func newHarness(t *testing.T, mutate func(*config.Storage)) *harness {
	t.Helper()
	keyring, err := secretbox.NewKeyring(map[int][]byte{1: bytes.Repeat([]byte{7}, 32)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{repo: memoryrepo.NewStorageConnectionRepository(), keyring: keyring, clock: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), metrics: metrics.New()}
	cfg := config.Storage{BYOSEnabled: true, ClientCacheSize: 8, ClientCacheTTL: 15 * time.Minute,
		ConnectionCacheTTL: 10 * time.Second, OperationTimeout: time.Minute, MaxConcurrentPerConnection: 4}
	if mutate != nil {
		mutate(&cfg)
	}
	var mu sync.Mutex
	h.registry, err = New(Dependencies{
		Config:      cfg,
		Platform:    interfaces.StorageLocation{StoragePlace: interfaces.StoragePlace{ConnectionID: model.PlatformConnectionID, StagingBucket: "q", StagingPrefix: "quarantine", ReadyBucket: "d", ReadyPrefix: "assets"}, Storage: memory.NewObjectStorage()},
		Connections: h.repo,
		Sealer:      keyring,
		Metrics:     h.metrics,
		Now:         func() time.Time { return h.clock },
		Build: func(_ context.Context, c model.StorageConnection, secrets model.StorageSecrets) (interfaces.StorageProvider, func(), error) {
			mu.Lock()
			defer mu.Unlock()
			h.builds++
			h.secrets = append(h.secrets, secrets)
			return memory.NewObjectStorage(), func() { h.closed++ }, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// connection stores an active tenant connection with sealed credentials.
func (h *harness) connection(t *testing.T, tenant, id string, secrets model.StorageSecrets) model.StorageConnection {
	t.Helper()
	c := model.StorageConnection{ID: id, TenantID: tenant, Name: id, Provider: model.ProviderS3Compatible,
		Status: model.ConnectionActive, AuthMethod: model.AuthAccessKey, Bucket: "customer-bucket",
		Endpoint: "https://s3.example.com", StagingPrefix: "staging", ReadyPrefix: "published", StagingMode: model.StagingInPlace,
		Capabilities: &model.ProviderCapabilities{PresignedUpload: true, Multipart: true}}
	h.repo.Put(c, h.seal(t, c, 1, secrets))
	stored, err := h.repo.GetConnection(context.Background(), tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func (h *harness) seal(t *testing.T, c model.StorageConnection, version int, secrets model.StorageSecrets) interfaces.SealedCredential {
	t.Helper()
	plain, _ := json.Marshal(map[string]string{"AccessKeyID": secrets.AccessKeyID, "SecretAccessKey": secrets.SecretAccessKey})
	sealed, err := h.keyring.Seal(plain, c.CredentialAAD(version))
	if err != nil {
		t.Fatal(err)
	}
	return interfaces.SealedCredential{Version: version, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}
}

var keys = model.StorageSecrets{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI-secret"}

func TestLocateResolvesOnlyTheTenantsOwnConnections(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.connection(t, "t1", "sc_one", keys)

	if loc, err := h.registry.Locate(ctx, "t2", "platform"); err != nil || loc.ConnectionID != model.PlatformConnectionID {
		t.Fatalf("platform: %+v %v", loc, err)
	}
	// Every stored record names its binding, so an empty one is lost, not
	// the platform's: it must not quietly reach the platform bucket.
	if loc, err := h.registry.Locate(ctx, "t1", ""); !errors.Is(err, model.ErrConnectionNotFound) || loc.Storage != nil {
		t.Fatalf("an unset binding resolved: %+v %v", loc, err)
	}
	loc, err := h.registry.Locate(ctx, "t1", "sc_one")
	if err != nil || loc.ReadyBucket != "customer-bucket" || loc.StagingKey("t1", "u1") != "staging/t1/u1" {
		t.Fatalf("own connection: %+v %v", loc, err)
	}
	if h.secrets[0].SecretAccessKey != keys.SecretAccessKey {
		t.Fatal("the client must be built from the decrypted credentials")
	}
	// Another tenant naming the same ID gets nothing, not even a hint.
	if _, err := h.registry.Locate(ctx, "t2", "sc_one"); !errors.Is(err, model.ErrConnectionNotFound) {
		t.Fatalf("cross-tenant locate must be not found, got %v", err)
	}
}

func TestDisabledConnectionsDrainButPendingOnesDoNotResolve(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.connection(t, "t1", "sc_one", keys)

	c.Status = model.ConnectionDisabled
	h.repo.Put(c, interfaces.SealedCredential{})
	h.registry.Invalidate(c.ID)
	if _, err := h.registry.Locate(ctx, "t1", c.ID); err != nil {
		t.Fatalf("existing work still reaches a disabled connection: %v", err)
	}

	c.Status = model.ConnectionPending
	h.repo.Put(c, interfaces.SealedCredential{})
	h.registry.Invalidate(c.ID)
	if _, err := h.registry.Locate(ctx, "t1", c.ID); !errors.Is(err, model.ErrConnectionNotActive) {
		t.Fatalf("a pending connection holds no work: %v", err)
	}
}

func TestClientsAreCachedAndRebuiltOnRotation(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.connection(t, "t1", "sc_one", keys)

	for range 3 {
		if _, err := h.registry.Locate(ctx, "t1", c.ID); err != nil {
			t.Fatal(err)
		}
	}
	if h.builds != 1 {
		t.Fatalf("one client for repeated use, built %d", h.builds)
	}

	// Rotation in this process: the next use builds from the new secrets.
	rotated := model.StorageSecrets{AccessKeyID: "AKIDNEW", SecretAccessKey: "new-secret"}
	h.repo.Put(c, h.seal(t, c, 2, rotated))
	h.registry.Invalidate(c.ID)
	if _, err := h.registry.Locate(ctx, "t1", c.ID); err != nil {
		t.Fatal(err)
	}
	if h.builds != 2 || h.secrets[1].AccessKeyID != "AKIDNEW" || h.closed != 1 {
		t.Fatalf("rotation must rebuild and close the old client: builds %d closed %d", h.builds, h.closed)
	}

	// Rotation in another process: noticed once the row cache expires,
	// because the credential version is part of the client's key.
	h.repo.Put(c, h.seal(t, c, 3, keys))
	if _, err := h.registry.Locate(ctx, "t1", c.ID); err != nil || h.builds != 2 {
		t.Fatalf("within the row TTL the cached row is used: builds %d %v", h.builds, err)
	}
	h.clock = h.clock.Add(11 * time.Second)
	if _, err := h.registry.Locate(ctx, "t1", c.ID); err != nil || h.builds != 3 {
		t.Fatalf("after the row TTL the new version is built: builds %d %v", h.builds, err)
	}

	// A client never outlives its TTL, revoked credentials or not.
	h.clock = h.clock.Add(16 * time.Minute)
	if _, err := h.registry.Locate(ctx, "t1", c.ID); err != nil || h.builds != 4 {
		t.Fatalf("an expired client is rebuilt: builds %d %v", h.builds, err)
	}
}

// Requests keep resolving and using a connection while its credentials
// rotate: none fails, and the rotation is what later requests are built
// from. Meant for -race.
func TestRotationDuringConcurrentUseIsSafe(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	c := h.connection(t, "t1", "sc_one", keys)
	rotated := model.StorageSecrets{AccessKeyID: "AKIDNEW", SecretAccessKey: "new-secret"}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				loc, err := h.registry.Locate(ctx, "t1", c.ID)
				if err == nil {
					_, err = loc.Storage.HeadObject(ctx, loc.ReadyBucket, "k")
				}
				if err != nil && !errors.Is(err, interfaces.ErrObjectNotFound) {
					errs <- err
					return
				}
			}
		}()
	}
	for v := 2; v <= 6; v++ {
		h.repo.Put(c, h.seal(t, c, v, rotated))
		h.registry.Invalidate(c.ID)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a request failed during rotation: %v", err)
	}
	h.registry.Invalidate(c.ID)
	if _, err := h.registry.Locate(ctx, "t1", c.ID); err != nil || h.secrets[len(h.secrets)-1].AccessKeyID != "AKIDNEW" {
		t.Fatalf("after rotation the client is built from the new secrets: %v", err)
	}
}

func TestClientCacheIsBounded(t *testing.T) {
	h := newHarness(t, func(c *config.Storage) { c.ClientCacheSize = 2 })
	ctx := context.Background()
	for _, id := range []string{"sc_a", "sc_b", "sc_c"} {
		h.connection(t, "t1", id, keys)
		if _, err := h.registry.Locate(ctx, "t1", id); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.registry.clients) != 2 || h.closed != 1 {
		t.Fatalf("the least recently used client is evicted: %d cached, %d closed", len(h.registry.clients), h.closed)
	}
	if _, ok := h.registry.clients["sc_a"]; ok {
		t.Fatal("sc_a was the least recently used")
	}
}

func TestCredentialsSealedForAnotherConnectionDoNotOpen(t *testing.T) {
	h := newHarness(t, nil)
	victim := h.connection(t, "t1", "sc_victim", keys)
	thief := h.connection(t, "t2", "sc_thief", model.StorageSecrets{AccessKeyID: "x", SecretAccessKey: "y"})
	// Copy the victim's ciphertext onto the thief's row: the associated
	// data names tenant and connection, so it must not decrypt there.
	h.repo.Put(thief, h.seal(t, victim, 1, keys))

	_, err := h.registry.Locate(context.Background(), "t2", thief.ID)
	if !errors.Is(err, model.ErrCredentialMissing) {
		t.Fatalf("a moved ciphertext must not open, got %v", err)
	}
	if strings.Contains(err.Error(), keys.SecretAccessKey) {
		t.Fatal("the error must not carry the secret")
	}
}

func TestWithoutBYOSOnlyThePlatformResolves(t *testing.T) {
	h := newHarness(t, nil)
	h.registry.deps.Sealer = nil
	h.connection(t, "t1", "sc_one", keys)
	if _, err := h.registry.Locate(context.Background(), "t1", "sc_one"); !errors.Is(err, model.ErrStorageNotConfigured) {
		t.Fatalf("no silent fallback to the platform: %v", err)
	}
}

func TestChooseForUpload(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.connection(t, "t1", "sc_own", keys)
	staged := h.connection(t, "t1", "sc_staged", keys)
	staged.StagingMode = model.StagingPlatform
	h.repo.Put(staged, interfaces.SealedCredential{})

	developer := model.Principal{TenantID: "t1", UserID: "u1", Permissions: model.PermUploadsWrite | model.PermStorageUse}
	reader := model.Principal{TenantID: "t1", UserID: "u2", Permissions: model.PermUploadsWrite}

	choice, err := h.registry.ChooseForUpload(ctx, reader, "", "")
	if err != nil || choice.Final.ConnectionID != model.PlatformConnectionID {
		t.Fatalf("the initial default is the platform: %+v %v", choice, err)
	}
	if _, err := h.registry.ChooseForUpload(ctx, reader, "sc_own", ""); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("choosing a non-default connection needs storage.use: %v", err)
	}
	if choice, err := h.registry.ChooseForUpload(ctx, developer, "sc_own", ""); err != nil || choice.Staging.ConnectionID != "sc_own" {
		t.Fatalf("explicit choice: %+v %v", choice, err)
	}
	if _, err := h.registry.ChooseForUpload(ctx, developer, "sc_other_tenant", ""); !errors.Is(err, model.ErrConnectionNotFound) {
		t.Fatalf("unknown connection: %v", err)
	}

	// The dataset's choice, then the organization's default.
	h.repo.SetDatasetConnection("t1", "images", "sc_staged")
	choice, err = h.registry.ChooseForUpload(ctx, reader, "", "images")
	if err != nil || choice.Final.ConnectionID != "sc_staged" || choice.Staging.ConnectionID != model.PlatformConnectionID {
		t.Fatalf("a PLATFORM-staged connection lands uploads on the platform: %+v %v", choice, err)
	}
	if err := h.repo.SetDefault(ctx, "t1", "sc_own"); err != nil {
		t.Fatal(err)
	}
	if choice, err := h.registry.ChooseForUpload(ctx, reader, "", ""); err != nil || choice.Final.ConnectionID != "sc_own" {
		t.Fatalf("the organization default: %+v %v", choice, err)
	}

	// A disabled connection takes no new upload, even from a fresh cache.
	own, _ := h.repo.GetConnection(ctx, "t1", "sc_own")
	own.Status = model.ConnectionDisabled
	h.repo.Put(own, interfaces.SealedCredential{})
	if _, err := h.registry.ChooseForUpload(ctx, developer, "sc_own", ""); !errors.Is(err, model.ErrConnectionNotActive) {
		t.Fatalf("disabled connections take no new work: %v", err)
	}
}

func TestEndpointPolicy(t *testing.T) {
	strict := netguard.DefaultPolicy()
	local := netguard.DefaultPolicy()
	local.AllowPrivate = true
	conn := func(p model.StorageProviderType, endpoint, account string) model.StorageConnection {
		return model.StorageConnection{Provider: p, Endpoint: endpoint, AccountID: account}
	}
	cases := []struct {
		name   string
		c      model.StorageConnection
		policy netguard.Policy
		want   string // "" for rejected
	}{
		{"aws default", conn(model.ProviderAWSS3, "", ""), strict, ""},
		{"r2 account endpoint", conn(model.ProviderR2, "", "abc123"), strict, "https://abc123.r2.cloudflarestorage.com"},
		{"azure account endpoint", conn(model.ProviderAzureBlob, "", "acct"), strict, "https://acct.blob.core.windows.net"},
		{"generic https", conn(model.ProviderS3Compatible, "https://s3.example.com", ""), strict, "https://s3.example.com"},
	}
	for _, tc := range cases {
		got, err := Endpoint(tc.c, tc.policy)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q %v, want %q", tc.name, got, err, tc.want)
		}
	}

	rejected := []struct {
		name   string
		c      model.StorageConnection
		policy netguard.Policy
	}{
		{"generic needs an endpoint", conn(model.ProviderS3Compatible, "", ""), strict},
		{"plain http", conn(model.ProviderS3Compatible, "http://s3.example.com", ""), strict},
		{"credentials in the URL", conn(model.ProviderS3Compatible, "https://user:pass@s3.example.com", ""), strict},
		{"a path", conn(model.ProviderS3Compatible, "https://s3.example.com/bucket", ""), strict},
		{"cloud metadata", conn(model.ProviderS3Compatible, "https://169.254.169.254", ""), strict},
		{"loopback", conn(model.ProviderS3Compatible, "https://127.0.0.1:9000", ""), strict},
		{"private network", conn(model.ProviderS3Compatible, "https://10.0.0.5", ""), strict},
		{"aws outside amazonaws.com", conn(model.ProviderAWSS3, "https://s3.evil.example", ""), strict},
		{"r2 outside cloudflare", conn(model.ProviderR2, "https://r2.evil.example", "abc"), strict},
		{"gcs outside googleapis", conn(model.ProviderGCS, "https://storage.evil.example", ""), strict},
		{"azure outside windows.net", conn(model.ProviderAzureBlob, "https://blob.evil.example", "acct"), strict},
		{"look-alike domain", conn(model.ProviderAWSS3, "https://amazonaws.com.evil.example", ""), strict},
		{"metadata even locally", conn(model.ProviderS3Compatible, "http://169.254.169.254", ""), local},
	}
	for _, tc := range rejected {
		if got, err := Endpoint(tc.c, tc.policy); !errors.Is(err, model.ErrEndpointRejected) {
			t.Errorf("%s: must be rejected, got %q %v", tc.name, got, err)
		}
	}

	// The development policy admits a local emulator, nothing more.
	if got, err := Endpoint(conn(model.ProviderS3Compatible, "http://127.0.0.1:9000", ""), local); err != nil || got != "http://127.0.0.1:9000" {
		t.Errorf("local MinIO under the development policy: %q %v", got, err)
	}
	if got, err := Endpoint(conn(model.ProviderGCS, "http://localhost:4443", ""), local); err != nil || got == "" {
		t.Errorf("local GCS emulator under the development policy: %q %v", got, err)
	}
}

// blockingStorage holds every read open until released.
type blockingStorage struct {
	*memory.ObjectStorage
}

func TestLimitedBoundsConcurrencyAndHoldsSlotsForStreams(t *testing.T) {
	inner := memory.NewObjectStorage()
	inner.PutObject("k", []byte("data"))
	m := metrics.New()
	l := newLimited(blockingStorage{inner}, model.ProviderS3Compatible, 2, time.Minute, m)
	ctx := context.Background()

	first, err := l.OpenObject(ctx, "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.OpenObject(ctx, "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	// Both slots are held by open streams: a third call waits, and gives
	// up with its context.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := l.HeadObject(short, "b", "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a full connection must make callers wait: %v", err)
	}
	_, _ = io.ReadAll(first)
	first.Close()
	first.Close() // closing twice releases once
	if _, err := l.HeadObject(ctx, "b", "k"); err != nil {
		t.Fatalf("a closed stream frees its slot: %v", err)
	}
	second.Close()
	if len(l.slots) != 0 {
		t.Fatalf("every slot is free again, %d held", len(l.slots))
	}

	for name := range m.Snapshot() {
		if !strings.HasPrefix(name, "storage.S3_COMPATIBLE.") && name != "storage.active_transfers" {
			t.Errorf("unexpected metric %q", name)
		}
	}
	if m.Snapshot()["storage.S3_COMPATIBLE.saturated"] != 1 {
		t.Error("a saturated connection is counted")
	}
}
