package dataset

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
)

// streamRepo stands in for the database in the bounded-memory test: it
// resolves every reference and remembers only counts. Methods the import
// does not call are left to the nil embedded interface.
type streamRepo struct {
	interfaces.DatasetRepository
	batches, members int64
	largestBatch     int
	finished         model.ImportState
}

func (r *streamRepo) ResolveObjects(_ context.Context, _ string, refs []interfaces.ObjectRef) ([]interfaces.ResolvedRef, error) {
	out := make([]interfaces.ResolvedRef, len(refs))
	for i, ref := range refs {
		out[i].ObjectID = ref.ObjectID
	}
	return out, nil
}

func (r *streamRepo) AddMembers(_ context.Context, _, _ string, changes []model.MemberChange, _ model.DuplicatePolicy,
	_, _ string, _ time.Time, _ *interfaces.ImportCheckpoint) (model.MemberResult, error) {
	r.batches++
	r.members += int64(len(changes))
	r.largestBatch = max(r.largestBatch, len(changes))
	return model.MemberResult{Outcomes: make([]model.MemberOutcome, len(changes))}, nil
}

func (r *streamRepo) ExtendImport(context.Context, string, string, time.Time, time.Duration) (bool, error) {
	return false, nil
}

func (r *streamRepo) FinishImport(_ context.Context, _, _ string, state model.ImportState, _ string, _ time.Time) error {
	r.finished = state
	return nil
}

// generated is a manifest of n lines produced as it is read: it never
// exists whole anywhere, so the import's memory is all that is measured.
type generated struct {
	n, next int
	pending []byte
}

func (g *generated) Read(p []byte) (int, error) {
	if len(g.pending) == 0 {
		if g.next == g.n {
			return 0, io.EOF
		}
		g.pending = fmt.Appendf(nil, "{\"object_id\":\"obj-%09d\",\"label\":\"class-%d\",\"split\":\"train\"}\n", g.next, g.next%10)
		g.next++
	}
	n := copy(p, g.pending)
	g.pending = g.pending[n:]
	return n, nil
}

func (g *generated) Close() error { return nil }

type generatedStore struct {
	ManifestStore
	lines int
}

func (s generatedStore) OpenRange(context.Context, string, string, int64, int64, interfaces.ReadCondition) (io.ReadCloser, error) {
	return &generated{n: s.lines}, nil
}

func TestLargeManifestIsStreamedInBoundedMemory(t *testing.T) {
	const lines = 1_000_000 // about 60 MB of manifest
	repo := &streamRepo{}
	svc, err := NewService(Dependencies{
		Repo: repo, Manifests: generatedStore{lines: lines}, ManifestBucket: "b",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, config.Datasets{
		ImportBatch: 500, ImportBudget: time.Hour, MaxManifestLineBytes: 4096, MaxManifestEntries: 2 * lines,
		MaxManifestBytes: 1 << 40, MaxImportErrors: 100, MaxAttempts: 3, ContentConcurrency: 1, Lease: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	imp := model.ManifestImport{ID: "imp", TenantID: "t", DatasetID: "d", State: model.ImportRunning, Source: model.ManifestSourceStaged,
		ManifestKey: "k", ManifestBytes: 1 << 40, DuplicatePolicy: model.DuplicateReject, Attempts: 1}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&m)
				peak = max(peak, m.HeapAlloc)
			}
		}
	}()
	note := svc.runImport(context.Background(), "worker", imp)
	close(stop)
	wg.Wait()

	if repo.finished != model.ImportCompleted || repo.members != lines {
		t.Fatalf("%s: finished %s with %d members", note, repo.finished, repo.members)
	}
	if repo.largestBatch > 500 {
		t.Fatalf("a batch held %d members", repo.largestBatch)
	}
	growth := int64(peak) - int64(base.HeapAlloc)
	t.Logf("%d lines in %d batches; peak heap growth %.1f MiB", lines, repo.batches, float64(growth)/(1<<20))
	if growth > 32<<20 {
		t.Fatalf("heap grew by %d bytes while streaming the manifest", growth)
	}
}
