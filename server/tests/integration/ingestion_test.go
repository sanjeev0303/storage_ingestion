package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	eventdto "github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// The numbers in the comments are the Phase 7 required scenarios each test
// covers. Manifest parsing, path safety and parser memory (1, 2, 3, 5, 26)
// also have unit tests in internal/service/ingestion.

func TestIngestionMixedSmallAndLargeFilesArePublished(t *testing.T) {
	// 8, 9, 10: small files are one presigned PUT, large ones multipart,
	// in one job; each becomes its own asset through the normal pipeline.
	e := openIngestEnv(t)
	ctx := context.Background()
	base := e.slots(t)
	d := e.dataset(t, "mixed", model.CompleteAll)
	files := []file{
		textFile("docs/a.txt", 2048),
		textFile("video/large.txt", 6<<20),
		textFile("docs/edge.txt", 1<<20), // exactly the threshold: still one PUT
		textFile("docs/b.txt", 100),
	}
	data := contents(files...)
	total := totalSize(files...)

	job := e.readyJob(t, d.ID, files...)
	if job.State != model.JobReady || job.Counters.Total != 4 || job.Counters.Pending != 4 || job.Counters.ExpectedBytes != total {
		t.Fatalf("ready job: %s %+v", job.State, job.Counters)
	}

	result := e.claim(t, job.ID, 10)
	if len(result.Grants) != 4 || result.Failed != 0 {
		t.Fatalf("claim: %+v", result)
	}
	want := map[string]string{
		"docs/a.txt": model.StrategySingle, "video/large.txt": model.StrategyMultipart,
		"docs/edge.txt": model.StrategySingle, "docs/b.txt": model.StrategySingle,
	}
	for _, g := range result.Grants {
		if g.Entry.Strategy != want[g.Entry.Path] || g.Entry.UploadID == "" || g.Entry.State != model.EntryScheduled {
			t.Fatalf("grant %s: %+v", g.Entry.Path, g.Entry)
		}
		if single := g.Entry.Strategy == model.StrategySingle; single != (g.UploadURL != "") {
			t.Fatalf("%s: only a SINGLE grant carries an upload URL", g.Entry.Path)
		}
	}
	if got := e.slots(t); got != base+4 {
		t.Fatalf("slots in use: %d, want %d", got, base+4)
	}
	if u := e.usage(t); u.ActiveUploads != 4 || u.ActiveMultipart != 1 || u.ReservedBytes != total {
		t.Fatalf("usage while uploading: %+v", u)
	}

	for _, g := range result.Grants {
		if g.Entry.Strategy == model.StrategyMultipart {
			urls, err := e.svc.PartURLs(ctx, e.p, job.ID, g.Entry.Position, []int32{1, 2})
			if err != nil || len(urls) != 2 || urls[0].URL == "" {
				t.Fatalf("part URLs: %v %+v", err, urls)
			}
		}
		entry := e.upload(t, g, data[g.Entry.Path])
		if entry.State != model.EntryUploaded {
			t.Fatalf("%s after completion: %s", entry.Path, entry.State)
		}
		if note := e.process(t, entry); note != "published" {
			t.Fatalf("%s: %s", entry.Path, note)
		}
	}

	job = e.job(t, job.ID)
	if job.State != model.JobCompleted || job.Counters.Published != 4 || job.Counters.StoredBytes != total || job.CompletedAt == nil {
		t.Fatalf("finished job: %s %+v", job.State, job.Counters)
	}
	if ds := e.getDataset(t, d.ID); ds.Status != model.DatasetReady || ds.StoredBytes != total || ds.ReservedBytes != 0 || ds.ReadyAt == nil {
		t.Fatalf("dataset: %+v", ds)
	}
	if u := e.usage(t); u != (model.TenantUsage{TenantID: e.tenant, StoredBytes: total}) {
		t.Fatalf("usage afterwards: %+v", u)
	}
	if got := e.slots(t); got != base {
		t.Fatalf("slots after the job: %d, want %d", got, base)
	}

	// Each entry is its own AVAILABLE asset, and its outcome went through
	// the outbox exactly once.
	for _, entry := range e.entries(t, job.ID) {
		asset, err := e.assets.GetByUpload(ctx, e.tenant, entry.UploadID)
		if err != nil || asset.ID != entry.AssetID || asset.Status != model.AssetStatusAvailable || entry.StoredBytes != entry.SizeBytes {
			t.Fatalf("%s: asset %+v, entry %+v, %v", entry.Path, asset, entry, err)
		}
		if n := e.outboxEvents(t, eventdto.TypeUploadProcessed, entry.UploadID); n != 1 {
			t.Fatalf("%s: %d upload.processed events", entry.Path, n)
		}
	}
}

func TestIngestionRejectsDuplicateAndUnsafePaths(t *testing.T) {
	// 4, 5: duplicates by normalized path and unsafe paths become INVALID
	// entries with a reason; the rest of the manifest is accepted.
	e := openIngestEnv(t)
	ctx := context.Background()
	d := e.dataset(t, "paths", model.CompleteAll)
	cat := textFile("photos/cat.txt", 10)
	manifest := manifestOf(
		cat,                               // 1
		textFile("photos//./cat.txt", 10), // 2: the same path once normalized
		textFile("Photos/CAT.txt", 10),    // 3: paths match without case
		textFile("../etc/passwd.txt", 10), // 4
		textFile("/abs/file.txt", 10),     // 5
		textFile(`C:\win\file.txt`, 10),   // 6
		textFile("_system/x.txt", 10),     // 7: reserved
		textFile("photos/dog.txt", 10),    // 8
	)
	job := e.newJob(t, d.ID)
	report := e.submit(t, job.ID, manifest)
	// Samples are the lines validation rejected; duplicates are found when
	// stored, and are counted and listed with the failures.
	if report.Accepted != 2 || report.Invalid != 6 || report.Duplicates != 2 || !report.Complete || len(report.Samples) != 4 {
		t.Fatalf("report: %+v", report)
	}
	if failures, err := e.svc.ListEntries(ctx, e.p, job.ID, ingestion.FailedStates, 0, 0); err != nil || len(failures) != 6 {
		t.Fatalf("failures: %v %d", err, len(failures))
	}
	codes := map[int64]string{
		2: model.EntryErrDuplicatePath, 3: model.EntryErrDuplicatePath, 4: model.EntryErrInvalidPath,
		5: model.EntryErrInvalidPath, 6: model.EntryErrInvalidPath, 7: model.EntryErrInvalidPath,
	}
	for pos, code := range codes {
		if entry := e.entry(t, job.ID, pos); entry.State != model.EntryInvalid || entry.ErrorCode != code {
			t.Errorf("line %d: %s %s (%s), want %s", pos, entry.State, entry.ErrorCode, entry.ErrorDetail, code)
		}
	}
	if first := e.entry(t, job.ID, 1); first.Path != "photos/cat.txt" || first.State != model.EntryPending {
		t.Fatalf("first: %+v", first)
	}

	// A path is taken in the whole dataset, not only in its job.
	other := e.newJob(t, d.ID)
	if r := e.submit(t, other.ID, manifestOf(textFile("PHOTOS/cat.txt", 10), textFile("photos/new.txt", 10))); r.Accepted != 1 || r.Duplicates != 1 {
		t.Fatalf("second job: %+v", r)
	}
	// Another dataset is another namespace.
	elsewhere := e.dataset(t, "paths-two", model.CompleteAll)
	if r := e.submit(t, e.newJob(t, elsewhere.ID).ID, manifestOf(cat)); r.Accepted != 1 {
		t.Fatalf("other dataset: %+v", r)
	}

	// Once the entry holding a path is cancelled, the path is free again.
	if _, err := e.svc.Cancel(ctx, e.p, job.ID); err != nil {
		t.Fatal(err)
	}
	e.advanceCancellations(t)
	if j := e.job(t, job.ID); j.State != model.JobCancelled {
		t.Fatalf("cancelled job: %s", j.State)
	}
	if r := e.submit(t, e.newJob(t, d.ID).ID, manifestOf(cat)); r.Accepted != 1 {
		t.Fatalf("after cancellation: %+v", r)
	}
}

