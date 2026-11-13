package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// A batch reservation (bulk ingestion's claims) stores its sessions and their
// quota in one transaction, applies each limit counting the sessions before
// it, and is all or nothing; a Create afterwards finds its session again.
func TestBatchReservationIsAllOrNothingAndCreateFindsItsSessions(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "batch")
	dev := acme.developer
	inputs := func(prefix string, n int, size int64) []uploadservice.CreateInput {
		out := make([]uploadservice.CreateInput, n)
		for i := range out {
			out[i] = uploadservice.CreateInput{TenantID: dev.TenantID, OwnerID: dev.UserID,
				Request: uploadRequest(fmt.Sprintf("%s-%d", prefix, i), size)}
		}
		return out
	}

	// Three sessions in one go: their bytes and count reserved together.
	setQuota(t, s, acme.org.ID, model.TenantQuota{MaxActiveUploads: 5, MaxActiveMultipart: 2})
	if err := s.ie.single.Reserve(ctx, inputs("ok", 3, 100)); err != nil {
		t.Fatalf("reserve a batch: %v", err)
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedUploads != 3 || u.ReservedBytes != 300 || u.ActiveUploads != 3 {
		t.Fatalf("usage after the batch: %+v", u)
	}
	// The same requests afterwards are replays: no second reservation.
	for i := 0; i < 3; i++ {
		res, err := s.ie.single.CreateFor(ctx, dev, uploadRequest(fmt.Sprintf("ok-%d", i), 100))
		if err != nil || !res.Reused || res.Plan.UploadURL == "" {
			t.Fatalf("create after the batch: reused=%v err=%v", res.Reused, err)
		}
		if state := quotaState(t, s, res.Upload.ID); state != "RESERVED" {
			t.Fatalf("the batch's session holds its reservation: %s", state)
		}
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedUploads != 3 || u.ReservedBytes != 300 {
		t.Fatalf("a replay reserved again: %+v", u)
	}

	// Two slots are left: a batch of three stores nothing at all.
	err := s.ie.single.Reserve(ctx, inputs("over", 3, 100))
	if !errors.Is(err, model.ErrBatchNotReserved) {
		t.Fatalf("a batch past the limit: %v", err)
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedUploads != 3 || u.ReservedBytes != 300 || u.ActiveUploads != 3 {
		t.Fatalf("a refused batch left usage behind: %+v", u)
	}
	// One at a time, as the caller falls back to, the first two fit.
	for i := 0; i < 3; i++ {
		_, err := s.ie.single.CreateFor(ctx, dev, uploadRequest(fmt.Sprintf("over-%d", i), 100))
		var qe *model.QuotaError
		if i < 2 && err != nil || i == 2 && !(errors.As(err, &qe) && qe.Limit == "active_uploads") {
			t.Fatalf("one at a time, upload %d: %v", i, err)
		}
	}

	// A key already taken rolls the whole batch back.
	setQuota(t, s, acme.org.ID, model.TenantQuota{MaxActiveUploads: 100, MaxActiveMultipart: 2})
	taken := append(inputs("fresh", 2, 10), inputs("ok", 1, 100)...)
	if err := s.ie.single.Reserve(ctx, taken); err == nil {
		t.Fatal("a batch with a taken idempotency key was stored")
	}
	if u := usageOf(t, s, acme.org.ID); u.ReservedUploads != 5 || u.ReservedBytes != 500 {
		t.Fatalf("a rolled-back batch left usage behind: %+v", u)
	}

	// Two sessions of one path are judged one at a time instead.
	same := inputs("same", 2, 10)
	same[0].Request.Path, same[1].Request.Path = "dup/file.txt", "dup/file.txt"
	if err := s.ie.single.Reserve(ctx, same); !errors.Is(err, model.ErrBatchNotReserved) {
		t.Fatalf("a batch with one path twice: %v", err)
	}
}

// A claim reserves its grants' quota in one batch, and those sessions then
// upload and publish like any other.
func TestIngestionClaimReservesItsGrantsInOneBatch(t *testing.T) {
	s := openSecurityStack(t, nil)
	e := s.ie
	files := []file{textFile("batch/a.txt", 120), textFile("batch/b.txt", 130), textFile("batch/c.txt", 140), textFile("batch/d.txt", 150)}
	d := e.dataset(t, "batch-claims", model.CompleteAll)
	job := e.readyJob(t, d.ID, files...)

	before := e.metrics.Snapshot()
	result := e.claim(t, job.ID, 10)
	after := e.metrics.Snapshot()
	if len(result.Grants) != len(files) || result.Failed != 0 {
		t.Fatalf("granted %d, failed %d", len(result.Grants), result.Failed)
	}
	if after["ingestion_batch_reservations"]-before["ingestion_batch_reservations"] != 1 ||
		after["ingestion_batch_reserved_sessions"]-before["ingestion_batch_reserved_sessions"] != int64(len(files)) ||
		after["ingestion_batch_reservations_skipped"] != before["ingestion_batch_reservations_skipped"] {
		t.Fatalf("the claim did not reserve in one batch: before %v after %v", before, after)
	}
	u := usageOf(t, s, e.p.TenantID)
	if u.ReservedUploads != int64(len(files)) || u.ReservedBytes != totalSize(files...) {
		t.Fatalf("usage after the claim: %+v", u)
	}
	for _, g := range result.Grants {
		if g.Entry.UploadID == "" || g.UploadURL == "" {
			t.Fatalf("grant %d has no session or URL: %+v", g.Entry.Position, g)
		}
		if state := quotaState(t, s, g.Entry.UploadID); state != "RESERVED" {
			t.Fatalf("grant %d: quota state %s", g.Entry.Position, state)
		}
	}

	data := contents(files...)
	for _, g := range result.Grants {
		e.process(t, e.upload(t, g, data[g.Entry.Path]))
	}
	if got := e.job(t, job.ID); got.State != model.JobCompleted {
		t.Fatalf("job state %s, counters %+v", got.State, got.Counters)
	}
	u = usageOf(t, s, e.p.TenantID)
	if u.ReservedUploads != 0 || u.ReservedBytes != 0 || u.AssetCount != int64(len(files)) || u.StoredBytes != totalSize(files...) {
		t.Fatalf("usage after publishing: %+v", u)
	}
}
