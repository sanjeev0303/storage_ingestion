package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// setQuota replaces the tenant's overrides, as a platform administrator.
func setQuota(t *testing.T, s *securityStack, tenantID string, q model.TenantQuota) {
	t.Helper()
	if _, err := s.quotas.Set(context.Background(), operator, tenantID, q); err != nil {
		t.Fatalf("set quota: %v", err)
	}
}

func usageOf(t *testing.T, s *securityStack, tenantID string) model.StorageUsage {
	t.Helper()
	u, err := s.quotaRepo.Usage(context.Background(), tenantID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func quotaState(t *testing.T, s *securityStack, uploadID string) string {
	t.Helper()
	var state string
	if err := s.db.Pool.QueryRow(context.Background(), `SELECT quota_state FROM upload_sessions WHERE id = $1`, uploadID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// race runs n calls of fn at once and sorts the results: successes, and
// quota refusals by limit. Anything else fails the test.
func race(t *testing.T, n int, fn func(i int) error) (int, map[string][]*model.QuotaError) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}()
	}
	close(start)
	wg.Wait()
	ok, refused := 0, map[string][]*model.QuotaError{}
	for _, err := range errs {
		var qe *model.QuotaError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &qe):
			refused[qe.Limit] = append(refused[qe.Limit], qe)
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	return ok, refused
}

func TestQuotaReservationsHoldUnderConcurrency(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "quota")
	dev := acme.developer

	// Twenty uploads at once against five upload slots.
	setQuota(t, s, acme.org.ID, model.TenantQuota{MaxActiveUploads: 5, MaxActiveMultipart: 2})
	ok, refused := race(t, 20, func(i int) error {
		_, err := s.ie.single.CreateFor(ctx, dev, uploadRequest(fmt.Sprintf("slot-%d", i), 100))
		return err
	})
	if ok != 5 || len(refused["active_uploads"]) != 15 {
		t.Fatalf("expected 5 reservations and 15 refusals, got %d and %v", ok, refused)
	}
	if qe := refused["active_uploads"][0]; qe.Max != 5 || qe.Used != 5 || qe.Remaining() != 0 {
		t.Fatalf("the refusal reports the limit: %+v", qe)
	}
	u := usageOf(t, s, acme.org.ID)
	if u.ReservedUploads != 5 || u.ReservedBytes != 500 || u.ActiveUploads != 5 {
		t.Fatalf("usage after the race: %+v", u)
	}

	// Storage bytes: 500 reserved of 1000, ten uploads of 200 bytes at
	// once; exactly two fit.
	setQuota(t, s, acme.org.ID, model.TenantQuota{MaxActiveUploads: 100, MaxActiveMultipart: 2, MaxStorageBytes: 1000, MaxObjectBytes: 1000})
	ok, refused = race(t, 10, func(i int) error {
		_, err := s.ie.single.CreateFor(ctx, dev, uploadRequest(fmt.Sprintf("bytes-%d", i), 200))
		return err
	})
	if ok != 2 || len(refused["storage_bytes"]) != 8 {
		t.Fatalf("expected 2 reservations and 8 refusals, got %d and %v", ok, refused)
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedBytes != 900 {
		t.Fatalf("reserved bytes may never pass the limit: %+v", u)
	}

	// One object above the per-object limit is refused outright.
	setQuota(t, s, acme.org.ID, model.TenantQuota{MaxActiveUploads: 100, MaxActiveMultipart: 2, MaxObjectBytes: 150})
	_, err := s.ie.single.CreateFor(ctx, dev, uploadRequest("too-big", 151))
	var qe *model.QuotaError
	if !errors.As(err, &qe) || qe.Limit != "max_object_bytes" || qe.Requested != 151 {
		t.Fatalf("object limit: %v", err)
	}

	// Multipart sessions have their own, smaller limit.
	ok, refused = race(t, 6, func(i int) error {
		name := fmt.Sprintf("multi-%d", i)
		_, err := s.ie.multi.StartFor(ctx, dev, uploadservice.StartMultipartInput{
			FileName: name + ".txt", ContentType: "text/plain", SizeBytes: 100, ChecksumSHA256: sha256Hex(name), IdempotencyKey: name,
		})
		return err
	})
	if ok != 2 || len(refused["active_multipart"]) != 4 {
		t.Fatalf("expected 2 multipart sessions, got %d and %v", ok, refused)
	}

	// Refusals are audited, with the limit and the numbers.
	exceeded := s.auditEvents(t, acme.org.ID, "quota.exceeded")
	if len(exceeded) != 15+8+1+4 || exceeded[0].Details["limit"] != "active_uploads" || exceeded[0].Outcome != model.AuditDenied {
		t.Fatalf("quota refusals audited: %d %+v", len(exceeded), exceeded[0])
	}
}

func TestQuotaIsReleasedOnAbortAndFailureAndConsumedOnSuccess(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "release")
	dev := acme.developer

	// Aborting a multipart upload gives its reservation back.
	started, err := s.ie.multi.StartFor(ctx, dev, uploadservice.StartMultipartInput{
		FileName: "big.txt", ContentType: "text/plain", SizeBytes: 1000, ChecksumSHA256: sha256Hex("big"), IdempotencyKey: "big",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedBytes != 1000 || u.ReservedUploads != 1 {
		t.Fatalf("after start: %+v", u)
	}
	if _, err := s.ie.multi.AbortFor(ctx, dev, started.Session.ID); err != nil {
		t.Fatal(err)
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedBytes != 0 || u.ReservedUploads != 0 || quotaState(t, s, started.Session.ID) != "RELEASED" {
		t.Fatalf("after abort: %+v", u)
	}

	// An upload whose object does not match fails, and is released.
	short := s.upload(t, dev, "short", textData("short", 40))
	if _, err := s.db.Pool.Exec(ctx, `UPDATE upload_sessions SET expected_size = 100, reserved_bytes = 100 WHERE id = $1`, short.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Pool.Exec(ctx, `UPDATE tenant_usage SET reserved_bytes = reserved_bytes + 60 WHERE tenant_id = $1`, acme.org.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ie.single.CompleteFor(ctx, dev, short.ID); !errors.Is(err, uploadservice.ErrObjectSizeMismatch) {
		t.Fatalf("size mismatch: %v", err)
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedBytes != 0 || quotaState(t, s, short.ID) != "RELEASED" {
		t.Fatalf("after a failed completion: %+v", u)
	}

	// A file processing rejects (here: malware) is released too.
	infected := infectedFile("bad.txt").data
	bad := s.upload(t, dev, "bad", infected)
	if _, err := s.ie.single.CompleteFor(ctx, dev, bad.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ie.proc.Intake(ctx, acme.org.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	if asset, err := s.ie.assets.GetByUpload(ctx, acme.org.ID, bad.ID); err != nil || asset.Status != model.AssetStatusRejected {
		t.Fatalf("infected upload: %+v %v", asset, err)
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedBytes != 0 || u.StoredBytes != 0 || quotaState(t, s, bad.ID) != "RELEASED" {
		t.Fatalf("after rejection: %+v", u)
	}

	// A published file turns its reservation into stored bytes, once,
	// however often the request is retried.
	data := textData("good", 300)
	if _, err := s.ie.single.CreateFor(ctx, dev, uploadOf("good", data)); err != nil {
		t.Fatal(err)
	}
	again, err := s.ie.single.CreateFor(ctx, dev, uploadOf("good", data))
	if err != nil || !again.Reused {
		t.Fatalf("an idempotent retry reuses the upload: %+v %v", again, err)
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedBytes != 300 || u.ReservedUploads != 1 {
		t.Fatalf("a retry must not reserve twice: %+v", u)
	}
	s.ie.storage.PutObject(again.Upload.ObjectKey, data)
	if _, err := s.ie.single.CompleteFor(ctx, dev, again.Upload.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ie.proc.Intake(ctx, acme.org.ID, again.Upload.ID); err != nil {
		t.Fatal(err)
	}
	if asset, err := s.ie.assets.GetByUpload(ctx, acme.org.ID, again.Upload.ID); err != nil || asset.Status != model.AssetStatusAvailable {
		t.Fatalf("published upload: %+v %v", asset, err)
	}
	u := usageOf(t, s, acme.org.ID)
	if u.ReservedBytes != 0 || u.ReservedUploads != 0 || u.StoredBytes != 300 || u.AssetCount != 1 || quotaState(t, s, again.Upload.ID) != "CONSUMED" {
		t.Fatalf("after publication: %+v", u)
	}
}

func TestQuotaReconciliationRepairsUsage(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "recon")
	dev := acme.developer

	published := s.availableAsset(t, dev, "kept", textData("kept", 50))
	crashed := s.upload(t, dev, "crashed", textData("crashed", 70))
	expired := s.upload(t, dev, "expired", textData("expired", 80))
	s.upload(t, dev, "live", textData("live", 90))

	// A crash between a session's end and its release, a session left
	// past its expiry, and counters that drifted.
	for _, q := range []string{
		`UPDATE upload_sessions SET status = 'FAILED', failure_reason = 'object_size_mismatch' WHERE id = '` + crashed.ID + `'`,
		`UPDATE upload_sessions SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 minute' WHERE id = '` + expired.ID + `'`,
		`UPDATE tenant_usage SET reserved_bytes = 999999, reserved_uploads = 42, stored_bytes = 0, asset_count = 7 WHERE tenant_id = '` + acme.org.ID + `'`,
	} {
		if _, err := s.db.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.quotas.AdminReconcile(ctx, acme.owner, acme.org.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("tenants do not reconcile their own usage: %v", err)
	}
	result, err := s.quotas.Reconcile(ctx, acme.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := model.StorageUsage{TenantID: acme.org.ID, ReservedBytes: 90, ReservedUploads: 1, StoredBytes: published.SizeBytes, AssetCount: 1}
	if result.Settled != 2 || !result.Repaired || result.After != want || result.Before.ReservedBytes != 999999 {
		t.Fatalf("reconcile: %+v", result)
	}
	if quotaState(t, s, crashed.ID) != "RELEASED" || quotaState(t, s, expired.ID) != "RELEASED" {
		t.Fatal("stale reservations are released")
	}
	var status string
	if err := s.db.Pool.QueryRow(ctx, `SELECT status FROM upload_sessions WHERE id = $1`, expired.ID).Scan(&status); err != nil || status != "EXPIRED" {
		t.Fatalf("an upload past its expiry is expired: %s %v", status, err)
	}

	// Reconciling again changes nothing, and is idempotent.
	again, err := s.quotas.Reconcile(ctx, acme.org.ID)
	if err != nil || again.Settled != 0 || again.Repaired || again.After != want {
		t.Fatalf("second reconcile: %+v %v", again, err)
	}
	if got := s.auditEvents(t, acme.org.ID, "quota.reconcile"); len(got) != 1 || got[0].ActorType != model.ActorService {
		t.Fatalf("a repair is audited once: %+v", got)
	}
}