func TestIngestionManifestSubmissionIsIdempotent(t *testing.T) {
	// 6: creating a dataset or job again returns it; submitting the same
	// manifest again stores nothing new; a different one is refused.
	e := openIngestEnv(t)
	ctx := context.Background()
	d := e.dataset(t, "idem", model.CompleteAll)

	same, created, err := e.svc.CreateDataset(ctx, e.p, ingestion.CreateDatasetInput{Name: "idem"})
	if err != nil || created || same.ID != d.ID {
		t.Fatalf("dataset again: %v %v %s", err, created, same.ID)
	}
	if _, _, err := e.svc.CreateDataset(ctx, e.p, ingestion.CreateDatasetInput{Name: "idem", CompletionPolicy: model.CompletePartial}); !errors.Is(err, model.ErrDatasetConflict) {
		t.Fatalf("dataset with other settings: %v", err)
	}

	in := ingestion.CreateJobInput{DatasetID: d.ID, IdempotencyKey: "nightly-2026-10-19"}
	job, created, err := e.svc.CreateJob(ctx, e.p, in)
	if err != nil || !created {
		t.Fatal(err)
	}
	again, created, err := e.svc.CreateJob(ctx, e.p, in)
	if err != nil || created || again.ID != job.ID {
		t.Fatalf("job again: %v %v %s", err, created, again.ID)
	}
	other := e.dataset(t, "idem-other", model.CompleteAll)
	if _, _, err := e.svc.CreateJob(ctx, e.p, ingestion.CreateJobInput{DatasetID: other.ID, IdempotencyKey: in.IdempotencyKey}); !errors.Is(err, model.ErrIngestionConflict) {
		t.Fatalf("key reused for another dataset: %v", err)
	}

	manifest := manifestOf(textFile("a.txt", 10), textFile("b.txt", 20)) + "not json\n"
	first := e.submit(t, job.ID, manifest)
	if first.Accepted != 2 || first.Invalid != 1 || !first.Complete || first.SHA256 != sha256Hex(manifest) {
		t.Fatalf("first submission: %+v", first)
	}
	second := e.submit(t, job.ID, manifest)
	if !second.Complete || second.Accepted != 0 || second.Replayed != 3 || second.SHA256 != first.SHA256 {
		t.Fatalf("same manifest again: %+v", second)
	}
	if j := e.job(t, job.ID); j.Counters.Total != 3 || j.Counters.Pending != 2 {
		t.Fatalf("counters after a replay: %+v", j.Counters)
	}
	if _, err := e.svc.SubmitInlineManifest(ctx, e.p, job.ID, strings.NewReader(manifestOf(textFile("c.txt", 5))), ""); !errors.Is(err, model.ErrManifestConflict) {
		t.Fatalf("a different manifest: %v", err)
	}

	f1, err := e.svc.Finalize(ctx, e.p, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := e.svc.Finalize(ctx, e.p, job.ID)
	if err != nil || f2.State != model.JobReady || f1.FinalizedAt == nil || !f2.FinalizedAt.Equal(*f1.FinalizedAt) {
		t.Fatalf("finalize again: %v %+v", err, f2)
	}

	// A manifest that does not match its declared checksum fails its job.
	bad := e.newJob(t, d.ID)
	_, err = e.svc.SubmitInlineManifest(ctx, e.p, bad.ID, strings.NewReader(manifestOf(textFile("z.txt", 5))), strings.Repeat("0", 64))
	if !errors.Is(err, ingestion.ErrManifestChecksum) {
		t.Fatalf("checksum mismatch: %v", err)
	}
	e.advanceCancellations(t)
	if j := e.job(t, bad.ID); j.State != model.JobFailed || j.Error == "" {
		t.Fatalf("job of a mismatched manifest: %s %q", j.State, j.Error)
	}
}

// failingReader returns data, then fails as a dropped connection would.
type failingReader struct {
	data string
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, errors.New("connection reset by peer")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestIngestionManifestBatchesRollBackAndResume(t *testing.T) {
	// 7: each batch is stored with its checkpoint in one transaction. A
	// batch that fails part-way leaves nothing behind, and submitting the
	// manifest again resumes after the last stored batch.
	e := openIngestEnv(t) // batches of 100 lines
	ctx := context.Background()
	files := series("set", 350, 16)
	manifest := manifestOf(files...)

	// The tenant may hold 250 waiting entries: the third batch inserts its
	// rows, then fails the pending-entry check and rolls back whole.
	if err := e.repo.SetQuota(ctx, e.tenant, model.TenantQuota{MaxPendingEntries: 250}, time.Now()); err != nil {
		t.Fatal(err)
	}
	d := e.dataset(t, "batches", model.CompleteAll)
	job := e.newJob(t, d.ID)
	report, err := e.svc.SubmitInlineManifest(ctx, e.p, job.ID, strings.NewReader(manifest), "")
	if !errors.Is(err, model.ErrQuotaExceeded) || report.Accepted != 200 {
		t.Fatalf("submission over the limit: %v %+v", err, report)
	}
	stored := e.job(t, job.ID)
	if stored.State != model.JobValidating || stored.ParsedLines != 200 || stored.Counters.Total != 200 || stored.ManifestComplete {
		t.Fatalf("after the failed batch: %s lines=%d %+v", stored.State, stored.ParsedLines, stored.Counters)
	}
	if n := e.count(t, `SELECT count(*) FROM ingestion_entries WHERE job_id = $1`, job.ID); n != 200 {
		t.Fatalf("%d entries stored; the failed batch left rows behind", n)
	}
	if u := e.usage(t); u.PendingEntries != 200 {
		t.Fatalf("pending entries: %d", u.PendingEntries)
	}

	// With the limit lifted, the same manifest resumes after line 200.
	if err := e.repo.SetQuota(ctx, e.tenant, model.TenantQuota{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	report = e.submit(t, job.ID, manifest)
	if report.Accepted != 150 || !report.Complete {
		t.Fatalf("resumed submission: %+v", report)
	}
	entries := e.entries(t, job.ID)
	if len(entries) != 350 {
		t.Fatalf("%d entries", len(entries))
	}
	for i, entry := range entries {
		if entry.Position != int64(i+1) || entry.Path != files[i].path {
			t.Fatalf("entry %d: position %d path %s", i, entry.Position, entry.Path)
		}
	}

	// A body that breaks off mid-line keeps the lines before it.
	d2 := e.dataset(t, "batches-two", model.CompleteAll)
	job2 := e.newJob(t, d2.ID)
	cut := strings.Index(manifest, files[249].path) // inside line 250
	_, err = e.svc.SubmitInlineManifest(ctx, e.p, job2.ID, &failingReader{data: manifest[:cut]}, "")
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("broken body: %v", err)
	}
	if j := e.job(t, job2.ID); j.ParsedLines != 249 || j.Counters.Total != 249 {
		t.Fatalf("after a broken body: lines=%d %+v", j.ParsedLines, j.Counters)
	}
	if r := e.submit(t, job2.ID, manifest); r.Accepted != 101 || !r.Complete {
		t.Fatalf("resubmitted: %+v", r)
	}
	if j := e.job(t, job2.ID); j.Counters.Total != 350 || j.ParsedBytes != int64(len(manifest)) {
		t.Fatalf("complete job: %+v bytes=%d", j.Counters, j.ParsedBytes)
	}
}

func TestIngestionLargeInlineManifestStreams(t *testing.T) {
	// 3, 26: a large manifest is read and stored in batches; memory stays
	// flat whatever its size.
	if testing.Short() {
		t.Skip("stores 100,000 entries")
	}
	e := openIngestEnv(t, func(c *config.Ingestion) { c.InsertBatch = 1000 })
	d := e.dataset(t, "large-manifest", model.CompleteAll)
	job := e.newJob(t, d.ID)
	const lines = 100_000

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	body := &generatedManifest{n: lines}
	sampler := startHeapSampler()
	started := time.Now()
	report, err := e.svc.SubmitInlineManifest(context.Background(), e.p, job.ID, body, "")
	elapsed := time.Since(started)
	peak := sampler.stop()
	if err != nil || report.Accepted != lines || !report.Complete {
		t.Fatalf("submission: %v %+v", err, report)
	}
	growth := int64(peak) - int64(before.HeapInuse)
	t.Logf("stored %d entries (%d MiB manifest) in %s, %.0f lines/s; heap in use peaked %d MiB above the start",
		lines, body.bytes>>20, elapsed.Round(time.Millisecond), float64(lines)/elapsed.Seconds(), growth>>20)
	if growth > 64<<20 {
		t.Fatalf("heap grew by %d MiB while streaming", growth>>20)
	}
	if j := e.job(t, job.ID); j.Counters.Total != lines || j.ParsedLines != lines {
		t.Fatalf("job: %+v", j.Counters)
	}
}

// generatedManifest produces manifest lines on demand.
type generatedManifest struct {
	n, i  int
	buf   []byte
	bytes int64
}

func (g *generatedManifest) Read(p []byte) (int, error) {
	for len(g.buf) == 0 {
		if g.i >= g.n {
			return 0, io.EOF
		}
		g.i++
		g.buf = []byte(textFile(fmt.Sprintf("gen/%03d/%07d.txt", g.i/1000, g.i), 64+g.i%512).line() + "\n")
	}
	n := copy(p, g.buf)
	g.buf = g.buf[n:]
	g.bytes += int64(n)
	return n, nil
}

// heapSampler records the peak heap in use while it runs.
type heapSampler struct {
	done chan struct{}
	peak chan uint64
}

func startHeapSampler() *heapSampler {
	s := &heapSampler{done: make(chan struct{}), peak: make(chan uint64, 1)}
	go func() {
		var peak uint64
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapInuse)
			select {
			case <-s.done:
				s.peak <- peak
				return
			case <-ticker.C:
			}
		}
	}()
	return s
}

func (s *heapSampler) stop() uint64 {
	close(s.done)
	return <-s.peak
}

type claimTarget struct {
	env   *ingestEnv
	jobID string
}

// claimConcurrently runs claims of max entries each from n goroutines,
// spread over targets in turn, and returns every grant by tenant.
func claimConcurrently(t *testing.T, targets []claimTarget, n, max int) (map[string][]ingestion.Grant, []string) {
	t.Helper()
	var mu sync.Mutex
	grants := map[string][]ingestion.Grant{}
	var limits []string
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		target := targets[i%len(targets)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := target.env.svc.Claim(context.Background(), target.env.p, target.jobID, max)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			grants[target.env.p.TenantID] = append(grants[target.env.p.TenantID], result.Grants...)
			limits = append(limits, result.LimitedBy)
		}()
	}
	wg.Wait()
	return grants, limits
}

