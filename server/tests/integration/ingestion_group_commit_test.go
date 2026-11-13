package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// TransitionMany applies a batch of entry changes as Transition would one by
// one: each refusal stays its own, the counters stay exact (a recount
// changes nothing), and a job whose last entry closes in the batch finishes.
func TestTransitionManyMatchesTransitionOneByOne(t *testing.T) {
	e := openIngestEnv(t)
	ctx := context.Background()
	files := []file{textFile("tm/a.txt", 100), textFile("tm/b.txt", 110), textFile("tm/c.txt", 120), textFile("tm/d.txt", 130)}
	data := contents(files...)
	d := e.dataset(t, "transition-many", model.CompleteAll)
	job := e.readyJob(t, d.ID, files...)
	grants := e.claim(t, job.ID, 10).Grants
	if len(grants) != len(files) {
		t.Fatalf("granted %d", len(grants))
	}
	var uploaded []model.IngestionEntry
	for _, g := range grants {
		uploaded = append(uploaded, e.upload(t, g, data[g.Entry.Path]))
	}

	now := time.Now().UTC()
	toProcessing := func(en model.IngestionEntry) interfaces.EntryChange {
		return interfaces.EntryChange{JobID: en.JobID, Position: en.Position, UploadID: en.UploadID, Now: now,
			From: []model.EntryState{model.EntryUploaded}, To: model.EntryProcessing}
	}
	changes := []interfaces.EntryChange{toProcessing(uploaded[0]), toProcessing(uploaded[1]), toProcessing(uploaded[2]), toProcessing(uploaded[3])}
	stale := toProcessing(uploaded[0])
	stale.Position, stale.UploadID = uploaded[1].Position, "another-upload" // an older attempt's outcome
	missing := toProcessing(uploaded[0])
	missing.Position = 999
	changes = append(changes, missing)

	outcomes, err := e.repo.TransitionMany(ctx, changes)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if outcomes[i].Err != nil || outcomes[i].Entry.State != model.EntryProcessing {
			t.Fatalf("change %d: %s %v", i, outcomes[i].Entry.State, outcomes[i].Err)
		}
	}
	if !errors.Is(outcomes[4].Err, model.ErrEntryNotFound) {
		t.Fatalf("a missing entry: %v", outcomes[4].Err)
	}
	// A batch that names one entry twice is refused whole, so the caller
	// writes its changes one at a time.
	if _, err := e.repo.TransitionMany(ctx, []interfaces.EntryChange{stale, stale}); err == nil {
		t.Fatal("a batch naming one entry twice was applied")
	}
	// A stale change is refused on its own, as Transition refuses it.
	outcomes, err = e.repo.TransitionMany(ctx, []interfaces.EntryChange{stale, toProcessing(uploaded[2])})
	if err != nil || !errors.Is(outcomes[0].Err, model.ErrInvalidEntryState) || outcomes[0].Entry.State != model.EntryProcessing ||
		!errors.Is(outcomes[1].Err, model.ErrInvalidEntryState) {
		t.Fatalf("stale and repeated changes: %+v %v", outcomes, err)
	}

	// All four published in one batch: the job finishes.
	var publish []interfaces.EntryChange
	for i, en := range uploaded {
		publish = append(publish, interfaces.EntryChange{JobID: en.JobID, Position: en.Position, UploadID: en.UploadID, Now: now,
			From: []model.EntryState{model.EntryProcessing}, To: model.EntryPublished, StoredBytes: int64(len(files[i].data))})
	}
	if outcomes, err = e.repo.TransitionMany(ctx, publish); err != nil {
		t.Fatal(err)
	}
	for i, o := range outcomes {
		if o.Err != nil || o.Entry.State != model.EntryPublished {
			t.Fatalf("publish %d: %s %v", i, o.Entry.State, o.Err)
		}
	}
	if got := e.job(t, job.ID); got.State != model.JobCompleted || got.Counters.Published != 4 {
		t.Fatalf("job after the batch: %s %+v", got.State, got.Counters)
	}

	// Exact counters: recounting from the entries changes nothing.
	before, after, err := e.repo.Recount(ctx, job.ID, now)
	if err != nil || before != after {
		t.Fatalf("job counters drifted: %+v -> %+v (%v)", before, after, err)
	}
	ub, ua, err := e.repo.ReconcileUsage(ctx, e.p.TenantID, now)
	if err != nil || ub != ua || ua.StoredBytes != totalSize(files...) || ua.ReservedBytes != 0 || ua.PendingEntries != 0 || ua.ActiveJobs != 0 {
		t.Fatalf("tenant usage drifted: %+v -> %+v (%v)", ub, ua, err)
	}
}

// Concurrent completions share transactions through the group commit, and
// the counters stay exact.
func TestConcurrentCompletionsShareTransactions(t *testing.T) {
	e := openIngestEnv(t)
	ctx := context.Background()
	var files []file
	for i := 0; i < 32; i++ {
		files = append(files, textFile("gc/"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt", 100+i))
	}
	data := contents(files...)
	d := e.dataset(t, "group-commit", model.CompleteAll)
	job := e.readyJob(t, d.ID, files...)
	grants := e.claim(t, job.ID, 32).Grants
	if len(grants) != len(files) {
		t.Fatalf("granted %d", len(grants))
	}
	for _, g := range grants {
		e.put(t, g, data[g.Entry.Path])
	}

	before := e.metrics.Snapshot()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, g := range grants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.svc.CompleteEntry(ctx, e.p, g.Entry.JobID, g.Entry.Position, nil); err != nil {
				t.Errorf("complete %d: %v", g.Entry.Position, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	after := e.metrics.Snapshot()

	if n := len(e.entries(t, job.ID, model.EntryUploaded)); n != len(files) {
		t.Fatalf("%d entries uploaded, want %d", n, len(files))
	}
	grouped := after["ingestion_group_commit_entries"] - before["ingestion_group_commit_entries"]
	if after["ingestion_group_commits"] == before["ingestion_group_commits"] || grouped < 2 {
		t.Fatalf("no completions shared a transaction (%d grouped)", grouped)
	}
	t.Logf("%d completions, %d of them in %d shared transactions", len(files), grouped,
		after["ingestion_group_commits"]-before["ingestion_group_commits"])

	b, a, err := e.repo.Recount(ctx, job.ID, time.Now().UTC())
	if err != nil || b != a {
		t.Fatalf("job counters drifted: %+v -> %+v (%v)", b, a, err)
	}
	ub, ua, err := e.repo.ReconcileUsage(ctx, e.p.TenantID, time.Now().UTC())
	if err != nil || ub != ua || ua.ActiveUploads != 0 || ua.AwaitingProcessing != int64(len(files)) {
		t.Fatalf("tenant usage drifted: %+v -> %+v (%v)", ub, ua, err)
	}
}
