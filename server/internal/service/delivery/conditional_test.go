package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func readAll(t *testing.T, d Download) string {
	t.Helper()
	if d.Body == nil {
		return ""
	}
	defer d.Body.Close()
	b, err := io.ReadAll(d.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *env) lastEvent(t *testing.T) model.DeliveryAuditEvent {
	t.Helper()
	events := e.audit.Events()
	if len(events) == 0 {
		t.Fatal("no audit events")
	}
	return events[len(events)-1]
}

func TestValidatorsAndConditionalRequests(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	etag := `"` + contentSum + `"`
	modified := baseTime.Format(http.TimeFormat)
	earlier := baseTime.Add(-time.Hour).Format(http.TimeFormat)
	open := func(req DownloadRequest) (Download, string, error) {
		d, err := e.svc.Open(context.Background(), owner, "a1", req)
		if err != nil {
			return d, "", err
		}
		return d, readAll(t, d), nil
	}

	d, body, err := open(DownloadRequest{})
	if err != nil || d.ETag != etag || !d.LastModified.Equal(baseTime) || body != string(content) {
		t.Fatalf("validators: %+v %v", d, err)
	}

	cases := []struct {
		name    string
		req     DownloadRequest
		want    string // body, "304" or an error
		partial bool
	}{
		{"if-none-match hit", DownloadRequest{IfNoneMatch: etag}, "304", false},
		{"if-none-match weak hit", DownloadRequest{IfNoneMatch: `"x", W/` + etag}, "304", false},
		{"if-none-match star", DownloadRequest{IfNoneMatch: "*"}, "304", false},
		{"if-none-match miss", DownloadRequest{IfNoneMatch: `"other"`}, string(content), false},
		{"if-match hit", DownloadRequest{IfMatch: etag, Range: "bytes=0-3"}, "0123", true},
		{"if-match miss", DownloadRequest{IfMatch: `"other"`}, "412", false},
		{"if-match weak never matches", DownloadRequest{IfMatch: "W/" + etag}, "412", false},
		{"if-range etag hit resumes", DownloadRequest{Range: "bytes=10-", IfRange: etag}, "abcdefghij", true},
		{"if-range etag miss sends all", DownloadRequest{Range: "bytes=10-", IfRange: `"stale"`}, string(content), false},
		{"if-range weak sends all", DownloadRequest{Range: "bytes=10-", IfRange: "W/" + etag}, string(content), false},
		{"if-range date hit", DownloadRequest{Range: "bytes=-5", IfRange: modified}, "fghij", true},
		{"if-range old date sends all", DownloadRequest{Range: "bytes=-5", IfRange: baseTime.Add(-time.Hour).Format(http.TimeFormat)}, string(content), false},
		{"if-modified-since not modified", DownloadRequest{IfModifiedSince: modified}, "304", false},
		{"if-modified-since modified", DownloadRequest{IfModifiedSince: earlier}, string(content), false},
		{"if-modified-since bad date ignored", DownloadRequest{IfModifiedSince: "yesterday"}, string(content), false},
		{"if-none-match wins over if-modified-since", DownloadRequest{IfNoneMatch: `"other"`, IfModifiedSince: modified}, string(content), false},
		{"if-unmodified-since passes", DownloadRequest{IfUnmodifiedSince: modified, Range: "bytes=0-1"}, "01", true},
		{"if-unmodified-since fails", DownloadRequest{IfUnmodifiedSince: earlier}, "412", false},
		{"if-match wins over if-unmodified-since", DownloadRequest{IfMatch: etag, IfUnmodifiedSince: earlier}, string(content), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, body, err := open(c.req)
			switch c.want {
			case "304":
				if err != nil || !d.NotModified || d.Body != nil {
					t.Fatalf("expected not modified, got %+v %v", d, err)
				}
			case "412":
				if !errors.Is(err, ErrPreconditionFailed) {
					t.Fatalf("expected 412, got %v", err)
				}
			default:
				if err != nil || body != c.want || d.Partial != c.partial {
					t.Fatalf("got %q partial=%v err=%v", body, d.Partial, err)
				}
			}
		})
	}
}