// uniqueGrants fails the test if one entry or upload was granted twice.
func uniqueGrants(t *testing.T, grants []ingestion.Grant) {
	t.Helper()
	entries := map[string]bool{}
	uploads := map[string]bool{}
	for _, g := range grants {
		key := fmt.Sprintf("%s/%d", g.Entry.JobID, g.Entry.Position)
		if entries[key] || uploads[g.Entry.UploadID] {
			t.Fatalf("entry %s granted twice", key)
		}
		entries[key], uploads[g.Entry.UploadID] = true, true
	}
}

func TestIngestionEnforcesGlobalAndTenantSlotLimits(t *testing.T) {
	// 11: concurrent claims of two tenants never pass the global slots or
	// a tenant's fair share of them.
	e := openIngestEnv(t)
	base := e.slots(t)
	e.cfg.GlobalUploadSlots = base + 6
	e.cfg.TenantUploadSlots, e.cfg.JobUploadSlots, e.cfg.TenantMultipartSessions = 4, 4, 4
	e.svc = e.service(t, e.repo)
	other := e.as(e.newTenant(t))

	var targets []claimTarget
	data := map[string][]byte{}
	for _, env := range []*ingestEnv{e, other} {
		d := env.dataset(t, "slots", model.CompleteAll)
		files := series("slot", 10, 32)
		for path, b := range contents(files...) {
			data[path] = b
		}
		targets = append(targets, claimTarget{env: env, jobID: env.readyJob(t, d.ID, files...).ID})
	}

	grants, _ := claimConcurrently(t, targets, 16, 2)
	// With both tenants waiting, each gets at most half of the six global
	// slots, and together they fill them.
	var all []ingestion.Grant
	for tenant, g := range grants {
		if len(g) > 3 {
			t.Fatalf("tenant %s holds %d slots, more than its fair share of 3", tenant, len(g))
		}
		all = append(all, g...)
	}
	uniqueGrants(t, all)
	if len(all) != 6 {
		t.Fatalf("%d slots granted, want all 6", len(all))
	}
	if got := e.slots(t); got != base+6 {
		t.Fatalf("global slots in use: %d, want %d", got, base+6)
	}
	for _, env := range []*ingestEnv{e, other} {
		if u := env.usage(t); u.ActiveUploads != len(grants[env.p.TenantID]) {
			t.Fatalf("tenant %s: usage counts %d uploads, holds %d", env.p.TenantID, u.ActiveUploads, len(grants[env.p.TenantID]))
		}
	}

	// Everything is taken: a claim grants nothing and says why.
	full := e.claim(t, targets[0].jobID, 5)
	if len(full.Grants) != 0 || full.LimitedBy != interfaces.LimitGlobalSlots || full.RetryAfter <= 0 {
		t.Fatalf("claim at the limit: %+v", full)
	}
	// Completing an upload frees its slot for the next claim.
	mine := grants[e.tenant][0]
	e.upload(t, mine, data[mine.Entry.Path])
	if next := e.claim(t, targets[0].jobID, 5); len(next.Grants) != 1 {
		t.Fatalf("claim after a completion: %+v", next)
	}
}

