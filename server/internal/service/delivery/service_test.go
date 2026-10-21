package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

var (
	owner      = member("tenant-1", "user-1", model.RoleDeveloper)
	content    = []byte("0123456789abcdefghij") // 20 bytes
	contentSum = sha256Hex(content)
	baseTime   = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
)

// member is an organization member with the built-in role roleID.
func member(tenantID, userID, roleID string) model.Principal {
	role, _ := model.BuiltInRole(roleID)
	return model.Principal{TenantID: tenantID, UserID: userID, RoleID: role.ID, Permissions: role.Permissions}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type env struct {
	svc     *Service
	assets  *memory.AssetRepository
	storage *storagememory.ObjectStorage
	audit   *memory.DeliveryAuditRepository
	tokens  *memory.DownloadTokenRepository
}

func newEnv(t *testing.T, cdn *CDNConfig) *env {
	t.Helper()
	e := &env{
		assets:  memory.NewAssetRepository(),
		storage: storagememory.NewObjectStorage(),
		audit:   memory.NewDeliveryAuditRepository(),
		tokens:  memory.NewDownloadTokenRepository(),
	}
	svc, err := NewService(e.assets, e.storage, e.audit, e.tokens, Config{
		Bucket: "private-bucket", Prefix: "assets", DefaultTTL: 5 * time.Minute, MaxTTL: 15 * time.Minute, CDN: cdn,
	}, quietLog)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return baseTime }
	e.svc = svc
	return e
}

// asset seeds an asset; AVAILABLE ones sit at their promoted key.
func (e *env) asset(t *testing.T, id string, status model.AssetStatus) {
	t.Helper()
	key := "quarantine/tenant-1/up-" + id
	storageETag := ""
	var published *time.Time
	if status == model.AssetStatusAvailable {
		key = "assets/tenant-1/" + id
		e.storage.PutObject(key, content)
		storageETag = e.storage.ETag(key)
		at := baseTime
		published = &at
	}
	asset := model.Asset{
		ID: id, TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-" + id, ObjectKey: key,
		FileName: "report.pdf", ContentType: "application/pdf", SizeBytes: int64(len(content)),
		ChecksumSHA256: contentSum, StorageETag: storageETag, PublishedAt: published,
		Status: status, CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	if status == model.AssetStatusRejected {
		asset.RejectionReason = "malware_detected"
	}
	if err := e.assets.Create(context.Background(), &asset); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyAvailableAssetsAreDelivered(t *testing.T) {
	cases := []struct {
		status model.AssetStatus
		want   error
	}{
		{model.AssetStatusAvailable, nil},
		{model.AssetStatusQuarantined, ErrNotAvailable},
		{model.AssetStatusProcessing, ErrNotAvailable},
		{model.AssetStatusRejected, ErrNotAvailable},
		{model.AssetStatusDeleted, ErrNotFound},
	}
	for _, c := range cases {
		t.Run(string(c.status), func(t *testing.T) {
			e := newEnv(t, nil)
			e.asset(t, "a1", c.status)

			_, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{})
			if !errors.Is(err, c.want) {
				t.Fatalf("URL: got %v, want %v", err, c.want)
			}
			_, err = e.svc.Open(context.Background(), owner, "a1", DownloadRequest{})
			if !errors.Is(err, c.want) {
				t.Fatalf("proxy: got %v, want %v", err, c.want)
			}
			if c.want != nil && len(e.storage.Presigns) != 0 {
				t.Fatal("no URL may be signed for an undeliverable asset")
			}
		})
	}
}

// A record always names its storage binding. One that has lost it is
// refused, never read from the platform bucket in its place.
func TestAnAssetWithoutABindingIsNotServed(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	e.assets.Update("a1", func(a *model.Asset) { a.StorageConnectionID = "" })

	if _, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{}); err == nil {
		t.Fatal("a download URL was signed for an asset without a binding")
	}
	if len(e.storage.Presigns) != 0 {
		t.Fatalf("the platform bucket was signed for: %+v", e.storage.Presigns)
	}
}