func TestReadsArePinnedToThePublishedVersion(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	// Someone overwrites the stored object out of band.
	e.storage.PutObject("assets/tenant-1/a1", []byte("ABCDEFGHIJKLMNOPQRST"))

	for _, req := range []DownloadRequest{{}, {Range: "bytes=10-"}, {Range: "bytes=10-", IfRange: `"` + contentSum + `"`}, {HeadOnly: true}} {
		d, err := e.svc.Open(context.Background(), owner, "a1", req)
		if !errors.Is(err, ErrVersionMismatch) || d.Body != nil {
			t.Fatalf("%+v: bytes of another version must never be served, got %v", req, err)
		}
		if event := e.lastEvent(t); event.Outcome != model.DeliveryFailed || event.Reason != "version_mismatch" {
			t.Fatalf("expected a version_mismatch audit, got %+v", event)
		}
	}
}

func TestAssetsWithoutRecordedVersionUseAFreshHead(t *testing.T) {
	e := newEnv(t, nil)
	e.storage.PutObject("assets/tenant-1/old", content)
	asset := model.Asset{
		ID: "old", TenantID: "tenant-1", OwnerID: "user-1", UploadID: "up-old", ObjectKey: "assets/tenant-1/old",
		FileName: "old.pdf", ContentType: "application/pdf", SizeBytes: int64(len(content)), ChecksumSHA256: contentSum,
		Status: model.AssetStatusAvailable, CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	if err := e.assets.Create(context.Background(), &asset); err != nil {
		t.Fatal(err)
	}

	d, err := e.svc.Open(context.Background(), owner, "old", DownloadRequest{Range: "bytes=0-4"})
	if err != nil || readAll(t, d) != "01234" {
		t.Fatalf("a matching legacy object must be served: %v", err)
	}

	e.storage.PutObject("assets/tenant-1/old", []byte("short"))
	if _, err := e.svc.Open(context.Background(), owner, "old", DownloadRequest{}); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("a legacy object of the wrong size must be refused, got %v", err)
	}
}

func TestHeadReadsNoBytesAndIsNotAuditedAsADownload(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	d, err := e.svc.Open(context.Background(), owner, "a1", DownloadRequest{HeadOnly: true, Range: "bytes=5-9"})
	if err != nil || d.Body != nil || !d.Partial || d.Length() != 5 || d.TotalSize != 20 {
		t.Fatalf("HEAD: %+v %v", d, err)
	}
	if e.storage.RangeCalls != 0 {
		t.Fatal("HEAD must not read the object")
	}
	if len(e.audit.Events()) != 0 {
		t.Fatalf("HEAD delivers no content and is not audited as a download: %+v", e.audit.Events())
	}
}

// deletingStorage deletes the asset just as its bytes are requested, as a
// concurrent deletion would.
type deletingStorage struct {
	*storagememory.ObjectStorage
	assets interface {
		TransitionStatus(context.Context, string, string, []model.AssetStatus, model.AssetStatus, string, string, time.Time) error
	}
}

func (d deletingStorage) OpenRange(ctx context.Context, bucket, key string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error) {
	_ = d.assets.TransitionStatus(ctx, "tenant-1", "a1", []model.AssetStatus{model.AssetStatusAvailable}, model.AssetStatusDeleted, "", "", baseTime)
	return nil, fmt.Errorf("get object: %w", interfaces.ErrObjectNotFound)
}

