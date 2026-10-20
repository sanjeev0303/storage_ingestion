package asset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

func TestDeletionAndHoldReleaseWriteTheirEvents(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	svc := e.service("worker-a")
	ctx := correlation.With(context.Background(), "req-7")

	result, err := svc.RequestDeletion(ctx, owner, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestDeletion(ctx, owner, "a1"); err != nil {
		t.Fatal(err)
	}
	outbox := e.deletions.Outbox()
	if len(outbox) != 1 {
		t.Fatalf("one deletion, one event; got %d", len(outbox))
	}
	scheduled := outbox[0]
	var payload event.DeletionScheduled
	_ = json.Unmarshal(scheduled.Payload, &payload)
	if scheduled.Type != event.TypeDeletionScheduled || scheduled.AggregateType != event.AggregateAsset ||
		scheduled.AggregateID != "a1" || scheduled.CorrelationID != "req-7" || payload.OperationID != result.Operation.ID {
		t.Fatalf("unexpected event %+v (%+v)", scheduled, payload)
	}

	hold, err := svc.PlaceHold(ctx, model.LegalHold{TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "a1", Reason: "case", PlacedBy: "legal"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ReleaseHold(ctx, "tenant-1", hold.ID, "legal"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReleaseHold(ctx, "tenant-1", hold.ID, "legal"); !errors.Is(err, model.ErrHoldNotFound) {
		t.Fatalf("a second release must fail, got %v", err)
	}
	released := e.holds.Outbox()
	if len(released) != 1 || released[0].Type != event.TypeHoldReleased || released[0].AggregateID != hold.ID {
		t.Fatalf("expected one legal_hold.released, got %+v", released)
	}
}

func TestRunDeletionActsOnlyOnADueLiveDeletion(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	svc := e.service("worker-a")
	ctx := context.Background()
	result, err := svc.RequestDeletion(ctx, owner, "a1")
	if err != nil {
		t.Fatal(err)
	}
	id := result.Operation.ID

	note, err := svc.RunDeletion(ctx, "tenant-1", id)
	if err != nil || !strings.Contains(note, "not due") || e.objects("a1") != 2 {
		t.Fatalf("during the grace period nothing happens: %q %v", note, err)
	}

	e.clock = e.clock.Add(grace)
	note, err = svc.RunDeletion(ctx, "tenant-1", id)
	if err != nil || note != "deletion completed" || e.objects("a1") != 0 || e.status("a1") != model.AssetStatusDeleted {
		t.Fatalf("a due deletion runs at once: %q %v, asset %s", note, err, e.status("a1"))
	}

	// The same event delivered again finds the deletion finished.
	if _, err := svc.RunDeletion(ctx, "tenant-1", id); !errors.Is(err, model.ErrStaleEvent) {
		t.Fatalf("a finished deletion makes the event stale, got %v", err)
	}
	if _, err := svc.RunDeletion(ctx, "tenant-1", "no-such-op"); !retry.IsPermanent(err) {
		t.Fatalf("an unknown deletion can never succeed, got %v", err)
	}
}

func TestRunDeletionAfterRestoreIsStale(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	svc := e.service("worker-a")
	ctx := context.Background()
	result, err := svc.RequestDeletion(ctx, owner, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(ctx, owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(2 * grace)
	if _, err := svc.RunDeletion(ctx, "tenant-1", result.Operation.ID); !errors.Is(err, model.ErrStaleEvent) {
		t.Fatalf("a delayed event must not delete a restored asset, got %v", err)
	}
	if e.objects("a1") != 2 || e.status("a1") != model.AssetStatusAvailable {
		t.Fatal("the restored asset must be untouched")
	}
}

func TestResumeAfterHoldMakesOnlyItsDeletionsDue(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	e.published("a2", e.clock)
	e.assets.Update("a1", func(a *model.Asset) { a.Dataset = "reports" })
	svc := e.service("worker-a")
	ctx := context.Background()
	for _, id := range []string{"a1", "a2"} {
		if _, err := svc.RequestDeletion(ctx, owner, id); err != nil {
			t.Fatal(err)
		}
	}
	datasetHold, err := svc.PlaceHold(ctx, model.LegalHold{TenantID: "tenant-1", Scope: model.ScopeDataset, ScopeKey: "reports", Reason: "audit", PlacedBy: "legal"})
	if err != nil {
		t.Fatal(err)
	}
	assetHold, err := svc.PlaceHold(ctx, model.LegalHold{TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "a2", Reason: "case", PlacedBy: "legal"})
	if err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(grace)
	if stats := e.run(svc); stats.Protected != 2 {
		t.Fatalf("both deletions are held: %+v", stats)
	}

	// Still active: nothing to resume.
	if note, err := svc.ResumeAfterHold(ctx, "tenant-1", datasetHold.ID); err != nil || !strings.HasPrefix(note, "0 ") {
		t.Fatalf("an active hold resumes nothing: %q %v", note, err)
	}
	if err := svc.ReleaseHold(ctx, "tenant-1", datasetHold.ID, "legal"); err != nil {
		t.Fatal(err)
	}
	if note, err := svc.ResumeAfterHold(ctx, "tenant-1", datasetHold.ID); err != nil || !strings.HasPrefix(note, "1 ") {
		t.Fatalf("the released hold's deletion becomes due: %q %v", note, err)
	}

	// No recheck interval has passed, yet the resumed deletion runs; the
	// other one keeps its own hold.
	if stats := e.run(svc); stats.Completed != 1 || e.objects("a1") != 0 || e.objects("a2") != 2 {
		t.Fatalf("only a1 may be deleted: %+v", stats)
	}
	if e.op("a2").State != model.DeletionProtected || assetHold.ID == "" {
		t.Fatal("a2 stays protected by its own hold")
	}
}