func TestIngestionSharesTenantSlotsBetweenJobs(t *testing.T) {
	// 12: a tenant's slots are shared between its jobs with work, so one
	// big job cannot starve another; max_concurrency caps a job further.
	e := openIngestEnv(t, func(c *config.Ingestion) {
		c.TenantUploadSlots, c.JobUploadSlots, c.TenantMultipartSessions = 4, 4, 4
	})
	ctx := context.Background()
	d := e.dataset(t, "fair", model.CompleteAll)
	firstFiles, secondFiles := series("first", 10, 32), series("second", 10, 32)
	data := contents(append(append([]file{}, firstFiles...), secondFiles...)...)
	first := e.readyJob(t, d.ID, firstFiles...)
	second := e.readyJob(t, d.ID, secondFiles...)

	a := e.claim(t, first.ID, 10)
	if len(a.Grants) != 2 || a.LimitedBy != interfaces.LimitJobSlots {
		t.Fatalf("first job asked for 10: %+v", a)
	}
	b := e.claim(t, second.ID, 10)
	if len(b.Grants) != 2 {
		t.Fatalf("second job: %+v", b)
	}
	if again := e.claim(t, first.ID, 10); len(again.Grants) != 0 || again.LimitedBy == "" {
		t.Fatalf("first job again while the tenant is full: %+v", again)
	}

	// Finishing the first job's uploads lets it claim again, within its
	// share.
	for _, g := range a.Grants {
		e.upload(t, g, data[g.Entry.Path])
	}
	next := e.claim(t, first.ID, 10)
	if len(next.Grants) != 2 {
		t.Fatalf("first job after its uploads: %+v", next)
	}

	// With three jobs waiting and every slot free, a job capped at one
	// upload gets one.
	for _, g := range append(b.Grants, next.Grants...) {
		e.upload(t, g, data[g.Entry.Path])
	}
	capped, _, err := e.svc.CreateJob(ctx, e.p, ingestion.CreateJobInput{DatasetID: d.ID, IdempotencyKey: "capped", MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.submit(t, capped.ID, manifestOf(series("capped", 5, 32)...))
	if _, err := e.svc.Finalize(ctx, e.p, capped.ID); err != nil {
		t.Fatal(err)
	}
	if c := e.claim(t, capped.ID, 10); len(c.Grants) != 1 || c.LimitedBy != interfaces.LimitJobSlots {
		t.Fatalf("capped job: %+v", c)
	}
}

func TestIngestionProcessingBacklogHoldsClaims(t *testing.T) {
	// 13: while processing lags, claims grant nothing, so uploads do not
	// pile up ahead of scanning.
	e := openIngestEnv(t, func(c *config.Ingestion) { c.TenantProcessingBacklog = 2 })
	d := e.dataset(t, "backlog", model.CompleteAll)
	files := series("backlog", 5, 64)
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)

	first := e.claim(t, job.ID, 2)
	var uploaded []model.IngestionEntry
	for _, g := range first.Grants {
		uploaded = append(uploaded, e.upload(t, g, data[g.Entry.Path]))
	}
	if u := e.usage(t); u.AwaitingProcessing != 2 {
		t.Fatalf("awaiting processing: %d", u.AwaitingProcessing)
	}
	held := e.claim(t, job.ID, 5)
	if len(held.Grants) != 0 || held.LimitedBy != interfaces.LimitBacklog || held.RetryAfter <= 0 {
		t.Fatalf("claim with a full backlog: %+v", held)
	}

	e.process(t, uploaded[0])
	if resumed := e.claim(t, job.ID, 5); len(resumed.Grants) != 3 {
		t.Fatalf("claim once processing caught up: %+v", resumed)
	}
}

func TestIngestionQuotaHoldsUnderConcurrentJobs(t *testing.T) {
	// 14: concurrent claims of two jobs never reserve more than the
	// tenant's quota; published bytes then count against it.
	e := openIngestEnv(t)
	ctx := context.Background()
	if err := e.repo.SetQuota(ctx, e.tenant, model.TenantQuota{MaxStorageBytes: 10_000}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// A dataset's own cap is checked when its job is finalized.
	capped, _, err := e.svc.CreateDataset(ctx, e.p, ingestion.CreateDatasetInput{Name: "quota-capped", MaxBytes: 3000})
	if err != nil {
		t.Fatal(err)
	}
	over := e.newJob(t, capped.ID)
	e.submit(t, over.ID, manifestOf(series("over", 4, 1000)...))
	if _, err := e.svc.Finalize(ctx, e.p, over.ID); !errors.Is(err, model.ErrQuotaExceeded) || !strings.Contains(err.Error(), "dataset") {
		t.Fatalf("job over the dataset cap: %v", err)
	}

	d := e.dataset(t, "quota", model.CompleteAll)
	aFiles, bFiles := series("a", 8, 1000), series("b", 8, 1000)
	data := contents(append(append([]file{}, aFiles...), bFiles...)...)
	jobA := e.readyJob(t, d.ID, aFiles...)
	jobB := e.readyJob(t, d.ID, bFiles...)
	// A job that could never fit is refused before any upload.
	tooBig := e.newJob(t, d.ID)
	e.submit(t, tooBig.ID, manifestOf(series("c", 11, 1000)...))
	if _, err := e.svc.Finalize(ctx, e.p, tooBig.ID); !errors.Is(err, model.ErrQuotaExceeded) {
		t.Fatalf("job over the tenant quota: %v", err)
	}

	grants, limits := claimConcurrently(t, []claimTarget{{e, jobA.ID}, {e, jobB.ID}}, 8, 3)
	all := grants[e.tenant]
	uniqueGrants(t, all)
	var reserved int64
	for _, g := range all {
		reserved += g.Entry.SizeBytes
	}
	if reserved != 10_000 || len(all) != 10 {
		t.Fatalf("granted %d entries reserving %d bytes, want 10 and 10000", len(all), reserved)
	}
	if !contains(limits, interfaces.LimitQuota) {
		t.Fatalf("no claim reported the quota: %v", limits)
	}
	if u := e.usage(t); u.ReservedBytes != 10_000 {
		t.Fatalf("tenant reserved %d", u.ReservedBytes)
	}
	if ds := e.getDataset(t, d.ID); ds.ReservedBytes != 10_000 {
		t.Fatalf("dataset reserved %d", ds.ReservedBytes)
	}

	// Published bytes replace the reservations and keep the quota full.
	for _, g := range all {
		e.process(t, e.upload(t, g, data[g.Entry.Path]))
	}
	if u := e.usage(t); u.ReservedBytes != 0 || u.StoredBytes != 10_000 {
		t.Fatalf("usage after publishing: %+v", u)
	}
	// Claim on a job with entries left: how the concurrent claims split the
	// ten grants varies, and a job granted all eight of its entries has
	// finished by now.
	fromA := 0
	for _, g := range all {
		if g.Entry.JobID == jobA.ID {
			fromA++
		}
	}
	open := jobA.ID
	if fromA == len(aFiles) {
		open = jobB.ID
	}
	if c := e.claim(t, open, 5); len(c.Grants) != 0 || c.LimitedBy != interfaces.LimitQuota {
		t.Fatalf("claim with a full quota: %+v", c)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestIngestionReleasesQuotaAfterFailureAndCancellation(t *testing.T) {
	// 15: a failed or cancelled entry gives back its slot and reserved
	// bytes; a cancelled multipart upload is aborted at the provider.
	e := openIngestEnv(t)
	ctx := context.Background()
	base := e.slots(t)
	d := e.dataset(t, "release", model.CompleteAll)
	files := []file{
		textFile("a.txt", 1000), textFile("b.txt", 1000), textFile("c.txt", 1000),
		textFile("big.txt", 2<<20), textFile("d.txt", 1000),
	}
	job := e.readyJob(t, d.ID, files...)
	grants := e.claim(t, job.ID, 4).Grants
	if u := e.usage(t); u.ReservedBytes != 3000+2<<20 || u.ActiveUploads != 4 || u.ActiveMultipart != 1 {
		t.Fatalf("usage after the claim: %+v", u)
	}

	// a.txt: the client uploads other bytes than it declared.
	if failed := e.upload(t, grants[0], []byte("short")); failed.State != model.EntryFailed || failed.ErrorCode != model.EntryErrSizeMismatch {
		t.Fatalf("size mismatch: %+v", failed)
	}
	if u := e.usage(t); u.ReservedBytes != 2000+2<<20 || u.ActiveUploads != 3 {
		t.Fatalf("usage after a failure: %+v", u)
	}

	// b.txt: the client goes away and its lease runs out.
	e.expireLease(t, job.ID, 2)
	if _, err := e.svc.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if b := e.entry(t, job.ID, 2); b.State != model.EntryRetryable || b.ErrorCode != model.EntryErrLeaseExpired || b.UploadID != "" {
		t.Fatalf("abandoned entry: %+v", b)
	}
	if u := e.usage(t); u.ReservedBytes != 1000+2<<20 || u.ActiveUploads != 2 {
		t.Fatalf("usage after an expired lease: %+v", u)
	}

	// Cancel with c.txt and big.txt in flight and d.txt still waiting.
	if j, err := e.svc.Cancel(ctx, e.p, job.ID); err != nil || j.State != model.JobCancelling {
		t.Fatalf("cancel: %v %s", err, j.State)
	}
	e.advanceCancellations(t)
	job = e.job(t, job.ID)
	if job.State != model.JobCancelled || job.Counters.Cancelled != 4 || job.Counters.Failed != 1 {
		t.Fatalf("cancelled job: %s %+v", job.State, job.Counters)
	}
	if u := e.usage(t); u != (model.TenantUsage{TenantID: e.tenant}) {
		t.Fatalf("usage after cancellation: %+v", u)
	}
	if got := e.slots(t); got != base {
		t.Fatalf("slots: %d, want %d", got, base)
	}
	if e.storage.AbortCalls != 1 {
		t.Fatalf("multipart aborts: %d", e.storage.AbortCalls)
	}
	if ds := e.getDataset(t, d.ID); ds.ReservedBytes != 0 || ds.Status != model.DatasetIncomplete {
		t.Fatalf("dataset: %+v", ds)
	}
}

func TestIngestionRepeatedClaimsAndEventsDoNotDuplicateWork(t *testing.T) {
	// 16: concurrent claims, repeated completions, repeated processing and
	// duplicate upload.processed deliveries all leave one asset per entry.
	e := openIngestEnv(t)
	ctx := context.Background()
	d := e.dataset(t, "repeat", model.CompleteAll)
	files := series("repeat", 20, 64)
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)

	grants, _ := claimConcurrently(t, []claimTarget{{e, job.ID}}, 8, 5)
	all := grants[e.tenant]
	uniqueGrants(t, all)
	if len(all) != 20 {
		t.Fatalf("%d entries granted", len(all))
	}

	// A client that lost a claim's answer asks for its entry again and
	// gets the same upload session.
	refreshed, err := e.svc.RefreshUpload(ctx, e.p, job.ID, all[0].Entry.Position)
	if err != nil || refreshed.Entry.UploadID != all[0].Entry.UploadID || refreshed.UploadURL == "" {
		t.Fatalf("refresh: %v %+v", err, refreshed.Entry)
	}

	for _, g := range all {
		entry := e.upload(t, g, data[g.Entry.Path])
		if again, err := e.svc.CompleteEntry(ctx, e.p, job.ID, g.Entry.Position, nil); err != nil || again.State != model.EntryUploaded {
			t.Fatalf("completing again: %v %s", err, again.State)
		}
		if _, err := e.proc.Intake(ctx, e.tenant, entry.UploadID); err != nil {
			t.Fatal(err)
		}
		// The completion event redelivered: processing must not run twice.
		if _, err := e.proc.Intake(ctx, e.tenant, entry.UploadID); err != nil && !errors.Is(err, model.ErrStaleEvent) {
			t.Fatal(err)
		}
		for i, want := range []string{"published", "nothing to record", "nothing to record"} {
			note, err := e.svc.OnUploadProcessed(ctx, e.tenant, entry.UploadID)
			if err != nil || !strings.Contains(note, want) {
				t.Fatalf("delivery %d of upload.processed: %v %q", i+1, err, note)
			}
		}
	}

	job = e.job(t, job.ID)
	if job.State != model.JobCompleted || job.Counters.Published != 20 || job.Counters.StoredBytes != totalSize(files...) {
		t.Fatalf("job: %s %+v", job.State, job.Counters)
	}
	if u := e.usage(t); u.StoredBytes != totalSize(files...) {
		t.Fatalf("stored bytes counted %d times over", u.StoredBytes/totalSize(files...))
	}
	if n := e.count(t, `SELECT count(*) FROM assets WHERE tenant_id = $1`, e.tenant); n != 20 {
		t.Fatalf("%d assets for 20 entries", n)
	}
	for _, entry := range e.entries(t, job.ID) {
		if n := e.outboxEvents(t, eventdto.TypeUploadProcessed, entry.UploadID); n != 1 {
			t.Fatalf("%s: %d upload.processed events", entry.Path, n)
		}
	}
	if stored, actual := e.recount(t, job.ID); stored != actual {
		t.Fatalf("counters drifted: %+v != %+v", stored, actual)
	}
}

func TestIngestionRecoversAbandonedUploadsAfterRestart(t *testing.T) {
	// 17: a worker started after a crash finds expired leases: an upload
	// that was stored is completed, others are retried with a new session,
	// and a started multipart upload is aborted.
	e := openIngestEnv(t)
	ctx := context.Background()
	base := e.slots(t)
	d := e.dataset(t, "restart", model.CompleteAll)
	files := []file{textFile("stored.txt", 300), textFile("never-sent.txt", 300), textFile("parts.txt", 6<<20)}
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)
	grants := e.claim(t, job.ID, 3).Grants

	// The client stored the first file but crashed before completing it,
	// never sent the second, and sent one part of the third.
	e.put(t, grants[0], data["stored.txt"])
	session := e.session(t, grants[2].Entry.UploadID)
	etag := e.storage.PutPart(session.StorageUploadID, 1, data["parts.txt"][:5<<20])
	if err := e.svc.ConfirmParts(ctx, e.p, job.ID, 3, []uploadservice.RecordPartInput{{PartNumber: 1, ETag: etag, SizeBytes: 5 << 20}}); err != nil {
		t.Fatal(err)
	}

	restarted := e.service(t, e.repo)
	for pos := int64(1); pos <= 3; pos++ {
		e.expireLease(t, job.ID, pos)
	}
	if _, err := restarted.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if stored := e.entry(t, job.ID, 1); stored.State != model.EntryUploaded || stored.Attempts != 0 {
		t.Fatalf("stored but not completed: %+v", stored)
	}
	for _, pos := range []int64{2, 3} {
		entry := e.entry(t, job.ID, pos)
		if entry.State != model.EntryRetryable || entry.ErrorCode != model.EntryErrLeaseExpired || entry.UploadID != "" || entry.UploadGeneration != 1 {
			t.Fatalf("entry %d: %+v", pos, entry)
		}
	}
	if e.storage.AbortCalls != 1 || e.session(t, grants[2].Entry.UploadID).Status != model.UploadStatusAborted {
		t.Fatalf("the started multipart upload was not aborted")
	}
	if got := e.slots(t); got != base {
		t.Fatalf("slots: %d, want %d", got, base)
	}

	waitForBackoff()
	if _, err := restarted.RunDueRetries(ctx); err != nil {
		t.Fatal(err)
	}
	e.process(t, e.entry(t, job.ID, 1))
	again := e.claim(t, job.ID, 10).Grants
	if len(again) != 2 {
		t.Fatalf("retried grants: %d", len(again))
	}
	for _, g := range again {
		if g.Entry.UploadID == grants[g.Entry.Position-1].Entry.UploadID {
			t.Fatalf("entry %d reused its abandoned session", g.Entry.Position)
		}
		e.process(t, e.upload(t, g, data[g.Entry.Path]))
	}
	if j := e.job(t, job.ID); j.State != model.JobCompleted || j.Counters.Published != 3 {
		t.Fatalf("job: %s %+v", j.State, j.Counters)
	}
}

// stage uploads manifest as job's staged manifest and submits it.
func (e *ingestEnv) stage(t *testing.T, jobID, manifest, declaredSHA256 string) {
	t.Helper()
	ctx := context.Background()
	staged, err := e.svc.StageManifest(ctx, e.p, jobID, int64(len(manifest)))
	if err != nil || staged.UploadURL == "" {
		t.Fatalf("stage: %v %+v", err, staged)
	}
	e.storage.PutObject(staged.ObjectKey, []byte(manifest))
	if job, err := e.svc.SubmitStagedManifest(ctx, e.p, jobID, declaredSHA256); err != nil || job.State != model.JobValidating {
		t.Fatalf("submit staged: %v %s", err, job.State)
	}
}

func TestIngestionStagedManifestResumesFromItsCheckpoint(t *testing.T) {
	// 3, 17, 27: a staged manifest is read in bounded passes; each stores
	// whole batches with their checkpoint, and a pass that stops (budget
	// spent, a crash, a storage error) is resumed by another reader.
	e := openIngestEnv(t, func(c *config.Ingestion) { c.ParseBudget = time.Nanosecond })
	ctx := context.Background()
	d := e.dataset(t, "staged", model.CompleteAll)
	files := series("staged", 450, 8)
	manifest := manifestOf(files...)
	job := e.newJob(t, d.ID)

	if _, err := e.svc.SubmitStagedManifest(ctx, e.p, job.ID, ""); !errors.Is(err, ingestion.ErrManifestNotStaged) {
		t.Fatalf("submitting before the upload: %v", err)
	}
	e.stage(t, job.ID, manifest, sha256Hex(manifest))
	if n := e.outboxEvents(t, eventdto.TypeManifestStaged, job.ID); n != 1 {
		t.Fatalf("%d manifest_staged events", n)
	}

	// A storage error ends a pass without moving the checkpoint.
	e.storage.RangeErr = errors.New("connection reset")
	if _, err := e.svc.ReadStagedManifest(ctx, "reader-0", job.ID); err == nil {
		t.Fatal("a failed range read was not reported")
	}
	if j := e.job(t, job.ID); j.ParsedLines != 0 || j.Counters.Total != 0 {
		t.Fatalf("after a storage error: %d lines", j.ParsedLines)
	}

	// Each pass stores one batch before its budget is spent; passes
	// alternate between readers, as after restarts.
	passes := 0
	for note := ""; note != "manifest read"; {
		passes++
		if passes > 20 {
			t.Fatalf("still reading after %d passes: %s", passes, note)
		}
		var err error
		note, err = e.svc.ReadStagedManifest(ctx, fmt.Sprintf("reader-%d", passes%2), job.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if passes < 5 {
		t.Fatalf("read in %d passes; the budget did not bound them", passes)
	}
	stored := e.job(t, job.ID)
	if !stored.ManifestComplete || stored.Counters.Total != 450 || stored.ParsedBytes != int64(len(manifest)) || stored.ManifestSHA256 != sha256Hex(manifest) {
		t.Fatalf("read job: %+v", stored)
	}
	for i, entry := range e.entries(t, job.ID) {
		if entry.Position != int64(i+1) || entry.Path != files[i].path {
			t.Fatalf("entry %d: %d %s", i, entry.Position, entry.Path)
		}
	}

	// A reader that died holding the lease blocks others until it runs out;
	// then the worker's resume pass takes over.
	d2 := e.dataset(t, "staged-two", model.CompleteAll)
	second := e.newJob(t, d2.ID)
	e.stage(t, second.ID, manifest, "")
	if claimed, err := e.repo.ClaimManifests(ctx, "crashed-reader", second.ID, time.Now().UTC(), time.Minute, 1); err != nil || len(claimed) != 1 {
		t.Fatalf("lease: %v %d", err, len(claimed))
	}
	if note, err := e.svc.ReadStagedManifest(ctx, "reader-x", second.ID); err != nil || !strings.HasPrefix(note, "not due") {
		t.Fatalf("read under another's lease: %v %q", err, note)
	}
	e.exec(t, `UPDATE ingestion_jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, second.ID)
	for i := 0; i < 20 && !e.job(t, second.ID).ManifestComplete; i++ {
		if _, err := e.svc.ResumeManifests(ctx, "worker"); err != nil {
			t.Fatal(err)
		}
	}
	if j := e.job(t, second.ID); !j.ManifestComplete || j.Counters.Total != 450 {
		t.Fatalf("resumed job: %+v", j.Counters)
	}
}

func TestIngestionStagedManifestRefusesChangedContent(t *testing.T) {
	// A staged manifest replaced after submission, or not matching its
	// declared checksum, fails its job rather than mixing two manifests.
	e := openIngestEnv(t, func(c *config.Ingestion) { c.ParseBudget = time.Nanosecond })
	ctx := context.Background()
	d := e.dataset(t, "staged-changed", model.CompleteAll)
	manifest := manifestOf(series("changed", 250, 8)...)

	replaced := e.newJob(t, d.ID)
	e.stage(t, replaced.ID, manifest, "")
	if _, err := e.svc.ReadStagedManifest(ctx, "reader", replaced.ID); err != nil {
		t.Fatal(err)
	}
	e.storage.PutObject(manifestKeyOf(e.tenant, replaced.ID), []byte(manifestOf(series("other", 250, 8)...)))
	if note, err := e.svc.ReadStagedManifest(ctx, "reader", replaced.ID); err != nil || note != "manifest gone" {
		t.Fatalf("read after replacement: %v %q", err, note)
	}
	e.advanceCancellations(t)
	if j := e.job(t, replaced.ID); j.State != model.JobFailed || j.Counters.Cancelled != 100 {
		t.Fatalf("replaced manifest's job: %s %+v", j.State, j.Counters)
	}

	d2 := e.dataset(t, "staged-mismatch", model.CompleteAll)
	mismatch := e.newJob(t, d2.ID)
	e.stage(t, mismatch.ID, manifest, strings.Repeat("ab", 32))
	note := ""
	for i := 0; i < 10 && !strings.HasPrefix(note, "manifest rejected"); i++ {
		var err error
		if note, err = e.svc.ReadStagedManifest(ctx, "reader", mismatch.ID); err != nil {
			t.Fatal(err)
		}
	}
	e.advanceCancellations(t)
	if j := e.job(t, mismatch.ID); j.State != model.JobFailed || !strings.Contains(j.Error, "sha256") {
		t.Fatalf("mismatched manifest: %q, job %s %q", note, j.State, j.Error)
	}
}

func manifestKeyOf(tenantID, jobID string) string {
	return "manifests/" + tenantID + "/" + jobID + ".jsonl"
}

func TestIngestionPartialCompletionAndReadinessPolicies(t *testing.T) {
	// 18, 22: rejected content fails its entry while the rest publish; the
	// dataset is never ready while entries are in flight, and afterwards
	// its completion policy decides.
	e := openIngestEnv(t)
	cases := []struct {
		policy      model.CompletionPolicy
		badRequired bool
		want        model.DatasetStatus
	}{
		{model.CompleteAll, false, model.DatasetIncomplete},
		{model.CompleteRequired, false, model.DatasetReady},
		{model.CompleteRequired, true, model.DatasetIncomplete},
		{model.CompletePartial, true, model.DatasetPartiallyReady},
	}
	for i, c := range cases {
		d := e.dataset(t, fmt.Sprintf("policy-%d", i), c.policy)
		bad := infectedFile("c.txt")
		if !c.badRequired {
			bad.extra = `,"required":false`
		}
		files := []file{textFile("a.txt", 50), textFile("b.txt", 60), bad}
		data := contents(files...)
		job := e.readyJob(t, d.ID, files...)
		for n, g := range e.claim(t, job.ID, 3).Grants {
			e.process(t, e.upload(t, g, data[g.Entry.Path]))
			if n == 0 {
				if ds := e.getDataset(t, d.ID); ds.Status != model.DatasetIngesting {
					t.Fatalf("%s: dataset %s while entries are in flight", c.policy, ds.Status)
				}
			}
		}
		if rejected := e.entry(t, job.ID, 3); rejected.State != model.EntryFailed || rejected.ErrorCode != model.EntryErrRejected {
			t.Fatalf("%s: infected entry %+v", c.policy, rejected)
		}
		if j := e.job(t, job.ID); j.State != model.JobPartiallyCompleted || j.Counters.Published != 2 {
			t.Fatalf("%s: job %s %+v", c.policy, j.State, j.Counters)
		}
		if ds := e.getDataset(t, d.ID); ds.Status != c.want {
			t.Fatalf("%s (bad file required=%v): dataset %s, want %s", c.policy, c.badRequired, ds.Status, c.want)
		}
	}
}

func TestIngestionRetryLeavesPublishedEntriesAlone(t *testing.T) {
	// 19: retrying a partly failed job uploads only the failed entries
	// again; published ones keep their single asset.
	e := openIngestEnv(t)
	ctx := context.Background()
	d := e.dataset(t, "retry", model.CompleteAll)
	files := []file{textFile("ok-1.txt", 100), textFile("ok-2.txt", 100), textFile("wrong-size.txt", 100), infectedFile("bad.txt")}
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)
	grants := e.claim(t, job.ID, 4).Grants
	e.process(t, e.upload(t, grants[0], data["ok-1.txt"]))
	e.process(t, e.upload(t, grants[1], data["ok-2.txt"]))
	if entry := e.upload(t, grants[2], []byte("too short")); entry.ErrorCode != model.EntryErrSizeMismatch {
		t.Fatalf("wrong size: %+v", entry)
	}
	e.process(t, e.upload(t, grants[3], data["bad.txt"]))
	if j := e.job(t, job.ID); j.State != model.JobPartiallyCompleted || j.Counters.Failed != 2 {
		t.Fatalf("job: %s %+v", j.State, j.Counters)
	}
	assets := map[int64]string{1: e.entry(t, job.ID, 1).AssetID, 2: e.entry(t, job.ID, 2).AssetID}

	if r, err := e.svc.Retry(ctx, e.p, job.ID, []int64{1, 2}, false); err != nil || r.Retried != 0 {
		t.Fatalf("retrying published entries: %v %+v", err, r)
	}
	// Rejected content is final; the wrong upload is retried.
	r, err := e.svc.Retry(ctx, e.p, job.ID, nil, true)
	if err != nil || r.Retried != 1 {
		t.Fatalf("retry all: %v %+v", err, r)
	}
	if j := e.job(t, job.ID); j.State != model.JobRunning || j.Counters.Pending != 1 {
		t.Fatalf("job after retry: %s %+v", j.State, j.Counters)
	}
	if u := e.usage(t); u.ActiveJobs != 1 {
		t.Fatalf("a retried job counts as active again: %+v", u)
	}

	again := e.claim(t, job.ID, 10).Grants
	if len(again) != 1 || again[0].Entry.Position != 3 || again[0].Entry.UploadID == grants[2].Entry.UploadID {
		t.Fatalf("retried grants: %+v", again)
	}
	e.process(t, e.upload(t, again[0], data["wrong-size.txt"]))
	if j := e.job(t, job.ID); j.State != model.JobPartiallyCompleted || j.Counters.Published != 3 {
		t.Fatalf("job after the retried upload: %s %+v", j.State, j.Counters)
	}
	for pos, id := range assets {
		if got := e.entry(t, job.ID, pos).AssetID; got != id {
			t.Fatalf("entry %d changed asset: %s -> %s", pos, id, got)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM assets WHERE tenant_id = $1 AND status = 'AVAILABLE'`, e.tenant); n != 3 {
		t.Fatalf("%d available assets, want 3", n)
	}
}

func TestIngestionCancellationDuringActiveUploads(t *testing.T) {
	// 20, 21: cancelling stops new grants, cancels waiting and in-flight
	// entries and aborts their multipart uploads. Stored uploads still
	// publish. A provider abort that fails is finished later by the
	// multipart recovery (Phase 5 reconciliation resolves ABORTING).
	e := openIngestEnv(t)
	ctx := context.Background()
	base := e.slots(t)
	d := e.dataset(t, "cancel", model.CompleteAll)
	files := []file{
		textFile("stored.txt", 400),     // 1: uploaded before the cancel
		textFile("half.txt", 6<<20),     // 2: one part sent
		textFile("idle.txt", 400),       // 3: claimed, nothing sent
		textFile("big-idle.txt", 2<<20), // 4: multipart, nothing sent
		textFile("waiting-1.txt", 400),  // 5
		textFile("waiting-2.txt", 400),  // 6
	}
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)
	grants := e.claim(t, job.ID, 4).Grants
	stored := e.upload(t, grants[0], data["stored.txt"])
	half := e.session(t, grants[1].Entry.UploadID)
	etag := e.storage.PutPart(half.StorageUploadID, 1, data["half.txt"][:5<<20])
	if err := e.svc.ConfirmParts(ctx, e.p, job.ID, 2, []uploadservice.RecordPartInput{{PartNumber: 1, ETag: etag, SizeBytes: 5 << 20}}); err != nil {
		t.Fatal(err)
	}

	if j, err := e.svc.Cancel(ctx, e.p, job.ID); err != nil || j.State != model.JobCancelling {
		t.Fatalf("cancel: %v %s", err, j.State)
	}
	if c := e.claim(t, job.ID, 5); len(c.Grants) != 0 || c.LimitedBy != interfaces.LimitJobNotRunning {
		t.Fatalf("claim on a cancelled job: %+v", c)
	}
	if _, err := e.svc.CompleteEntry(ctx, e.p, job.ID, 3, nil); !errors.Is(err, model.ErrInvalidJobState) {
		t.Fatalf("completing on a cancelled job: %v", err)
	}

	// The provider fails the first abort.
	e.storage.AbortErr = errors.New("provider unavailable")
	e.advanceCancellations(t)
	for _, pos := range []int64{2, 3, 4, 5, 6} {
		if entry := e.entry(t, job.ID, pos); entry.State != model.EntryCancelled {
			t.Fatalf("entry %d: %s", pos, entry.State)
		}
	}
	if s := e.session(t, grants[1].Entry.UploadID); s.Status != model.UploadStatusAborting {
		t.Fatalf("session whose abort failed: %s", s.Status)
	}
	if s := e.session(t, grants[3].Entry.UploadID); s.Status != model.UploadStatusAborted {
		t.Fatalf("session aborted on the first try: %s", s.Status)
	}
	// The stored upload is still being processed: the job waits for it.
	if j := e.job(t, job.ID); j.State != model.JobCancelling {
		t.Fatalf("job with a stored upload left: %s", j.State)
	}
	if note := e.process(t, stored); note != "published" {
		t.Fatalf("stored upload: %s", note)
	}
	if j := e.job(t, job.ID); j.State != model.JobCancelled || j.Counters.Published != 1 || j.Counters.Cancelled != 5 {
		t.Fatalf("job: %s %+v", j.State, j.Counters)
	}

	// Recovery finishes the failed abort.
	if status, err := e.multi.ResolveAbort(ctx, e.tenant, grants[1].Entry.UploadID); err != nil || status != model.UploadStatusAborted {
		t.Fatalf("recovery: %v %s", err, status)
	}
	if left, err := e.storage.ListMultipartUploads(ctx, e.bucket, half.ObjectKey); err != nil || len(left) != 0 {
		t.Fatalf("provider uploads left: %v %v", err, left)
	}

	if u := e.usage(t); u != (model.TenantUsage{TenantID: e.tenant, StoredBytes: 400}) {
		t.Fatalf("usage: %+v", u)
	}
	if got := e.slots(t); got != base {
		t.Fatalf("slots: %d, want %d", got, base)
	}
	if _, err := e.svc.Cancel(ctx, e.p, job.ID); !errors.Is(err, model.ErrInvalidJobState) {
		t.Fatalf("cancelling a cancelled job: %v", err)
	}
}

// Cancellation goes through waiting entries in position order and picks
// up where the last pass stopped. An entry that fails back to waiting
// behind that point is still found, the job ends CANCELLED with its
// counters matching its entries, and another pass changes nothing.
func TestIngestionCancellationResumesAndFindsStragglers(t *testing.T) {
	e := openIngestEnv(t, func(c *config.Ingestion) { c.WorkerBatch = 2 }) // 40 entries a pass
	ctx := context.Background()
	d := e.dataset(t, "cancel-keyset", model.CompleteAll)
	var files []file
	for i := 1; i <= 50; i++ {
		files = append(files, textFile(fmt.Sprintf("entry-%02d.txt", i), 100))
	}
	job := e.readyJob(t, d.ID, files...)
	stored := e.upload(t, e.claim(t, job.ID, 1).Grants[0], files[0].data)
	if _, err := e.svc.Cancel(ctx, e.p, job.ID); err != nil {
		t.Fatal(err)
	}
	waiting := func() int {
		return e.count(t, `SELECT count(*) FROM ingestion_entries WHERE job_id = $1 AND state IN ('PENDING', 'FAILED_RETRYABLE')`, job.ID)
	}

	e.advanceCancellations(t)
	if n := waiting(); n != 9 {
		t.Fatalf("one pass cancels 40 waiting entries, %d left of 49", n)
	}
	// The stored upload fails retryably: waiting again, behind the cursor.
	retryAt := time.Now().UTC()
	if _, err := e.repo.Transition(ctx, interfaces.EntryChange{
		JobID: job.ID, Position: 1, From: []model.EntryState{stored.State}, To: model.EntryRetryable,
		Now: retryAt, UploadID: stored.UploadID, ErrorCode: "processing_failed", ErrorDetail: "test", NextAttemptAt: &retryAt,
	}); err != nil {
		t.Fatal(err)
	}
	e.advanceCancellations(t) // the rest after the cursor, then the end
	if n := waiting(); n != 1 {
		t.Fatalf("only the straggler behind the cursor is left, got %d", n)
	}
	e.advanceCancellations(t) // starts over and finds it
	j := e.job(t, job.ID)
	if j.State != model.JobCancelled || j.Counters.Cancelled != 50 || j.Counters.Open() != 0 || waiting() != 0 {
		t.Fatalf("job: %s %+v, %d waiting", j.State, j.Counters, waiting())
	}
	cancelled := e.count(t, `SELECT count(*) FROM ingestion_entries WHERE job_id = $1 AND state = 'CANCELLED'`, job.ID)
	if int64(cancelled) != j.Counters.Cancelled {
		t.Fatalf("counters %d against %d cancelled entries", j.Counters.Cancelled, cancelled)
	}
	e.advanceCancellations(t)
	if again := e.job(t, job.ID); again.State != model.JobCancelled || again.Counters != j.Counters {
		t.Fatalf("another pass changed a cancelled job: %+v", again.Counters)
	}
}

func TestIngestionCountersStayAccurateUnderConcurrency(t *testing.T) {
	// 23: many clients claim, upload, complete and process at once, with
	// duplicate outcome deliveries; the counters match the entries.
	e := openIngestEnv(t)
	ctx := context.Background()
	base := e.slots(t)
	d := e.dataset(t, "counters", model.CompleteAll)
	files := series("count", 120, 128)
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				result, err := e.svc.Claim(ctx, e.p, job.ID, 4)
				if err != nil {
					t.Error(err)
					return
				}
				if len(result.Grants) == 0 {
					return
				}
				for _, g := range result.Grants {
					session, err := e.uploads.GetByID(ctx, e.tenant, g.Entry.UploadID)
					if err != nil {
						t.Error(err)
						return
					}
					e.storage.PutObject(session.ObjectKey, data[g.Entry.Path])
					if _, err := e.svc.Heartbeat(ctx, e.p, job.ID, g.Entry.Position); err != nil {
						t.Error(err)
						return
					}
					entry, err := e.svc.CompleteEntry(ctx, e.p, job.ID, g.Entry.Position, nil)
					if err != nil {
						t.Error(err)
						return
					}
					if _, err := e.proc.Intake(ctx, e.tenant, entry.UploadID); err != nil {
						t.Error(err)
						return
					}
					for i := 0; i < 2; i++ {
						if _, err := e.svc.OnUploadProcessed(ctx, e.tenant, entry.UploadID); err != nil {
							t.Error(err)
							return
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	job = e.job(t, job.ID)
	if job.State != model.JobCompleted || job.Counters.Published != 120 || job.Counters.Open() != 0 || job.Counters.StoredBytes != totalSize(files...) {
		t.Fatalf("job: %s %+v", job.State, job.Counters)
	}
	if stored, actual := e.recount(t, job.ID); stored != actual {
		t.Fatalf("counters drifted under concurrency:\nstored %+v\nactual %+v", stored, actual)
	}
	if u := e.usage(t); u != (model.TenantUsage{TenantID: e.tenant, StoredBytes: totalSize(files...)}) {
		t.Fatalf("usage: %+v", u)
	}
	if got := e.slots(t); got != base {
		t.Fatalf("slots: %d, want %d", got, base)
	}
}

func TestIngestionEntryListingsPage(t *testing.T) {
	// 24: entry and failure listings page by position through large jobs.
	e := openIngestEnv(t)
	ctx := context.Background()
	d := e.dataset(t, "pages", model.CompleteAll)
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		if i%25 == 0 {
			b.WriteString("{broken\n")
			continue
		}
		b.WriteString(textFile(fmt.Sprintf("p/%05d.txt", i), 8).line() + "\n")
	}
	job := e.newJob(t, d.ID)
	if r := e.submit(t, job.ID, b.String()); r.Accepted != 2400 || r.Invalid != 100 {
		t.Fatalf("report: %+v", r)
	}

	var after int64
	seen := 0
	for pages := 1; ; pages++ {
		page, err := e.svc.ListEntries(ctx, e.p, job.ID, nil, after, 300)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page {
			seen++
			if entry.Position != int64(seen) {
				t.Fatalf("page %d: position %d, want %d", pages, entry.Position, seen)
			}
		}
		if len(page) < 300 {
			break
		}
		after = page[len(page)-1].Position
	}
	if seen != 2500 {
		t.Fatalf("paged through %d entries", seen)
	}

	failures := 0
	after = 0
	for {
		page, err := e.svc.ListEntries(ctx, e.p, job.ID, ingestion.FailedStates, after, 30)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page {
			failures++
			if entry.Position != int64(failures*25) || entry.ErrorCode != model.EntryErrInvalidJSON {
				t.Fatalf("failure %d: %+v", failures, entry)
			}
		}
		if len(page) < 30 {
			break
		}
		after = page[len(page)-1].Position
	}
	if failures != 100 {
		t.Fatalf("paged through %d failures", failures)
	}
	if page, err := e.svc.ListEntries(ctx, e.p, job.ID, nil, 0, 0); err != nil || len(page) != ingestion.PageSize(0) {
		t.Fatalf("default page: %v %d", err, len(page))
	}
}

func TestIngestionReconcileRepairsStaleCounters(t *testing.T) {
	// 25: reconciliation recounts drifted job counters and tenant usage
	// from the entries, and finishes a job whose stale counters kept it
	// open. (Global slots are counted from the entries, so they cannot
	// drift.)
	e := openIngestEnv(t)
	ctx := context.Background()
	base := e.slots(t)
	d := e.dataset(t, "reconcile", model.CompleteAll)
	files := series("rec", 10, 100)
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)
	grants := e.claim(t, job.ID, 3).Grants

	e.exec(t, `UPDATE ingestion_jobs SET pending_entries = 99, published_entries = 5 WHERE id = $1`, job.ID)
	e.exec(t, `UPDATE ingestion_tenant_usage SET reserved_bytes = 12345, active_uploads = 40, awaiting_processing = 7 WHERE tenant_id = $1`, e.tenant)

	report, err := e.svc.Reconcile(ctx)
	if err != nil || report.JobsRepaired < 1 || report.TenantsRepaired < 1 {
		t.Fatalf("reconcile: %v %+v", err, report)
	}
	j := e.job(t, job.ID)
	if j.Counters.Pending != 7 || j.Counters.Scheduled != 3 || j.Counters.Published != 0 {
		t.Fatalf("repaired counters: %+v", j.Counters)
	}
	if u := e.usage(t); u.ReservedBytes != 300 || u.ActiveUploads != 3 || u.AwaitingProcessing != 0 || u.PendingEntries != 10 {
		t.Fatalf("repaired usage: %+v", u)
	}
	if got := e.slots(t); got != base+3 {
		t.Fatalf("slots: %d, want %d", got, base+3)
	}

	// Finish the job, then leave it looking unfinished: one entry counted as
	// processing, the job RUNNING and still active.
	for _, g := range grants {
		e.process(t, e.upload(t, g, data[g.Entry.Path]))
	}
	e.ingestAll(t, job.ID, data)
	if j := e.job(t, job.ID); j.State != model.JobCompleted {
		t.Fatalf("job before the drift: %s", j.State)
	}
	e.exec(t, `UPDATE ingestion_jobs SET state = 'RUNNING', completed_at = NULL,
	           published_entries = published_entries - 1, processing_entries = 1 WHERE id = $1`, job.ID)
	e.exec(t, `UPDATE ingestion_tenant_usage SET active_jobs = active_jobs + 1 WHERE tenant_id = $1`, e.tenant)
	if _, err := e.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if j := e.job(t, job.ID); j.State != model.JobCompleted || j.Counters.Published != 10 || j.Counters.Processing != 0 {
		t.Fatalf("job after reconciliation: %s %+v", j.State, j.Counters)
	}
	if u := e.usage(t); u.ActiveJobs != 0 || u.StoredBytes != 1000 {
		t.Fatalf("usage after reconciliation: %+v", u)
	}
}

// flakyRepo fails the next failures calls of Transition, as a dropped
// database connection would.
type flakyRepo struct {
	interfaces.IngestionRepository
	mu       sync.Mutex
	failures int
}

func (r *flakyRepo) Transition(ctx context.Context, change interfaces.EntryChange) (model.IngestionEntry, error) {
	r.mu.Lock()
	fail := r.failures > 0
	if fail {
		r.failures--
	}
	r.mu.Unlock()
	if fail {
		return model.IngestionEntry{}, errors.New("write ingestion entry: unexpected EOF")
	}
	return r.IngestionRepository.Transition(ctx, change)
}

func TestIngestionSurvivesDatabaseAndStorageOutages(t *testing.T) {
	// 27: provider, database, scanner and queue failures leave durable
	// state consistent, and every one is recovered by a retry or a sweep.
	e := openIngestEnv(t)
	ctx := context.Background()
	d := e.dataset(t, "outages", model.CompleteAll)
	files := []file{textFile("one.txt", 300), textFile("two.txt", 300), textFile("big.txt", 2<<20), textFile("three.txt", 300)}
	data := contents(files...)
	job := e.readyJob(t, d.ID, files...)

	// The provider refuses to start the multipart upload: the entry gives
	// back its slot and is retried after its backoff.
	e.storage.BeginErr = errors.New("503 slow down")
	first := e.claim(t, job.ID, 3)
	if len(first.Grants) != 2 || first.Failed != 1 {
		t.Fatalf("claim during a provider outage: %+v", first)
	}
	if big := e.entry(t, job.ID, 3); big.State != model.EntryRetryable || big.ErrorCode != model.EntryErrStorage {
		t.Fatalf("multipart entry: %+v", big)
	}
	if u := e.usage(t); u.ActiveUploads != 2 || u.ActiveMultipart != 0 || u.ReservedBytes != 600 {
		t.Fatalf("usage after a failed start: %+v", u)
	}

	// The provider fails the completion check: nothing moves, and the
	// client completes again.
	one, two := first.Grants[0], first.Grants[1]
	e.put(t, one, data["one.txt"])
	e.storage.HeadErr = errors.New("i/o timeout")
	if _, err := e.svc.CompleteEntry(ctx, e.p, job.ID, one.Entry.Position, nil); err == nil {
		t.Fatal("a failed completion check was not reported")
	}
	if entry := e.entry(t, job.ID, one.Entry.Position); entry.State != model.EntryScheduled {
		t.Fatalf("entry after a failed completion check: %s", entry.State)
	}
	if entry, err := e.svc.CompleteEntry(ctx, e.p, job.ID, one.Entry.Position, nil); err != nil || entry.State != model.EntryUploaded {
		t.Fatalf("completing again: %v %s", err, entry.State)
	}

	// The database drops the write that records a completion: the upload
	// session is completed, the entry is not. The lease sweep finds the
	// completed session and records it.
	flaky := &flakyRepo{IngestionRepository: e.repo, failures: 1}
	unreliable := e.service(t, flaky)
	e.put(t, two, data["two.txt"])
	if _, err := unreliable.CompleteEntry(ctx, e.p, job.ID, two.Entry.Position, nil); err == nil {
		t.Fatal("a failed database write was not reported")
	}
	if entry := e.entry(t, job.ID, two.Entry.Position); entry.State != model.EntryScheduled {
		t.Fatalf("entry after a failed write: %s", entry.State)
	}
	e.expireLease(t, job.ID, two.Entry.Position)
	if _, err := e.svc.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if entry := e.entry(t, job.ID, two.Entry.Position); entry.State != model.EntryUploaded {
		t.Fatalf("entry after the lease sweep: %s", entry.State)
	}

	// A database that cannot be reached fails requests cleanly.
	closed, err := postgres.Open(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	down := e.service(t, postgres.NewIngestionRepository(closed))
	if _, err := down.Claim(ctx, e.p, job.ID, 1); err == nil {
		t.Fatal("a claim without a database succeeded")
	}
	if _, err := down.SubmitInlineManifest(ctx, e.p, job.ID, strings.NewReader(""), ""); err == nil {
		t.Fatal("a manifest without a database succeeded")
	}

	// The scanner is down: processing holds the upload, the entry waits
	// with its upload kept, and is requeued, not uploaded again, once the
	// scanner is back.
	e.scannerDown.Store(true)
	held := e.entry(t, job.ID, one.Entry.Position)
	if _, err := e.proc.Intake(ctx, e.tenant, held.UploadID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		waitForBackoff()
		if _, err := e.proc.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if note, _ := e.svc.OnUploadProcessed(ctx, e.tenant, held.UploadID); note == "processing held" {
			break
		}
	}
	held = e.entry(t, job.ID, one.Entry.Position)
	if held.State != model.EntryRetryable || held.ErrorCode != model.EntryErrProcessingHeld || held.UploadID == "" {
		t.Fatalf("entry while the scanner is down: %+v", held)
	}
	e.scannerDown.Store(false)
	waitForBackoff()
	if _, err := e.svc.RunDueRetries(ctx); err != nil {
		t.Fatal(err)
	}
	if entry := e.entry(t, job.ID, one.Entry.Position); entry.State != model.EntryProcessing || entry.UploadID != held.UploadID {
		t.Fatalf("requeued entry: %+v", entry)
	}
	if _, err := e.proc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The queue lost the upload.processed events: the outcome sweep reads
	// the assets instead.
	if _, err := e.proc.Intake(ctx, e.tenant, e.entry(t, job.ID, two.Entry.Position).UploadID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := e.svc.SyncOutcomes(ctx); err != nil {
		t.Fatal(err)
	}
	for _, pos := range []int64{one.Entry.Position, two.Entry.Position} {
		if entry := e.entry(t, job.ID, pos); entry.State != model.EntryPublished {
			t.Fatalf("entry %d after the outcome sweep: %+v", pos, entry)
		}
	}

	// The provider is back: the failed start is retried and the job ends.
	waitForBackoff()
	if _, err := e.svc.RunDueRetries(ctx); err != nil {
		t.Fatal(err)
	}
	e.ingestAll(t, job.ID, data)
	if j := e.job(t, job.ID); j.State != model.JobCompleted || j.Counters.Published != 4 {
		t.Fatalf("job after the outages: %s %+v", j.State, j.Counters)
	}
	if stored, actual := e.recount(t, job.ID); stored != actual {
		t.Fatalf("counters drifted: %+v != %+v", stored, actual)
	}
}