func TestMissingObjectAndConcurrentDeletion(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	if err := e.storage.DeleteObject(context.Background(), "", "assets/tenant-1/a1"); err != nil {
		t.Fatal(err)
	}
	// The asset is still AVAILABLE but its object is gone: an incident, not
	// a "not found" for the owner.
	if _, err := e.svc.Open(context.Background(), owner, "a1", DownloadRequest{}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("expected ErrStorageUnavailable, got %v", err)
	}
	if event := e.lastEvent(t); event.Reason != "object_missing" {
		t.Fatalf("expected object_missing, got %+v", event)
	}

	e2 := newEnv(t, nil)
	e2.asset(t, "a1", model.AssetStatusAvailable)
	svc, err := NewService(e2.assets, deletingStorage{e2.storage, e2.assets}, e2.audit, nil, Config{
		Bucket: "private-bucket", Prefix: "assets", DefaultTTL: time.Minute, MaxTTL: time.Minute,
	}, quietLog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Open(context.Background(), owner, "a1", DownloadRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a concurrent deletion must answer like a deleted asset, got %v", err)
	}
}

func TestStorageThrottlingAndFailures(t *testing.T) {
	cases := []struct {
		inject error
		want   error
		reason string
	}{
		{fmt.Errorf("get: SlowDown: %w", interfaces.ErrStorageThrottled), ErrStorageBusy, "throttled"},
		{fmt.Errorf("get: %w", interfaces.ErrStorageAccessDenied), ErrStorageUnavailable, "storage_error"},
		{errors.New("get: connection reset"), ErrStorageUnavailable, "storage_error"},
		{fmt.Errorf("get: %w", interfaces.ErrPreconditionFailed), ErrVersionMismatch, "version_mismatch"},
	}
	for _, c := range cases {
		e := newEnv(t, nil)
		e.asset(t, "a1", model.AssetStatusAvailable)
		e.storage.RangeErr = c.inject
		_, err := e.svc.Open(context.Background(), owner, "a1", DownloadRequest{})
		if !errors.Is(err, c.want) {
			t.Fatalf("%v: got %v, want %v", c.inject, err, c.want)
		}
		if event := e.lastEvent(t); event.Outcome != model.DeliveryFailed || event.Reason != c.reason {
			t.Fatalf("%v: audit %+v", c.inject, event)
		}
	}
}

func TestDeniedAccessIsAuditedWithItsReason(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "q1", model.AssetStatusQuarantined)
	e.asset(t, "p1", model.AssetStatusProcessing)
	e.asset(t, "r1", model.AssetStatusRejected)

	for id, reason := range map[string]string{"q1": "quarantined", "p1": "processing", "r1": "rejected"} {
		_, err := e.svc.Open(context.Background(), owner, id, DownloadRequest{RequestID: "req-" + id})
		if !errors.Is(err, ErrNotAvailable) {
			t.Fatalf("%s: %v", id, err)
		}
		event := e.lastEvent(t)
		if event.Outcome != model.DeliveryDenied || event.Reason != reason || event.RequestID != "req-"+id {
			t.Fatalf("%s: audit %+v", id, event)
		}
	}
}

func TestPartialDownloadsAreAuditedWithTheirRange(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	d, err := e.svc.Open(context.Background(), owner, "a1", DownloadRequest{Range: "bytes=5-9", RequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, d)
	event := e.lastEvent(t)
	if event.Outcome != model.DeliveryAllowed || event.ByteRange != "5-9/20" || event.RequestID != "req-1" ||
		event.Operation != model.DeliveryOpProxy {
		t.Fatalf("audit %+v", event)
	}

	e.svc.TransferFailed(context.Background(), d, 3, "client_disconnected")
	failed := e.lastEvent(t)
	if failed.Outcome != model.DeliveryFailed || failed.BytesSent != 3 || failed.Reason != "client_disconnected" {
		t.Fatalf("transfer failure audit %+v", failed)
	}

	// The audit write survives a request context that is already gone.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(e.audit.Events())
	e.svc.TransferFailed(ctx, d, 4, "client_disconnected")
	if len(e.audit.Events()) != before+1 {
		t.Fatal("a transfer failure must be audited after the client left")
	}
}

func TestTokenLinks(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)

	link, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{Mode: ModeToken, Filename: "q3.pdf", TTL: time.Minute})
	if err != nil || link.Via != ModeToken || !link.Revocable || !strings.HasPrefix(link.URL, TokenPathPrefix) {
		t.Fatalf("token link: %+v %v", link, err)
	}
	raw := strings.TrimPrefix(link.URL, TokenPathPrefix)

	stored := e.tokens.Tokens()
	if len(stored) != 1 || stored[0].TokenHash == raw || strings.Contains(fmt.Sprintf("%+v", stored[0]), raw) {
		t.Fatal("only the token's hash may be stored")
	}
	if len(e.storage.Presigns) != 0 {
		t.Fatal("a token link must not presign a storage URL")
	}

	d, err := e.svc.OpenToken(context.Background(), raw, DownloadRequest{Range: "bytes=0-3"})
	if err != nil || readAll(t, d) != "0123" || !strings.Contains(d.ContentDisposition, "q3.pdf") {
		t.Fatalf("open token: %+v %v", d, err)
	}
	if event := e.lastEvent(t); event.Operation != model.DeliveryOpToken || event.PrincipalID != "user-1" {
		t.Fatalf("token download audit %+v", event)
	}

	for _, bad := range []string{"", "short", strings.Repeat("A", 43), raw + "x", "../../etc/passwd"} {
		if _, err := e.svc.OpenToken(context.Background(), bad, DownloadRequest{}); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("%q: expected ErrTokenInvalid, got %v", bad, err)
		}
	}

	// Another tenant cannot revoke, and the link keeps working.
	if _, err := e.svc.RevokeLinks(context.Background(), member("tenant-2", "user-1", model.RoleOwner), "a1", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant revoke: %v", err)
	}
	revoked, err := e.svc.RevokeLinks(context.Background(), owner, "a1", "")
	if err != nil || revoked != 1 {
		t.Fatalf("revoke: %d %v", revoked, err)
	}
	if _, err := e.svc.OpenToken(context.Background(), raw, DownloadRequest{}); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a revoked link must stop at once, got %v", err)
	}
	if event := e.lastEvent(t); event.Outcome != model.DeliveryDenied || event.Reason != "token_revoked" {
		t.Fatalf("revoked use audit %+v", event)
	}
}