func TestOtherTenantsGetTheSameNotFound(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	for _, intruder := range []model.Principal{
		member("tenant-2", "user-1", model.RoleDeveloper), // another tenant, same user ID
		member("tenant-2", "user-2", model.RoleOwner),     // another tenant's owner
	} {
		for _, call := range []func() error{
			func() error { _, err := e.svc.Metadata(context.Background(), intruder, "a1"); return err },
			func() error {
				_, err := e.svc.CreateDownloadURL(context.Background(), intruder, "a1", URLRequest{})
				return err
			},
			func() error {
				_, err := e.svc.Open(context.Background(), intruder, "a1", DownloadRequest{})
				return err
			},
		} {
			if err := call(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%+v: expected ErrNotFound, got %v", intruder, err)
			}
		}
		// Identical to an asset that does not exist at all.
		if _, err := e.svc.Metadata(context.Background(), intruder, "no-such-asset"); !errors.Is(err, ErrNotFound) {
			t.Fatal("a missing asset must answer the same")
		}
	}
	if len(e.storage.Presigns) != 0 {
		t.Fatal("no URL may be signed for another tenant's asset")
	}
}

func TestMembersAccessByPermission(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	// The asset belongs to the organization: a teammate with the
	// developer role downloads it.
	if _, err := e.svc.CreateDownloadURL(context.Background(), member("tenant-1", "user-2", model.RoleDeveloper), "a1", URLRequest{}); err != nil {
		t.Fatalf("teammate download: %v", err)
	}

	// An auditor reads metadata but cannot download.
	auditor := member("tenant-1", "user-3", model.RoleAuditor)
	if _, err := e.svc.Metadata(context.Background(), auditor, "a1"); err != nil {
		t.Fatalf("auditor metadata: %v", err)
	}
	presigned := len(e.storage.Presigns)
	if _, err := e.svc.CreateDownloadURL(context.Background(), auditor, "a1", URLRequest{}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("auditor download URL: expected ErrForbidden, got %v", err)
	}
	if _, err := e.svc.Open(context.Background(), auditor, "a1", DownloadRequest{}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("auditor download: expected ErrForbidden, got %v", err)
	}
	if len(e.storage.Presigns) != presigned {
		t.Fatal("no URL may be signed for a caller without assets.download")
	}
}

func TestUnauthenticatedCallersGetNothing(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	for _, nobody := range []model.Principal{{}, {TenantID: "tenant-1", Permissions: model.AllPermissions}} {
		if _, err := e.svc.CreateDownloadURL(context.Background(), nobody, "a1", URLRequest{}); !errors.Is(err, model.ErrUnauthenticated) {
			t.Fatalf("expected ErrUnauthenticated, got %v", err)
		}
	}
	// A signed-in caller who named no organization has no tenant access.
	if _, err := e.svc.CreateDownloadURL(context.Background(), model.Principal{UserID: "user-1"}, "a1", URLRequest{}); !errors.Is(err, model.ErrTenantRequired) {
		t.Fatalf("expected ErrTenantRequired, got %v", err)
	}
	if len(e.storage.Presigns) != 0 {
		t.Fatal("no URL may be signed without a principal")
	}
}

func TestURLLifetimeIsBounded(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	for _, c := range []struct{ asked, want time.Duration }{
		{0, 5 * time.Minute},
		{time.Minute, time.Minute},
		{24 * time.Hour, 15 * time.Minute},
	} {
		got, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{TTL: c.asked})
		if err != nil {
			t.Fatal(err)
		}
		last := e.storage.Presigns[len(e.storage.Presigns)-1]
		if last.TTL != c.want || !got.ExpiresAt.Equal(baseTime.Add(c.want)) {
			t.Fatalf("asked %s: signed for %s, expires %s", c.asked, last.TTL, got.ExpiresAt)
		}
		if last.Bucket != "private-bucket" || last.Key != "assets/tenant-1/a1" {
			t.Fatalf("signed the wrong object: %+v", last)
		}
	}

	if _, err := NewService(e.assets, e.storage, e.audit, nil, Config{
		Bucket: "b", Prefix: "assets", DefaultTTL: time.Minute, MaxTTL: 2 * time.Hour,
	}, quietLog); err == nil {
		t.Fatal("a maximum above the hard limit must be refused")
	}
}

