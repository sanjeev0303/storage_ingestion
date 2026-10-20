package compaction

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeStore holds a number of removable rows per rule and records every
// call, so batching, cutoffs and dry runs can be checked exactly.
type fakeStore struct {
	rows    map[string]int64
	batches map[string][]int
	cutoffs map[string]time.Time
	failOn  string
	runs    []model.CompactionRun
	cancel  func() // called after the first batch, when set
}

func newFakeStore(rows map[string]int64) *fakeStore {
	return &fakeStore{rows: rows, batches: map[string][]int{}, cutoffs: map[string]time.Time{}}
}

func (f *fakeStore) Count(_ context.Context, rule string, cutoff time.Time, limit int) (int64, error) {
	f.cutoffs[rule] = cutoff
	return min(f.rows[rule], int64(limit)), nil
}

func (f *fakeStore) CompactBatch(ctx context.Context, rule string, cutoff time.Time, limit int) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if rule == f.failOn {
		return 0, errors.New("statement timeout")
	}
	f.cutoffs[rule] = cutoff
	n := min(f.rows[rule], int64(limit))
	f.rows[rule] -= n
	f.batches[rule] = append(f.batches[rule], int(n))
	if f.cancel != nil {
		f.cancel()
	}
	return n, nil
}

func (f *fakeStore) RecordRun(_ context.Context, run model.CompactionRun) error {
	f.runs = append(f.runs, run)
	return nil
}

func (f *fakeStore) RecentRuns(context.Context, int) ([]model.CompactionRun, error) {
	return f.runs, nil
}

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func newService(t *testing.T, store *fakeStore, change ...func(*Config)) *Service {
	t.Helper()
	cfg := Config{
		BatchSize: 100, MaxBatches: 5,
		AuditRetention: 365 * 24 * time.Hour, OperationalRetention: 30 * 24 * time.Hour, IdempotencyTTL: 7 * 24 * time.Hour,
	}
	for _, c := range change {
		c(&cfg)
	}
	svc, err := NewService(store, cfg, quietLog, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return now }
	return svc
}

func TestBoundedBatchesAndCutoffs(t *testing.T) {
	store := newFakeStore(map[string]int64{"reconciliation_tasks": 250, "delivery_audit_events": 1000})
	results, err := newService(t, store).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// 250 rows: two full batches and a short one that ends the rule.
	if got := store.batches["reconciliation_tasks"]; !slices.Equal(got, []int{100, 100, 50}) {
		t.Fatalf("batches %v", got)
	}
	// 1000 rows: capped at MaxBatches; the rest waits for the next run.
	if got := store.batches["delivery_audit_events"]; len(got) != 5 || store.rows["delivery_audit_events"] != 500 {
		t.Fatalf("batches %v, left %d", got, store.rows["delivery_audit_events"])
	}

	// Audit records keep the long retention, operational ones the short.
	if c := store.cutoffs["delivery_audit_events"]; !c.Equal(now.Add(-365 * 24 * time.Hour)) {
		t.Fatalf("audit cutoff %v", c)
	}
	if c := store.cutoffs["reconciliation_tasks"]; !c.Equal(now.Add(-30 * 24 * time.Hour)) {
		t.Fatalf("operational cutoff %v", c)
	}
	if c := store.cutoffs["upload_idempotency_keys"]; !c.Equal(now.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("idempotency cutoff %v", c)
	}

	if len(results) != len(Rules) || len(store.runs) != len(Rules) {
		t.Fatalf("every rule must run and be recorded: %d results, %d runs", len(results), len(store.runs))
	}
	for _, r := range results {
		if r.Status != "COMPLETED" {
			t.Fatalf("%s: %+v", r.Rule, r)
		}
	}
}

func TestDryRunOnlyCounts(t *testing.T) {
	store := newFakeStore(map[string]int64{"lifecycle_events": 42})
	results, err := newService(t, store, func(c *Config) { c.DryRun = true }).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != 0 || len(store.runs) != 0 || store.rows["lifecycle_events"] != 42 {
		t.Fatal("a dry run must change nothing")
	}
	for _, r := range results {
		if r.Status != "DRY_RUN" || (r.Rule == "lifecycle_events" && r.Rows != 42) {
			t.Fatalf("dry run result %+v", r)
		}
	}
}

func TestMandatoryRetentionFloors(t *testing.T) {
	store := newFakeStore(nil)
	for name, change := range map[string]func(*Config){
		"audit":       func(c *Config) { c.AuditRetention = 30 * 24 * time.Hour },
		"operational": func(c *Config) { c.OperationalRetention = time.Hour },
		"idempotency": func(c *Config) { c.IdempotencyTTL = time.Hour },
		"unknown":     func(c *Config) { c.Only = []string{"assets"} },
	} {
		cfg := Config{
			BatchSize: 100, MaxBatches: 5,
			AuditRetention: 365 * 24 * time.Hour, OperationalRetention: 30 * 24 * time.Hour, IdempotencyTTL: 7 * 24 * time.Hour,
		}
		change(&cfg)
		if _, err := NewService(store, cfg, quietLog, nil); err == nil {
			t.Errorf("%s: an unsafe configuration was accepted", name)
		}
	}
}

func TestOneFailingRuleDoesNotStopTheOthers(t *testing.T) {
	store := newFakeStore(map[string]int64{"download_tokens": 10, "lifecycle_events": 10})
	store.failOn = "download_tokens"
	results, err := newService(t, store).RunOnce(context.Background())
	if err == nil {
		t.Fatal("the failure must be reported")
	}
	statuses := map[string]string{}
	for _, r := range results {
		statuses[r.Rule] = r.Status
	}
	if statuses["download_tokens"] != "FAILED" || statuses["lifecycle_events"] != "COMPLETED" || store.rows["lifecycle_events"] != 0 {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestCancellationStopsBetweenBatches(t *testing.T) {
	store := newFakeStore(map[string]int64{"reconciliation_tasks": 1000})
	ctx, cancel := context.WithCancel(context.Background())
	store.cancel = cancel
	results, _ := newService(t, store, func(c *Config) { c.Only = []string{"reconciliation_tasks"} }).RunOnce(ctx)
	if len(results) != 1 || results[0].Status != "CANCELLED" || results[0].Batches != 1 {
		t.Fatalf("expected a cancelled run after one batch: %+v", results)
	}
	if len(store.runs) != 1 || store.runs[0].Status != "CANCELLED" {
		t.Fatal("a cancelled run must still be recorded")
	}
}