func TestTokenLinksExpireAndFollowTheAsset(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	link, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{Mode: ModeToken, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimPrefix(link.URL, TokenPathPrefix)

	// Deleting the asset ends the link even before it expires.
	if err := e.assets.TransitionStatus(context.Background(), "tenant-1", "a1",
		[]model.AssetStatus{model.AssetStatusAvailable}, model.AssetStatusDeleted, "", "", baseTime); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.OpenToken(context.Background(), raw, DownloadRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted asset's link must not work, got %v", err)
	}

	e2 := newEnv(t, nil)
	e2.asset(t, "a1", model.AssetStatusAvailable)
	link, err = e2.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{Mode: ModeToken, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	e2.svc.now = func() time.Time { return baseTime.Add(time.Minute) }
	if _, err := e2.svc.OpenToken(context.Background(), strings.TrimPrefix(link.URL, TokenPathPrefix), DownloadRequest{}); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("an expired link must not work, got %v", err)
	}
	if event := e2.lastEvent(t); event.Reason != "token_expired" {
		t.Fatalf("expired use audit %+v", event)
	}
}

func TestURLModeValidation(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	if _, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{Mode: "public"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown mode: %v", err)
	}

	noTokens, err := NewService(e.assets, e.storage, e.audit, nil, Config{
		Bucket: "private-bucket", Prefix: "assets", DefaultTTL: time.Minute, MaxTTL: time.Minute,
	}, quietLog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noTokens.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{Mode: ModeToken}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("token mode without a repository: %v", err)
	}
	if _, err := NewService(e.assets, e.storage, e.audit, nil, Config{
		Bucket: "b", Prefix: "assets", DefaultTTL: time.Minute, MaxTTL: time.Minute, URLMode: ModeToken,
	}, quietLog); err == nil {
		t.Fatal("token as the default mode needs a token repository")
	}

	// Presigned URLs are pinned to the published version where there is one.
	e.storage.Presigns = nil
	if _, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := e.storage.Presigns[0].Options; got.ContentType != "application/pdf" || got.VersionID != "" {
		t.Fatalf("presign options %+v", got)
	}
}

func TestSafeContentType(t *testing.T) {
	cases := map[string]string{
		"application/pdf":                  "application/pdf",
		"image/png; name=evil":             "image/png",
		"TEXT/PLAIN; charset=utf-8":        "text/plain",
		"text/html":                        "application/octet-stream",
		"image/svg+xml":                    "application/octet-stream",
		"application/javascript":           "application/octet-stream",
		"text/plain\r\nSet-Cookie: x=1":    "application/octet-stream",
		"":                                 "application/octet-stream",
		"not a type":                       "application/octet-stream",
		"application/xhtml+xml; charset=x": "application/octet-stream",
	}
	for in, want := range cases {
		if got := SafeContentType(in); got != want {
			t.Errorf("SafeContentType(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SafeFilename("invoice\u202efdp.exe"); strings.ContainsRune(got, '\u202e') {
		t.Fatalf("bidi override kept: %q", got)
	}
	if got := attachment("a\r\nb.pdf", ""); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("CRLF in Content-Disposition: %q", got)
	}
}

func TestMetadataReportsDeliverability(t *testing.T) {
	e := newEnv(t, nil)
	e.asset(t, "a1", model.AssetStatusAvailable)
	e.asset(t, "p1", model.AssetStatusProcessing)

	info, err := e.svc.Metadata(context.Background(), owner, "a1")
	if err != nil || !info.Deliverable || info.ETag != `"`+contentSum+`"` || info.PublishedAt == nil {
		t.Fatalf("available: %+v %v", info, err)
	}
	info, err = e.svc.Metadata(context.Background(), owner, "p1")
	if err != nil || info.Deliverable || info.ETag != "" {
		t.Fatalf("processing: %+v %v", info, err)
	}
}
