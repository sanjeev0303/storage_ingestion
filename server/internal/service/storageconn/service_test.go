package storageconn

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/netguard"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/secretbox"
	memoryrepo "github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

// failingStorage answers capability checks with an error.
type failingStorage struct {
	*memory.ObjectStorage
	err error
}

func (f failingStorage) ProviderCapabilities(context.Context, string) (model.ProviderCapabilities, error) {
	return model.ProviderCapabilities{}, f.err
}

type fakeClients struct {
	mu          sync.Mutex
	storage     *memory.ObjectStorage
	fail        error // answered by the provider's capability check
	built       []model.StorageSecrets
	stored      int
	invalidated []string
}

func (f *fakeClients) provider() interfaces.StorageProvider {
	if f.fail != nil {
		return failingStorage{f.storage, f.fail}
	}
	return f.storage
}

func (f *fakeClients) Build(_ context.Context, _ model.StorageConnection, secrets model.StorageSecrets) (interfaces.StorageProvider, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.built = append(f.built, secrets)
	return f.provider(), func() {}, nil
}

func (f *fakeClients) BuildStored(context.Context, model.StorageConnection) (interfaces.StorageProvider, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored++
	return f.provider(), func() {}, nil
}

func (f *fakeClients) Invalidate(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, id)
}

type auditLog struct {
	mu     sync.Mutex
	events []model.AuditEvent
}

func (a *auditLog) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