func TestFilenamesAreSanitized(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":                "passwd",
		`C:\Users\x\evil.exe`:             "evil.exe",
		"a\"b;c\r\nd.pdf":                 "abcd.pdf",
		".hidden":                         "hidden",
		"   ":                             "download",
		"résumé 2026.pdf":                 "résumé 2026.pdf",
		strings.Repeat("x", 400) + ".pdf": strings.Repeat("x", 150),
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}

	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	if _, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{Filename: "../résumé.pdf"}); err != nil {
		t.Fatal(err)
	}
	disposition := e.storage.Presigns[0].Options.ContentDisposition
	if !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, "r%C3%A9sum%C3%A9.pdf") || strings.Contains(disposition, "..") {
		t.Fatalf("unsafe or wrong disposition %q", disposition)
	}
}

func TestStorageFailureIsMappedAndAudited(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	e.storage.PresignErr = errors.New("presign download: provider error SignatureDoesNotMatch at internal-host")

	_, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{})
	if err != ErrStorageUnavailable {
		t.Fatalf("expected the bare ErrStorageUnavailable, got %v", err)
	}
	events := e.audit.Events()
	if len(events) != 1 || events[0].Outcome != model.DeliveryFailed || events[0].Reason != "presign_failed" {
		t.Fatalf("expected a FAILED audit event, got %+v", events)
	}

	e.storage.RangeErr = errors.New("get object range: provider error InternalError")
	if _, err := e.svc.Open(context.Background(), owner, "a1", DownloadRequest{}); err != ErrStorageUnavailable {
		t.Fatalf("expected ErrStorageUnavailable for the proxy, got %v", err)
	}
}

func TestURLIsWithheldWhenTheAuditCannotBeWritten(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	e.audit.Err = errors.New("database unavailable")

	got, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{})
	if !errors.Is(err, ErrAuditUnavailable) || got.URL != "" {
		t.Fatalf("expected no URL without an audit record, got %q, %v", got.URL, err)
	}
}

func TestAuditNeverContainsTheURL(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	got, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{})
	if err != nil {
		t.Fatal(err)
	}
	event := e.audit.Events()[0]
	if event.Outcome != model.DeliveryAllowed || event.AssetID != "a1" || event.PrincipalID != "user-1" ||
		event.TenantID != "tenant-1" || event.Operation != model.DeliveryOpDownloadURL {
		t.Fatalf("unexpected audit event %+v", event)
	}
	if strings.Contains(event.Reason, got.URL) || strings.Contains(event.Reason, "Signature") {
		t.Fatal("the audit must never hold the URL")
	}
}

func TestAvailableAssetAtAnUnexpectedKeyIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	asset := model.Asset{
		ID: "a1", TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-1", ObjectKey: "assets/tenant-2/a1",
		FileName: "f", ContentType: "application/pdf", SizeBytes: 20, Status: model.AssetStatusAvailable,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	if err := e.assets.Create(context.Background(), &asset); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{}); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestDeletionTakesEffectOnTheNextRequest(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	if _, err := e.svc.Metadata(context.Background(), owner, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := e.assets.TransitionStatus(context.Background(), "tenant-1", "a1",
		[]model.AssetStatus{model.AssetStatusAvailable}, model.AssetStatusDeleted, "", "", baseTime); err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted asset must not get a URL, got %v", err)
	}
	if _, err := e.svc.Open(context.Background(), owner, "a1", DownloadRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted asset must not be streamed, got %v", err)
	}
}

// recordingSigner signs by appending a marker, recording what it signed.
type recordingSigner struct{ signed []string }

func (r *recordingSigner) SignURL(raw string, expires time.Time) (string, error) {
	r.signed = append(r.signed, raw)
	return raw + "?Signature=test", nil
}

