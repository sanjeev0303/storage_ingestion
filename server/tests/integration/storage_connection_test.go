package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/registry"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
	"github.com/sanjeev0303/object-storage-platform/internal/server"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
	"github.com/sanjeev0303/object-storage-platform/internal/service/copyengine"
	"github.com/sanjeev0303/object-storage-platform/internal/service/storageconn"
)

// Phase 11: storage connections against PostgreSQL, and a tenant's own
// bucket end to end, with the compose MinIO as an S3-compatible provider.

func testKeyring(t *testing.T) *secretbox.Keyring {
	t.Helper()
	keyring, err := secretbox.NewKeyring(map[int][]byte{1: bytes.Repeat([]byte{0x5a}, 32)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

// activeConnection stores a tested connection for tenantID.
// activeConnection makes an S3-compatible connection, or one of provider
// when given.
func activeConnection(t *testing.T, repo *postgres.StorageConnectionRepository, tenantID, name string, keyring *secretbox.Keyring, provider ...model.StorageProviderType) model.StorageConnection {
	t.Helper()
	ctx := context.Background()
	c := model.StorageConnection{
		ID: "sc_" + fmt.Sprint(time.Now().UnixNano()), TenantID: tenantID, Name: name, Provider: model.ProviderS3Compatible,
		AuthMethod: model.AuthAccessKey, Bucket: "tenant-bucket", Endpoint: "https://s3.example.com",
		StagingPrefix: "staging", ReadyPrefix: "published", StagingMode: model.StagingInPlace,
		CreatedBy: "it", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if len(provider) > 0 {
		c.Provider, c.Endpoint, c.Region = provider[0], "", "ap-south-1"
	}
	plain := []byte(`{"AccessKeyID":"AKIDIT","SecretAccessKey":"it-secret-value"}`)
	sealed, err := keyring.Seal(plain, c.CredentialAAD(1))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.CreateConnection(ctx, c, interfaces.SealedCredential{Version: 1, KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.ConnectionPending {
		t.Fatalf("a new connection is pending: %s", stored.Status)
	}
	stored, err = repo.RecordTest(ctx, tenantID, c.ID, interfaces.ConnectionTestRecord{OK: true, At: time.Now().UTC(),
		Capabilities: &model.ProviderCapabilities{PresignedUpload: true, Multipart: true}})
	if err != nil || stored.Status != model.ConnectionActive {
		t.Fatalf("a passing test activates: %+v %v", stored, err)
	}
	return stored
}

func boundSession(tenantID, id, connectionID string, status model.UploadStatus) *model.UploadSession {
	now := time.Now().UTC()
	return &model.UploadSession{
		ID: id, TenantID: tenantID, OwnerID: "u1", ObjectKey: "staging/" + tenantID + "/" + id, FileName: "f.txt",
		ContentType: "text/plain", ExpectedSize: 4, ChecksumSHA256: strings.Repeat("a", 64), Status: status,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		StorageConnectionID: connectionID, StagingConnectionID: connectionID,
	}
}

// A completed upload is still work until its asset exists, so its
// connection cannot be deleted under it. And an upload already stranded on a
// deleted connection never stops processing for anyone else.
func TestConnectionDeletionWaitsForUnprocessedUploads(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	owner, _ := e.account(t, "conn-drain")
	org := e.organization(t, "conn-drain", owner)
	repo := postgres.NewStorageConnectionRepository(e.db)
	uploads := postgres.NewUploadRepository(e.db)
	c := activeConnection(t, repo, org.ID, "Draining bucket", testKeyring(t))

	waiting := boundSession(org.ID, "up-waiting-"+e.run, c.ID, model.UploadStatusCompleted)
	if err := uploads.Create(ctx, waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetStatus(ctx, org.ID, c.ID, []model.ConnectionStatus{model.ConnectionActive}, model.ConnectionDisabled, "it", time.Now()); err != nil {
		t.Fatal(err)
	}
	if d, err := repo.Dependents(ctx, org.ID, c.ID); err != nil || d.ActiveUploads != 1 {
		t.Fatalf("a completed upload without an asset is a dependent: %+v %v", d, err)
	}
	if _, err := repo.Delete(ctx, org.ID, c.ID, "it", time.Now()); !errors.Is(err, model.ErrConnectionInUse) {
		t.Fatalf("the connection must not be deleted before its upload is processed: %v", err)
	}

	// A database where it was deleted anyway, before this check existed.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE storage_connections SET status = 'DELETED', deleted_at = now() WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	next := boundSession(org.ID, "up-next-"+e.run, model.PlatformConnectionID, model.UploadStatusCompleted)
	if err := uploads.Create(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.NewProcessingJobRepository(e.db).EnqueueDue(ctx, time.Now().UTC(), 3); err != nil {
		t.Fatalf("a stranded upload must not stop processing for everyone: %v", err)
	}
	var processed, stranded int
	err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE upload_id = $1), count(*) FILTER (WHERE upload_id = $2) FROM assets`,
		next.ID, waiting.ID).Scan(&processed, &stranded)
	if err != nil || processed != 1 || stranded != 0 {
		t.Fatalf("the next upload is processed (%d) and the stranded one is skipped (%d): %v", processed, stranded, err)
	}
}

// Two administrators disable a connection and make it the default at the
// same moment. Whichever wins, the default is never a disabled connection.
func TestDisableAndSetDefaultNeverLeaveADisabledDefault(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	owner, _ := e.account(t, "conn-race")
	org := e.organization(t, "conn-race", owner)
	repo := postgres.NewStorageConnectionRepository(e.db)
	keyring := testKeyring(t)

	outcomes := map[string]int{}
	for round := 0; round < 40; round++ {
		c := activeConnection(t, repo, org.ID, fmt.Sprintf("race %d", round), keyring)
		if err := repo.SetDefault(ctx, org.ID, model.PlatformConnectionID); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var disableErr, defaultErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, disableErr = repo.Disable(ctx, org.ID, c.ID, "admin-1", time.Now().UTC())
		}()
		go func() {
			defer wg.Done()
			<-start
			defaultErr = repo.SetDefault(ctx, org.ID, c.ID)
		}()
		close(start)
		wg.Wait()

		current, err := repo.DefaultConnection(ctx, org.ID)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := repo.GetConnection(ctx, org.ID, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current == c.ID && stored.Status != model.ConnectionActive {
			t.Fatalf("round %d: the default is %s (disable: %v, set default: %v)", round, stored.Status, disableErr, defaultErr)
		}
		if (disableErr == nil) == (defaultErr == nil) {
			t.Fatalf("round %d: exactly one may win (disable: %v, set default: %v)", round, disableErr, defaultErr)
		}
		if disableErr == nil {
			outcomes["disabled"]++
		} else {
			outcomes["made default"]++
		}
	}
	t.Logf("outcomes over 40 rounds: %v", outcomes)
}

// Reconciliation's scans of a tenant connection keep their own progress
// beside the cleanup worker's scan of the same prefix, and resume with the
// cursor and failure they saved. A finding names its connection.
func TestReconciliationScansKeepTheirOwnProgressInPostgres(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	owner, _ := e.account(t, "scan-kinds")
	org := e.organization(t, "scan-kinds", owner)
	c := activeConnection(t, postgres.NewStorageConnectionRepository(e.db), org.ID, "Scanned bucket", testKeyring(t))
	inventory := postgres.NewInventoryRepository(e.db)
	now := time.Now().UTC()
	prefix := "stage/" + org.ID + "/"

	objects, ok1, err1 := inventory.OpenScan(ctx, model.ScanObjects, c.ID, "b", prefix, "w1", "kind-o-"+e.run, now, time.Minute)
	sessions, ok2, err2 := inventory.OpenScan(ctx, model.ScanUploadSessions, c.ID, "b", prefix, "w1", "kind-s-"+e.run, now, time.Minute)
	t.Cleanup(func() {
		_, _ = e.db.Pool.Exec(context.Background(), `DELETE FROM inventory_scans WHERE id = ANY($1)`, []string{objects.ID, sessions.ID})
	})
	if err := errors.Join(err1, err2); err != nil || !ok1 || !ok2 || objects.ID == sessions.ID || sessions.Kind != model.ScanUploadSessions {
		t.Fatalf("two kinds over one prefix run side by side: %+v %+v %v", objects, sessions, err)
	}
	sessions.Cursor, sessions.Pages, sessions.Errors, sessions.LastError = "after-key", 3, 1, "list provider sessions: 503"
	if err := inventory.SaveProgress(ctx, sessions, "w1", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	resumed, ok, err := inventory.OpenScan(ctx, model.ScanUploadSessions, c.ID, "b", prefix, "w1", "kind-s2-"+e.run, now.Add(time.Second), time.Minute)
	if err != nil || !ok || resumed.ID != sessions.ID || resumed.Cursor != "after-key" || resumed.Errors != 1 || resumed.LastError != sessions.LastError {
		t.Fatalf("a restart resumes the saved scan: %+v %v", resumed, err)
	}
	if _, ok, _ := inventory.OpenScan(ctx, model.ScanUploadSessions, c.ID, "b", prefix, "w2", "kind-s3-"+e.run, now, time.Minute); ok {
		t.Fatal("another worker does not take a leased scan")
	}

	finding := model.ReconciliationFinding{
		ID: "finding-" + e.run, Kind: model.FindingOrphanObject, ConnectionID: c.ID, TenantID: org.ID, ObjectKey: prefix + "up-1",
		Action: model.ActionDeleteObject, Outcome: model.FindingDone, Reason: "upload session is ABORTED", CreatedAt: now,
	}
	if err := postgres.NewReconciliationRepository(e.db).RecordFinding(ctx, finding); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.db.Pool.Exec(context.Background(), `DELETE FROM reconciliation_findings WHERE id = $1`, finding.ID)
	})
	var stored string
	if err := e.db.Pool.QueryRow(ctx, `SELECT storage_connection_id FROM reconciliation_findings WHERE id = $1`, finding.ID).Scan(&stored); err != nil || stored != c.ID {
		t.Fatalf("the finding names its connection: %q %v", stored, err)
	}
}

func TestStorageConnectionsInPostgres(t *testing.T) {
	e := openSecurityEnv(t, nil)
	ctx := context.Background()
	ownerA, _ := e.account(t, "conn-a")
	ownerB, _ := e.account(t, "conn-b")
	orgA, orgB := e.organization(t, "conn-a", ownerA), e.organization(t, "conn-b", ownerB)
	repo := postgres.NewStorageConnectionRepository(e.db)
	uploads := postgres.NewUploadRepository(e.db)
	keyring := testKeyring(t)

	a := activeConnection(t, repo, orgA.ID, "Team A bucket", keyring)
	b := activeConnection(t, repo, orgB.ID, "Team B bucket", keyring)

	// Tenant scoping: B's connection does not exist for A.
	if _, err := repo.GetConnection(ctx, orgA.ID, b.ID); !errors.Is(err, model.ErrConnectionNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
	if _, err := repo.Credential(ctx, orgA.ID, b.ID, 1); !errors.Is(err, model.ErrCredentialMissing) {
		t.Fatalf("cross-tenant credential: %v", err)
	}
	listed, err := repo.ListConnections(ctx, orgA.ID)
	if err != nil || len(listed) != 2 || listed[0].ID != model.PlatformConnectionID || listed[1].ID != a.ID {
		t.Fatalf("a tenant sees the platform and its own: %+v %v", listed, err)
	}
	dup := a
	dup.ID = dup.ID + "x"
	if _, err := repo.CreateConnection(ctx, dup, interfaces.SealedCredential{}); !errors.Is(err, model.ErrConnectionExists) {
		t.Fatalf("names are unique per tenant: %v", err)
	}
	var stored []byte
	if err := e.db.Pool.QueryRow(ctx, `SELECT credential_ciphertext FROM storage_connections WHERE id = $1`, a.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("it-secret-value")) {
		t.Fatal("credentials are stored only as ciphertext")
	}

	// The binding trigger: another tenant's connection cannot be bound,
	// even by naming its ID, and a pending one takes no new upload.
	err = uploads.Create(ctx, boundSession(orgA.ID, "up-cross-"+e.run, b.ID, model.UploadStatusPending))
	if !errors.Is(err, model.ErrConnectionNotFound) {
		t.Fatalf("binding another tenant's connection must fail: %v", err)
	}
	if err := repo.SetDefault(ctx, orgA.ID, b.ID); !errors.Is(err, model.ErrConnectionNotFound) {
		t.Fatalf("another tenant's connection as default: %v", err)
	}
	pending := a
	pending.ID, pending.Name = a.ID+"p", "Pending"
	if _, err := repo.CreateConnection(ctx, pending, interfaces.SealedCredential{}); err != nil {
		t.Fatal(err)
	}
	if err := uploads.Create(ctx, boundSession(orgA.ID, "up-pending-"+e.run, pending.ID, model.UploadStatusPending)); !errors.Is(err, model.ErrConnectionNotActive) {
		t.Fatalf("a pending connection takes no upload: %v", err)
	}
	if err := repo.SetDefault(ctx, orgA.ID, pending.ID); !errors.Is(err, model.ErrConnectionNotActive) {
		t.Fatalf("a pending connection cannot be the default: %v", err)
	}

	// A bound session; its asset and job inherit the binding, which never
	// changes afterwards.
	session := boundSession(orgA.ID, "up-bound-"+e.run, a.ID, model.UploadStatusCompleted)
	if err := uploads.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	job, err := postgres.NewProcessingJobRepository(e.db).EnqueueUpload(ctx, orgA.ID, session.ID, time.Now().UTC(), 3)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := postgres.NewAssetRepository(e.db).GetByID(ctx, orgA.ID, job.AssetID)
	if err != nil || asset.StorageConnectionID != a.ID || asset.StagingConnectionID != a.ID {
		t.Fatalf("the asset is bound like its upload: %+v %v", asset, err)
	}
	var jobConnection string
	if err := e.db.Pool.QueryRow(ctx, `SELECT storage_connection_id FROM processing_jobs WHERE id = $1`, job.ID).Scan(&jobConnection); err != nil || jobConnection != a.ID {
		t.Fatalf("the job records the connection: %q %v", jobConnection, err)
	}
	_, err = e.db.Pool.Exec(ctx, `UPDATE assets SET storage_connection_id = 'platform' WHERE id = $1`, asset.ID)
	if err == nil || !strings.Contains(err.Error(), "cannot change") {
		t.Fatalf("an asset's binding is immutable: %v", err)
	}

	// Dependents block deletion until they are gone; deletion erases the
	// credential and frees the name.
	if err := repo.SetDefault(ctx, orgA.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetStatus(ctx, orgA.ID, a.ID, []model.ConnectionStatus{model.ConnectionActive}, model.ConnectionDisabled, "it", time.Now()); err != nil {
		t.Fatal(err)
	}
	d, err := repo.Dependents(ctx, orgA.ID, a.ID)
	if err != nil || d.LiveAssets != 1 || !d.IsDefault {
		t.Fatalf("dependents: %+v %v", d, err)
	}
	if _, err := repo.Delete(ctx, orgA.ID, a.ID, "it", time.Now()); !errors.Is(err, model.ErrConnectionInUse) {
		t.Fatalf("a connection with assets is not deleted: %v", err)
	}
	if err := repo.SetDefault(ctx, orgA.ID, model.PlatformConnectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE assets SET status = 'DELETED' WHERE id = $1`, asset.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.Delete(ctx, orgA.ID, a.ID, "it", time.Now())
	if err != nil || deleted.Status != model.ConnectionDeleted || deleted.HasCredential() {
		t.Fatalf("deleted: %+v %v", deleted, err)
	}
	if _, err := repo.Credential(ctx, orgA.ID, a.ID, 1); !errors.Is(err, model.ErrCredentialMissing) {
		t.Fatalf("no credential survives deletion: %v", err)
	}
	var leftover *[]byte
	_ = e.db.Pool.QueryRow(ctx, `SELECT credential_ciphertext FROM storage_connections WHERE id = $1`, a.ID).Scan(&leftover)
	if leftover != nil {
		t.Fatal("the ciphertext is erased")
	}
	if err := uploads.Create(ctx, boundSession(orgA.ID, "up-deleted-"+e.run, a.ID, model.UploadStatusPending)); !errors.Is(err, model.ErrConnectionNotActive) {
		t.Fatalf("nothing binds to a deleted connection: %v", err)
	}
	reuse := a
	reuse.ID = a.ID + "r"
	if _, err := repo.CreateConnection(ctx, reuse, interfaces.SealedCredential{}); err != nil {
		t.Fatalf("a deleted connection's name is free again: %v", err)
	}

	// Inventory scans are per connection.
	inventory := postgres.NewInventoryRepository(e.db)
	now := time.Now().UTC()
	s1, ok1, err1 := inventory.OpenScan(ctx, model.ScanObjects, "", "shared-bucket", "published/"+e.run+"/", "w1", "scan-p-"+e.run, now, time.Minute)
	s2, ok2, err2 := inventory.OpenScan(ctx, model.ScanObjects, b.ID, "shared-bucket", "published/"+e.run+"/", "w1", "scan-b-"+e.run, now, time.Minute)
	if err1 != nil || err2 != nil || !ok1 || !ok2 || s1.ConnectionID != model.PlatformConnectionID || s2.ConnectionID != b.ID {
		t.Fatalf("one running scan per connection: %+v %+v %v %v", s1, s2, err1, err2)
	}
	t.Cleanup(func() {
		_, _ = e.db.Pool.Exec(context.Background(), `DELETE FROM inventory_scans WHERE id = ANY($1)`, []string{s1.ID, s2.ID})
	})

	holding, err := repo.ConnectionsHoldingWork(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range holding {
		if c.ID == b.ID {
			found = true
		}
		if c.Status != model.ConnectionActive && c.Status != model.ConnectionDisabled {
			t.Fatalf("only connections that hold work: %+v", c)
		}
	}
	if !found {
		t.Fatal("an active connection holds work")
	}
}

// ─── End to end over HTTP, with MinIO as the tenant's bucket ──────────

type byosStack struct {
	*securityStack
	registry    *registry.Registry
	connections *postgres.StorageConnectionRepository
	health      *storageconn.Service
	copies      *copyengine.Service
}

func openBYOSStack(t *testing.T) *byosStack {
	t.Helper()
	if os.Getenv("TEST_S3_COMPATIBLE_ENDPOINT") == "" {
		t.Skip("TEST_S3_COMPATIBLE_ENDPOINT is not set; the BYOS end-to-end test needs the compose MinIO")
	}
	s := openSecurityStack(t, map[string]string{"RATE_LIMIT_STORAGE_TEST": "100/1m"})
	cfg := config.Storage{BYOSEnabled: true, AllowPrivateEndpoints: true, ClientCacheSize: 16, ClientCacheTTL: time.Minute,
		ConnectionCacheTTL: time.Second, OperationTimeout: time.Minute, TestTimeout: 20 * time.Second,
		MaxConcurrentPerConnection: 8, TransferPartBytes: 5 << 20}
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = true
	connections := postgres.NewStorageConnectionRepository(s.db)
	keyring := testKeyring(t)
	reg, err := registry.New(registry.Dependencies{
		Config: cfg,
		Platform: interfaces.StorageLocation{
			StoragePlace: interfaces.StoragePlace{ConnectionID: model.PlatformConnectionID, StagingBucket: s.ie.bucket,
				StagingPrefix: "quarantine", ReadyBucket: s.ie.bucket, ReadyPrefix: "published"},
			Provider: model.ProviderPlatform, Storage: s.ie.storage,
		},
		Connections: connections, Sealer: keyring, Build: registry.NewBuilder(policy, cfg.TransferPartBytes).Build,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.ie.single.Route(reg, reg)
	s.ie.multi.Route(reg, reg)
	s.ie.proc.Route(reg)
	s.delivery.Route(reg)
	s.lifecycle.Route(reg)
	connService, err := storageconn.NewService(storageconn.Dependencies{
		Connections: connections, Sealer: keyring, Clients: reg, Audit: s.audit, Log: s.log,
		Health: connections, Alerts: postgres.NewAccessRepository(s.db), Drift: connections,
	}, cfg, policy)
	if err != nil {
		t.Fatal(err)
	}
	reg.GuardPlacement(connService)
	s.placement = connService
	copies := copyengine.NewService(copyengine.Dependencies{
		Store: postgres.NewCopyRepository(s.db), Router: reg, Packs: postgres.NewPackRepository(s.db),
		Locks: postgres.NewObjectLockRepository(s.db), Posture: connections, Alerts: postgres.NewAccessRepository(s.db), Audit: s.audit,
	}, copyengine.Config{Owner: "it-copies", Batch: 100, MassDeletions: 1}, s.log)
	s.protection = copies
	s.delivery.FailOver(copies)
	s.datasets = s.newDatasetService(t, nil)

	rates := map[string]ratelimit.Rule{}
	for name, rule := range s.sec.Rates {
		rates[name] = ratelimit.Rule{Requests: rule.Requests, Window: rule.Window}
	}
	api := httptest.NewServer(server.NewRouter(server.Routes{
		Resolver: middleware.BearerResolver[identity.Identity](s.identity),
		Identity: s.identity, Quotas: s.quotas, Audit: s.audit,
		Uploads: s.ie.single, Multipart: s.ie.multi, Delivery: s.delivery, Lifecycle: s.lifecycle, Datasets: s.datasets,
		StorageConnections: connService, ProxyEnabled: true, Limiter: s.limiter, Rates: rates, Log: s.log,
		Access: s.access, Copies: copies,
	}))
	t.Cleanup(api.Close)
	s.api = api
	return &byosStack{securityStack: s, registry: reg, connections: connections, health: connService, copies: copies}
}

// removePrefix deletes what the test wrote to the tenant bucket.
func (s *byosStack) removePrefix(t *testing.T, tenantID, connectionID string, prefixes ...string) {
	ctx := context.Background()
	loc, err := s.registry.Locate(ctx, tenantID, connectionID)
	if err != nil {
		return
	}
	for _, prefix := range prefixes {
		page, err := loc.Storage.ListObjectsPage(ctx, loc.ReadyBucket, prefix, "", 1000)
		if err != nil {
			continue
		}
		for _, o := range page.Objects {
			_ = loc.Storage.DeleteObject(ctx, loc.ReadyBucket, o.ObjectKey)
		}
	}
}

func put(t *testing.T, rawURL string, headers map[string]string, body []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, rawURL, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT %d", resp.StatusCode)
	}
}

func TestBringYourOwnStorageEndToEnd(t *testing.T) {
	s := openBYOSStack(t)
	ctx := context.Background()
	tm := s.team(t, "byos")
	owner := s.signIn(t, tm.ownerUser.Email, tm.ownerPW).AccessToken
	admin := s.signIn(t, tm.adminUser.Email, tm.adminPW).AccessToken
	dev := s.signIn(t, tm.devUser.Email, tm.devPW).AccessToken
	auditor := s.signIn(t, tm.auditorUser.Email, tm.auditorPW).AccessToken
	org := tm.org.ID
	bucket := os.Getenv("TEST_S3_COMPATIBLE_BUCKET")
	if bucket == "" {
		bucket = "object-storage-it"
	}
	accessKey, secretKey := os.Getenv("TEST_S3_COMPATIBLE_ACCESS_KEY"), os.Getenv("TEST_S3_COMPATIBLE_SECRET_KEY")
	run := strings.TrimPrefix(s.run, "r")
	staging, ready := "st"+run[len(run)-8:], "pub"+run[len(run)-8:]

	var providers response.StorageProvidersResponse
	s.expect(t, auditor, org, http.MethodGet, "/api/v1/storage-providers", "", http.StatusOK, &providers)
	if !providers.BYOSEnabled || len(providers.Providers) != 6 {
		t.Fatalf("the provider catalog: %+v", providers)
	}
	create := fmt.Sprintf(`{"name":"MinIO","provider":"S3_COMPATIBLE","auth_method":"ACCESS_KEY","bucket":%q,
		"endpoint":%q,"force_path_style":true,"staging_prefix":%q,"ready_prefix":%q,
		"credentials":{"access_key_id":%q,"secret_access_key":%q}}`,
		bucket, os.Getenv("TEST_S3_COMPATIBLE_ENDPOINT"), staging, ready, accessKey, secretKey)
	s.expect(t, dev, org, http.MethodPost, "/api/v1/storage-connections", create, http.StatusForbidden, nil)

	var conn response.StorageConnectionResponse
	created := s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections", create, http.StatusCreated, &conn)
	if conn.Status != "PENDING" || !conn.HasCredentials || strings.Contains(string(created.body), secretKey) {
		t.Fatalf("created without echoing credentials: %s", created.body)
	}
	t.Cleanup(func() { s.removePrefix(t, org, conn.ID, staging+"/", ready+"/") })

	// Another organization cannot see it, or use it by ID.
	other := s.team(t, "byos-other")
	otherAdmin := s.signIn(t, other.adminUser.Email, other.adminPW).AccessToken
	s.expect(t, otherAdmin, other.org.ID, http.MethodGet, "/api/v1/storage-connections/"+conn.ID, "", http.StatusNotFound, nil)

	var tested response.StorageTestResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/test", `{"write_probe":true}`, http.StatusOK, &tested)
	if !tested.OK || tested.Connection == nil || tested.Connection.Status != "ACTIVE" || tested.Capabilities == nil || !tested.Capabilities.Multipart {
		t.Fatalf("a passing test activates: %+v", tested)
	}
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/set-default", "", http.StatusOK, nil)

	// An upload goes to the default: signed by MinIO, under the
	// connection's staging prefix.
	data := []byte("bring your own storage, end to end\n")
	var upload response.CreateUploadResponse
	body, _ := json.Marshal(uploadOf("byos-file", data))
	s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusCreated, &upload)
	if upload.StorageConnectionID != conn.ID || !strings.Contains(upload.UploadURL, "/"+bucket+"/"+staging+"/") {
		t.Fatalf("bound to the default connection: %+v", upload)
	}
	put(t, upload.UploadURL, map[string]string{"Content-Type": "text/plain"}, data)
	s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads/"+upload.UploadID+"/complete", "", http.StatusOK, nil)
	if _, err := s.ie.proc.Intake(ctx, org, upload.UploadID); err != nil {
		t.Fatalf("processing: %v", err)
	}
	asset, err := s.ie.assets.GetByUpload(ctx, org, upload.UploadID)
	if err != nil || asset.Status != model.AssetStatusAvailable || !strings.HasPrefix(asset.ObjectKey, ready+"/"+org+"/") {
		t.Fatalf("verified, scanned and published on the tenant's bucket: %+v %v", asset, err)
	}
	if _, ok := s.ie.storage.Object(asset.ObjectKey); ok {
		t.Fatal("nothing was published on the platform's storage")
	}

	var link response.DownloadURLResponse
	s.expect(t, dev, org, http.MethodPost, "/api/v1/assets/"+asset.ID+"/download-url", `{}`, http.StatusOK, &link)
	resp, err := http.Get(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
		t.Fatalf("downloaded from the tenant's bucket: %d %q", resp.StatusCode, got)
	}

	// The default cannot be disabled.
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/disable", "", http.StatusConflict, nil)
	s.expect(t, owner, org, http.MethodPost, "/api/v1/storage-connections/platform/set-default", "", http.StatusOK, nil)

	// A new default does not move what is already stored: the asset is
	// still read from the tenant's bucket.
	var again response.DownloadURLResponse
	s.expect(t, dev, org, http.MethodPost, "/api/v1/assets/"+asset.ID+"/download-url", `{}`, http.StatusOK, &again)
	resp, err = http.Get(again.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, data) || !strings.HasPrefix(again.URL, link.URL[:strings.Index(link.URL, "?")]) {
		t.Fatalf("after the default changed: %d %q", resp.StatusCode, got)
	}

	// A dataset may choose the connection for its own uploads (with
	// storage.use); other uploads follow the default again.
	var ds response.DatasetResponse
	if r := s.call(t, dev, org, http.MethodPost, "/api/v1/datasets", `{"name":"byos-set"}`); r.status/100 != 2 || json.Unmarshal(r.body, &ds) != nil {
		t.Fatalf("create dataset: %s", r)
	}
	choose := func(id string) string {
		return fmt.Sprintf(`{"revision":%d,"storage_connection_id":%q}`, ds.Revision, id)
	}
	s.expect(t, dev, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, choose("sc_not_ours"), http.StatusNotFound, nil)
	s.expect(t, dev, org, http.MethodPatch, "/api/v1/datasets/"+ds.ID, choose(conn.ID), http.StatusOK, &ds)
	if ds.StorageConnectionID != conn.ID {
		t.Fatalf("the dataset's connection: %+v", ds)
	}
	inDataset := uploadOf("byos-in-dataset", data)
	inDataset.Dataset = "byos-set"
	body, _ = json.Marshal(inDataset)
	var second response.CreateUploadResponse
	s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusCreated, &second)
	body, _ = json.Marshal(uploadOf("byos-default", data))
	var third response.CreateUploadResponse
	s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusCreated, &third)
	if second.StorageConnectionID != conn.ID || third.StorageConnectionID != model.PlatformConnectionID {
		t.Fatalf("dataset uploads follow the dataset, others the default: %s %s", second.StorageConnectionID, third.StorageConnectionID)
	}

	// Disabled: new uploads are refused, existing assets still delivered.
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/disable", "", http.StatusOK, nil)
	req := uploadOf("byos-after-disable", data)
	req.StorageConnectionID = conn.ID
	body, _ = json.Marshal(req)
	if r := s.call(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body)); r.status != http.StatusConflict || !strings.Contains(string(r.body), "connection_not_active") {
		t.Fatalf("a disabled connection takes no new upload: %s", r)
	}
	s.expect(t, dev, org, http.MethodPost, "/api/v1/assets/"+asset.ID+"/download-url", `{}`, http.StatusOK, nil)

	inDataset.IdempotencyKey = "byos-in-dataset-2"
	body, _ = json.Marshal(inDataset)
	if r := s.call(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body)); r.status != http.StatusConflict {
		t.Fatalf("the dataset's disabled connection takes no new upload: %s", r)
	}

	var blocked response.StorageDependentsResponse
	s.expect(t, admin, org, http.MethodDelete, "/api/v1/storage-connections/"+conn.ID, "", http.StatusConflict, &blocked)
	if blocked.LiveAssets != 1 || blocked.ActiveUploads != 1 || blocked.Datasets != 1 {
		t.Fatalf("its asset, open upload and dataset block deletion: %+v", blocked)
	}

	// Rotation to credentials the provider refuses is not stored.
	var current response.StorageConnectionResponse
	s.expect(t, admin, org, http.MethodGet, "/api/v1/storage-connections/"+conn.ID, "", http.StatusOK, &current)
	rotate := fmt.Sprintf(`{"revision":%d,"credentials":{"access_key_id":"wrong-key","secret_access_key":"wrong-secret-xyz"}}`, current.Revision)
	var refused response.StorageTestResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections/"+conn.ID+"/rotate-credentials", rotate, http.StatusUnprocessableEntity, &refused)
	if refused.OK || refused.Category == "" {
		t.Fatalf("a sanitized refusal: %+v", refused)
	}
	s.expect(t, admin, org, http.MethodGet, "/api/v1/storage-connections/"+conn.ID, "", http.StatusOK, &current)
	if current.CredentialRotation != 1 {
		t.Fatalf("the working credentials stay: %+v", current)
	}

	// The audit trail names every change and holds no credential.
	for _, action := range []string{"storage_connection.create", "storage_connection.test", "storage_connection.set_default",
		"storage_connection.disable", "storage_connection.rotate"} {
		events := s.auditEvents(t, org, action)
		if len(events) == 0 {
			t.Errorf("no %s audit event", action)
		}
		for _, ev := range events {
			dump, _ := json.Marshal(ev)
			if strings.Contains(string(dump), secretKey) || strings.Contains(string(dump), "wrong-secret-xyz") {
				t.Fatalf("a credential reached the audit log: %s", dump)
			}
		}
	}
	if strings.Contains(s.logs.String(), secretKey) || strings.Contains(s.logs.String(), "wrong-secret-xyz") {
		t.Fatal("a credential reached the logs")
	}
}

// Edits, a refused edit, a credential rotation and a disable/enable
// cycle, made while readers keep reading a published asset and an upload
// is in flight: none of them interrupts existing work, and a change that
// fails its test changes nothing.
func TestConnectionChangesKeepExistingWorkReachable(t *testing.T) {
	s := openBYOSStack(t)
	ctx := context.Background()
	tm := s.team(t, "byos-change")
	owner := s.signIn(t, tm.ownerUser.Email, tm.ownerPW).AccessToken
	admin := s.signIn(t, tm.adminUser.Email, tm.adminPW).AccessToken
	dev := s.signIn(t, tm.devUser.Email, tm.devPW).AccessToken
	org := tm.org.ID
	bucket := os.Getenv("TEST_S3_COMPATIBLE_BUCKET")
	if bucket == "" {
		bucket = "object-storage-it"
	}
	endpoint := os.Getenv("TEST_S3_COMPATIBLE_ENDPOINT")
	accessKey, secretKey := os.Getenv("TEST_S3_COMPATIBLE_ACCESS_KEY"), os.Getenv("TEST_S3_COMPATIBLE_SECRET_KEY")
	run := strings.TrimPrefix(s.run, "r")
	staging, ready := "stc"+run[len(run)-8:], "pubc"+run[len(run)-8:]
	path := "/api/v1/storage-connections/"

	create := fmt.Sprintf(`{"name":"MinIO","provider":"S3_COMPATIBLE","auth_method":"ACCESS_KEY","bucket":%q,
		"endpoint":%q,"force_path_style":true,"staging_prefix":%q,"ready_prefix":%q,
		"credentials":{"access_key_id":%q,"secret_access_key":%q}}`,
		bucket, endpoint, staging, ready, accessKey, secretKey)
	var conn response.StorageConnectionResponse
	s.expect(t, admin, org, http.MethodPost, "/api/v1/storage-connections", create, http.StatusCreated, &conn)
	t.Cleanup(func() { s.removePrefix(t, org, conn.ID, staging+"/", ready+"/") })
	s.expect(t, admin, org, http.MethodPost, path+conn.ID+"/test", "", http.StatusOK, nil)
	s.expect(t, admin, org, http.MethodPost, path+conn.ID+"/set-default", "", http.StatusOK, nil)

	data := []byte("still here after every change\n")
	publish := func(name string) *model.Asset {
		t.Helper()
		var upload response.CreateUploadResponse
		body, _ := json.Marshal(uploadOf(name, data))
		s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusCreated, &upload)
		put(t, upload.UploadURL, map[string]string{"Content-Type": "text/plain"}, data)
		s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads/"+upload.UploadID+"/complete", "", http.StatusOK, nil)
		if _, err := s.ie.proc.Intake(ctx, org, upload.UploadID); err != nil {
			t.Fatalf("processing: %v", err)
		}
		asset, err := s.ie.assets.GetByUpload(ctx, org, upload.UploadID)
		if err != nil || asset.Status != model.AssetStatusAvailable || asset.StorageConnectionID != conn.ID {
			t.Fatalf("published on the connection: %+v %v", asset, err)
		}
		return asset
	}
	asset := publish("change-first")

	// Readers resolve the connection and read the asset throughout.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	reads, failures := 0, []string{}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				loc, err := s.registry.Locate(ctx, org, conn.ID)
				if err == nil {
					_, err = loc.Storage.HeadObject(ctx, loc.ReadyBucket, asset.ObjectKey)
				}
				mu.Lock()
				reads++
				if err != nil {
					failures = append(failures, err.Error())
				}
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}

	// An upload granted before the changes finishes after them.
	var inFlight response.CreateUploadResponse
	body, _ := json.Marshal(uploadOf("change-in-flight", data))
	s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads", string(body), http.StatusCreated, &inFlight)

	current := func() response.StorageConnectionResponse {
		var c response.StorageConnectionResponse
		s.expect(t, admin, org, http.MethodGet, path+conn.ID, "", http.StatusOK, &c)
		return c
	}
	edit := func(revision int64, to string, want int) {
		t.Helper()
		var out response.StorageTestResponse
		r := s.expect(t, admin, org, http.MethodPatch, path+conn.ID, fmt.Sprintf(`{"revision":%d,"endpoint":%q}`, revision, to), want, nil)
		if want == http.StatusUnprocessableEntity {
			if err := json.Unmarshal(r.body, &out); err != nil || out.OK || out.Category == "" {
				t.Fatalf("a sanitized refusal: %s", r.body)
			}
		}
	}

	// The same MinIO under another name: tested, applied, still active.
	alternate := strings.Replace(endpoint, "127.0.0.1", "localhost", 1)
	if alternate == endpoint {
		alternate = strings.Replace(endpoint, "localhost", "127.0.0.1", 1)
	}
	if alternate != endpoint {
		edit(current().Revision, alternate, http.StatusOK)
		if c := current(); c.Status != "ACTIVE" || c.Endpoint != alternate {
			t.Fatalf("a tested edit keeps the connection active: %+v", c)
		}
	} else {
		t.Logf("no alternate name for %s; the applied edit is not exercised", endpoint)
	}
	before := current()
	edit(before.Revision, "http://127.0.0.1:1", http.StatusUnprocessableEntity)
	if c := current(); c.Status != "ACTIVE" || c.Endpoint != before.Endpoint || c.Revision != before.Revision {
		t.Fatalf("a refused edit changes nothing: %+v", c)
	}

	rotate := fmt.Sprintf(`{"revision":%d,"credentials":{"access_key_id":%q,"secret_access_key":%q}}`, current().Revision, accessKey, secretKey)
	s.expect(t, admin, org, http.MethodPost, path+conn.ID+"/rotate-credentials", rotate, http.StatusOK, nil)
	if c := current(); c.CredentialRotation != 2 {
		t.Fatalf("rotated: %+v", c)
	}

	s.expect(t, owner, org, http.MethodPost, path+"platform/set-default", "", http.StatusOK, nil)
	s.expect(t, admin, org, http.MethodPost, path+conn.ID+"/disable", "", http.StatusOK, nil)
	var enabled response.StorageTestResponse
	s.expect(t, admin, org, http.MethodPost, path+conn.ID+"/enable", "", http.StatusOK, &enabled)
	if !enabled.OK || enabled.Connection == nil || enabled.Connection.Status != "ACTIVE" {
		t.Fatalf("enabled after a passing test: %+v", enabled)
	}

	put(t, inFlight.UploadURL, map[string]string{"Content-Type": "text/plain"}, data)
	s.expect(t, dev, org, http.MethodPost, "/api/v1/uploads/"+inFlight.UploadID+"/complete", "", http.StatusOK, nil)
	if _, err := s.ie.proc.Intake(ctx, org, inFlight.UploadID); err != nil {
		t.Fatalf("processing the in-flight upload: %v", err)
	}
	if a, err := s.ie.assets.GetByUpload(ctx, org, inFlight.UploadID); err != nil || a.Status != model.AssetStatusAvailable || a.StorageConnectionID != conn.ID {
		t.Fatalf("the in-flight upload is published where it was bound: %+v %v", a, err)
	}

	close(stop)
	wg.Wait()
	if reads == 0 || len(failures) > 0 {
		t.Fatalf("%d of %d reads failed during the changes, first: %v", len(failures), reads, failures[:min(1, len(failures))])
	}
	t.Logf("%d reads, none failed", reads)
}
