package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	eventdto "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	sqsqueue "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/queue/sqs"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Bulk ingestion on the local stack: PostgreSQL, MinIO, ClamAV and
// ElasticMQ. Bytes go to MinIO over presigned URLs, and every step after an
// upload runs from the Phase 6 event pipeline.

func openIngestionQueues(t *testing.T) *sqsqueue.Queue {
	t.Helper()
	cfg := sqsTestConfig(t, 5, true)
	queues := []string{eventdto.QueueProcessing, eventdto.QueueIngestion}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q, err := sqsqueue.New(ctx, cfg, queues...)
	if err != nil {
		t.Fatalf("connect to ElasticMQ: %v", err)
	}
	t.Cleanup(func() { deleteTestQueues(cfg, queues) })
	return q
}

// httpPut uploads data to a presigned URL and returns the ETag.
func httpPut(t *testing.T, url, contentType string, data []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT: HTTP %d: %s", resp.StatusCode, body)
	}
	return resp.Header.Get("ETag")
}

func TestLocalStackBulkIngestionThroughTheEventPipeline(t *testing.T) {
	stack := openContentStack(t)
	pipeline := openStackPipeline(t, stack)
	pipeline.queue = openIngestionQueues(t)
	proc := stack.service(t, serviceOptions{})
	(&ingestEnv{processingEnv: stack.env}).cleanupTenant(t, stack.env.tenant)
	ctx := context.Background()
	remove := func(key string) {
		t.Cleanup(func() { _ = stack.provider.DeleteObject(context.Background(), stack.bucket, key) })
	}

	svc, err := ingestion.NewService(ingestion.Dependencies{
		Repo:    postgres.NewIngestionRepository(stack.env.db),
		Uploads: uploadservice.NewService(stack.env.uploads, stack.provider, stack.bucket, 1<<30, time.Hour), Multipart: stack.multipart,
		Sessions: stack.env.uploads, Assets: stack.env.assets, Jobs: stack.env.jobs,
		Requeue: func(ctx context.Context, tenantID, assetID, actor string) error {
			_, err := proc.Requeue(ctx, tenantID, assetID, actor)
			return err
		},
		Manifests: stack.provider, Bucket: stack.bucket, Log: lifecycleLog, Metrics: metrics.New(),
	}, testIngestionConfig())
	if err != nil {
		t.Fatal(err)
	}
	p := member(stack.env.tenant, "owner", model.RoleDeveloper)
	handlers := events.IngestionHandlers(
		func(ctx context.Context, _, jobID string) (string, error) {
			return svc.ReadStagedManifest(ctx, "it-worker", jobID)
		},
		svc.OnUploadProcessed)

	d, _, err := svc.CreateDataset(ctx, p, ingestion.CreateDatasetInput{Name: "stack-set"})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := svc.CreateJob(ctx, p, ingestion.CreateJobInput{DatasetID: d.ID, IdempotencyKey: "stack"})
	if err != nil {
		t.Fatal(err)
	}
	files := []file{textFile("notes/a.txt", 4096), textFile("notes/b.txt", 300), textFile("media/large.txt", 7<<20)}
	data := contents(files...)
	manifest := manifestOf(files...)

	// The manifest goes to MinIO over a presigned PUT; submitting it writes
	// the event that starts its read.
	staged, err := svc.StageManifest(ctx, p, job.ID, int64(len(manifest)))
	if err != nil {
		t.Fatal(err)
	}
	remove(staged.ObjectKey)
	httpPut(t, staged.UploadURL, "application/x-ndjson", []byte(manifest))
	if _, err := svc.SubmitStagedManifest(ctx, p, job.ID, sha256Hex(manifest)); err != nil {
		t.Fatal(err)
	}
	if stats := pipeline.publish(t); stats.Published != 1 {
		t.Fatalf("publish manifest_staged: %+v", stats)
	}
	if stats := pipeline.consumeAll(t, eventdto.QueueIngestion, handlers); stats.Completed != 1 {
		t.Fatalf("consume manifest_staged: %+v", stats)
	}
	job, err = svc.Finalize(ctx, p, job.ID)
	if err != nil || job.State != model.JobReady || job.Counters.Total != 3 {
		t.Fatalf("finalize: %v %s %+v", err, job.State, job.Counters)
	}

	// The client uploads straight to MinIO: one PUT for the small files,
	// parts for the large one.
	result, err := svc.Claim(ctx, p, job.ID, 10)
	if err != nil || len(result.Grants) != 3 {
		t.Fatalf("claim: %v %+v", err, result)
	}
	for _, g := range result.Grants {
		body := data[g.Entry.Path]
		var parts []uploadservice.CompletedPartInput
		if g.Entry.Strategy == model.StrategySingle {
			httpPut(t, g.UploadURL, g.Entry.ContentType, body)
		} else {
			urls, err := svc.PartURLs(ctx, p, job.ID, g.Entry.Position, []int32{1, 2})
			if err != nil {
				t.Fatal(err)
			}
			var recorded []uploadservice.RecordPartInput
			for i, u := range urls {
				chunk := body[i*(5<<20) : min((i+1)*(5<<20), len(body))]
				etag := httpPut(t, u.URL, "", chunk)
				recorded = append(recorded, uploadservice.RecordPartInput{PartNumber: u.PartNumber, ETag: etag, SizeBytes: int64(len(chunk))})
				parts = append(parts, uploadservice.CompletedPartInput{PartNumber: u.PartNumber, ETag: etag})
			}
			if err := svc.ConfirmParts(ctx, p, job.ID, g.Entry.Position, recorded); err != nil {
				t.Fatal(err)
			}
		}
		entry, err := svc.CompleteEntry(ctx, p, job.ID, g.Entry.Position, parts)
		if err != nil || entry.State != model.EntryUploaded {
			t.Fatalf("complete %s: %v %s", g.Entry.Path, err, entry.State)
		}
		session, err := stack.env.uploads.GetByID(ctx, p.TenantID, entry.UploadID)
		if err != nil {
			t.Fatal(err)
		}
		remove(session.ObjectKey)
	}

	// upload.completed starts processing (ClamAV, promotion), whose end
	// writes upload.processed, which publishes the entries.
	if stats := pipeline.publish(t); stats.Published != 3 {
		t.Fatalf("publish upload.completed: %+v", stats)
	}
	if stats := pipeline.consumeAll(t, eventdto.QueueProcessing, events.ProcessingHandlers(proc.Intake)); stats.Completed != 3 {
		t.Fatalf("consume upload.completed: %+v", stats)
	}
	if stats := pipeline.publish(t); stats.Published != 3 {
		t.Fatalf("publish upload.processed: %+v", stats)
	}
	if stats := pipeline.consumeAll(t, eventdto.QueueIngestion, handlers); stats.Completed != 3 {
		t.Fatalf("consume upload.processed: %+v", stats)
	}

	job, err = svc.GetJob(ctx, p, job.ID)
	if err != nil || job.State != model.JobCompleted || job.Counters.Published != 3 || job.Counters.StoredBytes != totalSize(files...) {
		t.Fatalf("job: %v %s %+v", err, job.State, job.Counters)
	}
	if ds, err := svc.GetDataset(ctx, p, d.ID); err != nil || ds.Status != model.DatasetReady {
		t.Fatalf("dataset: %v %+v", err, ds)
	}
	entries, err := svc.ListEntries(ctx, p, job.ID, nil, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		key := promotionPrefix + "/" + p.TenantID + "/" + entry.AssetID
		remove(key)
		meta, err := stack.provider.HeadObject(ctx, stack.bucket, key)
		if err != nil || meta.SizeBytes != entry.SizeBytes {
			t.Fatalf("%s: promoted object %+v %v", entry.Path, meta, err)
		}
	}

	// Cancelling a job aborts its multipart upload at MinIO.
	job2, _, err := svc.CreateJob(ctx, p, ingestion.CreateJobInput{DatasetID: d.ID, IdempotencyKey: "stack-cancel"})
	if err != nil {
		t.Fatal(err)
	}
	big := textFile("media/cancelled.txt", 6<<20)
	if _, err := svc.SubmitInlineManifest(ctx, p, job2.ID, bytes.NewReader([]byte(manifestOf(big))), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Finalize(ctx, p, job2.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := svc.Claim(ctx, p, job2.ID, 1)
	if err != nil || len(claimed.Grants) != 1 {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	g := claimed.Grants[0]
	urls, err := svc.PartURLs(ctx, p, job2.ID, g.Entry.Position, []int32{1})
	if err != nil {
		t.Fatal(err)
	}
	etag := httpPut(t, urls[0].URL, "", big.data[:5<<20])
	if err := svc.ConfirmParts(ctx, p, job2.ID, g.Entry.Position, []uploadservice.RecordPartInput{{PartNumber: 1, ETag: etag, SizeBytes: 5 << 20}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cancel(ctx, p, job2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdvanceCancellations(ctx); err != nil {
		t.Fatal(err)
	}
	session, err := stack.env.uploads.GetByID(ctx, p.TenantID, g.Entry.UploadID)
	if err != nil || session.Status != model.UploadStatusAborted {
		t.Fatalf("session: %v %+v", err, session)
	}
	if _, err := stack.provider.ListParts(ctx, stack.bucket, session.ObjectKey, session.StorageUploadID); !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		t.Fatalf("the provider still has the upload: %v", err)
	}
	if j, err := svc.GetJob(ctx, p, job2.ID); err != nil || j.State != model.JobCancelled {
		t.Fatalf("cancelled job: %v %s", err, j.State)
	}
}