func TestCDNDeliveryIsAlwaysSigned(t *testing.T) {
	signer := &recordingSigner{}
	e := newEnv(t, &CDNConfig{BaseURL: "https://cdn.example.test", Signer: signer})
	e.asset(t, "a1", model.AssetStatusAvailable)

	got, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Via != "cdn" || len(signer.signed) != 1 || signer.signed[0] != "https://cdn.example.test/assets/tenant-1/a1" {
		t.Fatalf("unexpected CDN URL %+v (signed %v)", got, signer.signed)
	}
	if len(e.storage.Presigns) != 0 {
		t.Fatal("CDN delivery must not presign storage URLs")
	}

	for _, bad := range []*CDNConfig{
		{BaseURL: "https://cdn.example.test"},                // unsigned
		{BaseURL: "http://cdn.example.test", Signer: signer}, // not https
		{BaseURL: "https://cdn.example.test?x=1", Signer: signer},
	} {
		if _, err := NewService(e.assets, e.storage, e.audit, nil, Config{
			Bucket: "b", Prefix: "assets", DefaultTTL: time.Minute, MaxTTL: time.Minute, CDN: bad,
		}, quietLog); err == nil {
			t.Fatalf("CDN config %+v must be refused", bad)
		}
	}
}

func TestProxiedRangesAndCancellation(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	read := func(header string) (Download, string, error) {
		d, err := e.svc.Open(context.Background(), owner, "a1", DownloadRequest{Range: header})
		if err != nil {
			return d, "", err
		}
		defer d.Body.Close()
		b, _ := io.ReadAll(d.Body)
		return d, string(b), nil
	}

	if d, body, err := read(""); err != nil || d.Partial || body != string(content) {
		t.Fatalf("full: %+v %q %v", d, body, err)
	}
	if d, body, err := read("bytes=2-5"); err != nil || !d.Partial || body != "2345" || d.First != 2 || d.Last != 5 {
		t.Fatalf("range: %+v %q %v", d, body, err)
	}
	if _, body, _ := read("bytes=-3"); body != "hij" {
		t.Fatalf("suffix: %q", body)
	}
	if d, _, err := read("bytes=50-"); !errors.Is(err, ErrRangeNotSatisfiable) || d.TotalSize != 20 {
		t.Fatalf("expected 416 data, got %+v %v", d, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must reach storage, got %v", err)
	}
}

func TestParseRange(t *testing.T) {
	const size = 100
	cases := []struct {
		header      string
		first, last int64
		partial     bool
		err         error
	}{
		{"", 0, 99, false, nil},
		{"bytes=0-0", 0, 0, true, nil},
		{"bytes=10-19", 10, 19, true, nil},
		{"bytes=90-", 90, 99, true, nil},
		{"bytes=90-500", 90, 99, true, nil},
		{"bytes=-10", 90, 99, true, nil},
		{"bytes=-500", 0, 99, true, nil},
		{"bytes=100-", 0, 0, false, ErrRangeNotSatisfiable},
		{"bytes=-0", 0, 0, false, ErrRangeNotSatisfiable},
		{"bytes=0-1,5-6", 0, 99, false, nil}, // multiple ranges: whole object
		{"bytes=9-1", 0, 99, false, nil},     // malformed: ignored
		{"items=0-1", 0, 99, false, nil},
		{"bytes=abc", 0, 99, false, nil},
		{"bytes=+5-9", 0, 99, false, nil},  // signs are not digits
		{"bytes=-", 0, 99, false, nil},     // no bounds at all
		{"bytes= 5 - 9 ", 5, 9, true, nil}, // optional whitespace
		{"bytes=99999999999999999999-", 0, 99, false, nil},
	}
	for _, c := range cases {
		first, last, partial, err := parseRange(c.header, size)
		if first != c.first || last != c.last || partial != c.partial || !errors.Is(err, c.err) {
			t.Errorf("%q: got %d-%d partial=%v err=%v", c.header, first, last, partial, err)
		}
	}

	// An empty object has no satisfiable range, only the whole (empty) body.
	for _, header := range []string{"bytes=0-", "bytes=0-0", "bytes=-1"} {
		if _, _, _, err := parseRange(header, 0); !errors.Is(err, ErrRangeNotSatisfiable) {
			t.Errorf("empty object %q: got %v", header, err)
		}
	}
	if first, last, partial, err := parseRange("", 0); err != nil || partial || last-first+1 != 0 {
		t.Errorf("empty object, no range: %d-%d %v %v", first, last, partial, err)
	}
}