type fixture struct {
	svc     *Service
	repo    *memoryrepo.StorageConnectionRepository
	clients *fakeClients
	audit   *auditLog
	keyring *secretbox.Keyring
	logs    *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	keyring, err := secretbox.NewKeyring(map[int][]byte{1: bytes.Repeat([]byte{9}, 32)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{repo: memoryrepo.NewStorageConnectionRepository(), clients: &fakeClients{storage: memory.NewObjectStorage()},
		audit: &auditLog{}, keyring: keyring, logs: &bytes.Buffer{}}
	// The local-development policy admits the loopback test endpoint, so
	// no test depends on DNS.
	policy := netguard.DefaultPolicy()
	policy.AllowPrivate = true
	f.svc, err = NewService(Dependencies{
		Connections: f.repo, Sealer: keyring, Clients: f.clients, Audit: f.audit,
		Log: slog.New(slog.NewTextHandler(f.logs, nil)),
	}, config.Storage{BYOSEnabled: true}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

var (
	admin     = model.Principal{TenantID: "t1", UserID: "admin", Permissions: model.AllPermissions &^ model.PermOwnership}
	developer = model.Principal{TenantID: "t1", UserID: "dev", Permissions: model.PermUploadsWrite | model.PermStorageRead | model.PermStorageUse}
	stranger  = model.Principal{TenantID: "t2", UserID: "other", Permissions: model.AllPermissions}
	secrets   = model.StorageSecrets{AccessKeyID: "AKIDSECRETPART", SecretAccessKey: "very-secret-value-123"}
)

func minioInput() CreateInput {
	return CreateInput{
		Name: "Team bucket", Provider: model.ProviderS3Compatible, AuthMethod: model.AuthAccessKey,
		Bucket: "team-bucket", Endpoint: "http://127.0.0.1:9000", ForcePathStyle: true, Secrets: secrets,
	}
}

// leaks reports where a secret value turns up.
func (f *fixture) leaks(t *testing.T, values ...any) {
	t.Helper()
	dump, _ := json.Marshal(f.audit.events)
	haystacks := []string{string(dump), f.logs.String()}
	for _, v := range values {
		b, _ := json.Marshal(v)
		haystacks = append(haystacks, string(b), fmt.Sprintf("%+v", v))
	}
	for _, h := range haystacks {
		for _, secret := range []string{secrets.SecretAccessKey, secrets.AccessKeyID, "rotated-secret-456"} {
			if strings.Contains(h, secret) {
				t.Fatalf("a credential leaked: %s", h)
			}
		}
	}
}

func TestCreateSealsCredentialsBoundToTheConnection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.svc.Create(ctx, admin, minioInput())
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != model.ConnectionPending || c.CredentialVersion != 1 || c.StagingPrefix != "quarantine" || c.ReadyPrefix != "assets" {
		t.Fatalf("a new connection waits for a test: %+v", c)
	}

	sealed, err := f.repo.Credential(ctx, "t1", c.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.Ciphertext, []byte(secrets.SecretAccessKey)) {
		t.Fatal("the credential is stored in plaintext")
	}
	plain, err := f.keyring.Open(secretbox.Sealed{KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}, c.CredentialAAD(1))
	if err != nil || !bytes.Contains(plain, []byte(secrets.SecretAccessKey)) {
		t.Fatalf("the sealed credential opens with its own associated data: %v", err)
	}
	other := c
	other.TenantID = "t2"
	if _, err := f.keyring.Open(secretbox.Sealed{KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}, other.CredentialAAD(1)); err == nil {
		t.Fatal("another tenant's associated data must not open it")
	}
	if _, err := f.keyring.Open(secretbox.Sealed{KeyVersion: sealed.KeyVersion, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext}, c.CredentialAAD(2)); err == nil {
		t.Fatal("another credential version's associated data must not open it")
	}
	f.leaks(t, c)

	// Another tenant cannot see or change it.
	if _, err := f.svc.Get(ctx, stranger, c.ID); !errors.Is(err, model.ErrConnectionNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
	if _, _, err := f.svc.Test(ctx, stranger, c.ID, false); !errors.Is(err, model.ErrConnectionNotFound) {
		t.Fatalf("cross-tenant test: %v", err)
	}
	if _, err := f.svc.Create(ctx, admin, minioInput()); !errors.Is(err, model.ErrConnectionExists) {
		t.Fatalf("names are unique per tenant: %v", err)
	}
}

func TestCreateValidatesBeforeStoringAnything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := func(change func(*CreateInput)) error {
		in := minioInput()
		in.Name = "n" + fmt.Sprint(len(f.audit.events))
		change(&in)
		_, err := f.svc.Create(ctx, admin, in)
		return err
	}
	if err := bad(func(in *CreateInput) { in.Endpoint = "http://169.254.169.254" }); !errors.Is(err, model.ErrEndpointRejected) {
		t.Errorf("metadata endpoint: %v", err)
	}
	if err := bad(func(in *CreateInput) { in.Endpoint = "https://user:pw@s3.example.com" }); !errors.Is(err, model.ErrEndpointRejected) {
		t.Errorf("credentials in the endpoint: %v", err)
	}
	if err := bad(func(in *CreateInput) { in.Secrets.AccountKey = "x" }); !errors.Is(err, model.ErrInvalidConnection) {
		t.Errorf("a field the method does not take: %v", err)
	}
	if err := bad(func(in *CreateInput) { in.Secrets = model.StorageSecrets{AccessKeyID: "a"} }); !errors.Is(err, model.ErrInvalidConnection) {
		t.Errorf("a missing secret: %v", err)
	}
	if err := bad(func(in *CreateInput) { in.Provider = model.ProviderPlatform }); !errors.Is(err, model.ErrInvalidConnection) {
		t.Errorf("the platform provider is not a tenant's: %v", err)
	}
	if err := bad(func(in *CreateInput) {
		in.Provider, in.AuthMethod, in.AccountID, in.Endpoint = model.ProviderAzureBlob, model.AuthSASToken, "acct", ""
		in.Secrets = model.StorageSecrets{SASToken: "sv=1&sig=x"}
	}); !errors.Is(err, model.ErrInvalidConnection) {
		t.Errorf("a SAS token needs platform staging: %v", err)
	}
	if _, err := f.svc.Create(ctx, developer, minioInput()); !errors.Is(err, model.ErrForbidden) {
		t.Errorf("developers do not manage connections: %v", err)
	}
	if list, _, _ := f.svc.List(ctx, admin); len(list) != 1 {
		t.Fatalf("only the platform connection exists: %d", len(list))
	}
	f.leaks(t)
}

func TestAssumeRoleGetsAPlatformExternalID(t *testing.T) {
	f := newFixture(t)
	c, err := f.svc.Create(context.Background(), admin, CreateInput{
		Name: "aws", Provider: model.ProviderAWSS3, AuthMethod: model.AuthAssumeRole, Bucket: "b-123", Region: "eu-west-1",
		RoleARN: "arn:aws:iam::123456789012:role/storage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.ExternalID, "osp-") || c.HasCredential() {
		t.Fatalf("the platform generates the external ID and stores no secret: %+v", c)
	}
}

func TestConnectionTestActivatesAndReportsSanitizedFailures(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.svc.Create(ctx, admin, minioInput())
	if err != nil {
		t.Fatal(err)
	}

	f.clients.fail = fmt.Errorf("head bucket: AccessDenied: arn:aws:iam::123456789012:user/x is not authorized: %w", interfaces.ErrStorageAccessDenied)
	stored, result, err := f.svc.Test(ctx, admin, c.ID, false)
	if err != nil || result.OK || result.Category != CategoryAccessDenied || stored.Status != model.ConnectionPending {
		t.Fatalf("a failing test leaves it pending: %+v %+v %v", stored, result, err)
	}
	if strings.Contains(result.Message, "arn:aws") || strings.Contains(stored.LastErrorCategory, "arn") {
		t.Fatal("the provider's message must not be returned or stored")
	}

	f.clients.fail = nil
	stored, result, err = f.svc.Test(ctx, admin, c.ID, true)
	if err != nil || !result.OK || stored.Status != model.ConnectionActive || stored.Capabilities == nil {
		t.Fatalf("a passing test activates: %+v %+v %v", stored, result, err)
	}
	if f.clients.storage.WriteCalls != 1 {
		t.Fatalf("the probe writes once, wrote %d", f.clients.storage.WriteCalls)
	}
	page, _ := f.clients.storage.ListObjectsPage(ctx, "team-bucket", "", "", 100)
	if len(page.Objects) != 0 {
		t.Fatalf("the probe leaves nothing behind: %+v", page.Objects)
	}
	if f.clients.stored != 2 || len(f.clients.built) != 0 {
		t.Fatalf("tests use the stored credentials: stored %d built %d", f.clients.stored, len(f.clients.built))
	}
	f.leaks(t, result, stored)
}

func TestRotationIsTestedFirstAndNeverReturnsSecrets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, _ := f.svc.Create(ctx, admin, minioInput())
	c, _, _ = f.svc.Test(ctx, admin, c.ID, false)
	rotated := model.StorageSecrets{AccessKeyID: "AKIDROTATED", SecretAccessKey: "rotated-secret-456"}

	f.clients.fail = interfaces.ErrStorageCredentialsRejected
	if _, result, err := f.svc.RotateCredentials(ctx, admin, c.ID, c.Revision, rotated); !errors.Is(err, ErrTestFailed) || result.Category != CategoryInvalidCredentials {
		t.Fatalf("credentials that fail the test are refused: %+v %v", result, err)
	}
	if current, _ := f.repo.GetConnection(ctx, "t1", c.ID); current.CredentialVersion != 1 {
		t.Fatal("refused credentials are not stored")
	}

	f.clients.fail = nil
	if _, _, err := f.svc.RotateCredentials(ctx, admin, c.ID, c.Revision+7, rotated); !errors.Is(err, model.ErrRevisionConflict) {
		t.Fatalf("a stale revision is refused: %v", err)
	}
	f.clients.invalidated = nil
	stored, result, err := f.svc.RotateCredentials(ctx, admin, c.ID, c.Revision, rotated)
	if err != nil || !result.OK || stored.CredentialVersion != 2 || stored.CredentialRotatedAt == nil {
		t.Fatalf("rotation: %+v %+v %v", stored, result, err)
	}
	if len(f.clients.invalidated) != 1 || f.clients.invalidated[0] != c.ID {
		t.Fatalf("rotation drops cached clients: %v", f.clients.invalidated)
	}
	if _, err := f.repo.Credential(ctx, "t1", c.ID, 1); err == nil {
		t.Fatal("the old credential version is gone")
	}
	f.leaks(t, stored, result)
}

func TestDisableDefaultAndDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, _ := f.svc.Create(ctx, admin, minioInput())
	if _, err := f.svc.SetDefault(ctx, admin, c.ID); !errors.Is(err, model.ErrConnectionNotActive) {
		t.Fatalf("an untested connection cannot be the default: %v", err)
	}
	c, _, _ = f.svc.Test(ctx, admin, c.ID, false)
	if _, err := f.svc.SetDefault(ctx, admin, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Disable(ctx, admin, c.ID); !errors.Is(err, model.ErrConnectionInUse) {
		t.Fatalf("the default cannot be disabled: %v", err)
	}
	if _, err := f.svc.SetDefault(ctx, admin, model.PlatformConnectionID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Delete(ctx, admin, c.ID); !errors.Is(err, model.ErrConnectionInUse) {
		t.Fatalf("an active connection is disabled before it is deleted: %v", err)
	}
	if _, err := f.svc.Disable(ctx, admin, c.ID); err != nil {
		t.Fatal(err)
	}

	f.repo.Blocking[c.ID] = interfaces.ConnectionDependents{LiveAssets: 3}
	_, dependents, err := f.svc.Delete(ctx, admin, c.ID)
	if !errors.Is(err, model.ErrConnectionInUse) || dependents.LiveAssets != 3 {
		t.Fatalf("assets still on it block deletion: %+v %v", dependents, err)
	}
	delete(f.repo.Blocking, c.ID)
	deleted, _, err := f.svc.Delete(ctx, admin, c.ID)
	if err != nil || deleted.Status != model.ConnectionDeleted || deleted.HasCredential() {
		t.Fatalf("deleted with its credentials erased: %+v %v", deleted, err)
	}
	if _, err := f.repo.Credential(ctx, "t1", c.ID, 1); err == nil {
		t.Fatal("no credential survives deletion")
	}
	if _, err := f.svc.Update(ctx, admin, model.PlatformConnectionID, 1, UpdateInput{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("the platform connection is not the tenant's to change: %v", err)
	}
	f.leaks(t)
}

func TestUpdatePointingElsewhereNeedsANewTest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, _ := f.svc.Create(ctx, admin, minioInput())
	c, _, _ = f.svc.Test(ctx, admin, c.ID, false)
	endpoint := "http://127.0.0.1:9100"
	updated, err := f.svc.Update(ctx, admin, c.ID, c.Revision, UpdateInput{Endpoint: &endpoint})
	if err != nil || updated.Status != model.ConnectionPending {
		t.Fatalf("a new endpoint returns it to pending: %+v %v", updated, err)
	}
	private := "https://10.1.2.3"
	if _, err := f.svc.Update(ctx, admin, c.ID, updated.Revision, UpdateInput{Endpoint: &private}); err != nil {
		t.Fatalf("the development policy admits a private address: %v", err)
	}
	f.svc.policy.AllowPrivate = false
	again, _ := f.repo.GetConnection(ctx, "t1", c.ID)
	if _, err := f.svc.Update(ctx, admin, c.ID, again.Revision, UpdateInput{Endpoint: &private}); !errors.Is(err, model.ErrEndpointRejected) {
		t.Fatalf("the production policy refuses it: %v", err)
	}
}

func TestBYOSDisabled(t *testing.T) {
	f := newFixture(t)
	f.svc.cfg.BYOSEnabled = false
	if _, err := f.svc.Create(context.Background(), admin, minioInput()); !errors.Is(err, model.ErrStorageNotConfigured) {
		t.Fatalf("no connections without BYOS: %v", err)
	}
	if list, defaultID, err := f.svc.List(context.Background(), developer); err != nil || len(list) != 1 || defaultID != model.PlatformConnectionID {
		t.Fatalf("the platform connection is still listed: %v %s %v", list, defaultID, err)
	}
}

func TestCategorize(t *testing.T) {
	cases := map[string]error{
		CategoryEndpointRejected:   fmt.Errorf("dial: %w", netguard.ErrRejected),
		CategoryTLS:                fmt.Errorf("get: %w", x509.UnknownAuthorityError{}),
		CategoryAccessDenied:       fmt.Errorf("list: %w", interfaces.ErrStorageAccessDenied),
		CategoryNotFound:           fmt.Errorf("bucket: %w", interfaces.ErrObjectNotFound),
		CategoryThrottled:          fmt.Errorf("list: %w", interfaces.ErrStorageThrottled),
		CategoryInvalidCredentials: fmt.Errorf("sign: %w", interfaces.ErrStorageCredentialsRejected),
		CategoryUnsupported:        model.ErrUnsupportedProvider,
		CategoryNetwork:            &net.OpError{Op: "dial", Err: errors.New("refused")},
		CategoryProviderError:      io.ErrUnexpectedEOF,
	}
	for want, err := range cases {
		if got := Categorize(err); got != want {
			t.Errorf("%v: got %s, want %s", err, got, want)
		}
		if categoryMessages[want] == "" {
			t.Errorf("%s has no message", want)
		}
	}
	if Categorize(context.DeadlineExceeded) != CategoryNetwork {
		t.Error("a timeout is a network failure")
	}
}

func TestSecretsNeverPrint(t *testing.T) {
	for _, out := range []string{fmt.Sprint(secrets), fmt.Sprintf("%v %+v %#v", secrets, secrets, secrets)} {
		if strings.Contains(out, secrets.SecretAccessKey) {
			t.Fatalf("printed: %s", out)
		}
	}
	b, _ := json.Marshal(struct{ S model.StorageSecrets }{secrets})
	if strings.Contains(string(b), secrets.SecretAccessKey) {
		t.Fatalf("marshalled: %s", b)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "secrets", secrets)
	if strings.Contains(buf.String(), secrets.SecretAccessKey) {
		t.Fatalf("logged: %s", buf.String())
	}
}
