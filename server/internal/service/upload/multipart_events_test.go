package upload

import (
	"context"
	"errors"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
)

func TestCompletionWritesUploadCompletedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := correlation.With(context.Background(), "req-1")
	session, etag1, etag2 := h.uploadedSession(t, "events-1")

	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// A retried request and the verification that follows write nothing more.
	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); err != nil {
		t.Fatalf("retried complete: %v", err)
	}
	if _, err := h.svc.VerifyIntegrity(ctx, testTenant, session.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}

	outbox := h.uploads.Outbox()
	if len(outbox) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(outbox))
	}
	e := outbox[0]
	if e.Type != event.TypeUploadCompleted || e.AggregateID != session.ID || e.TenantID != testTenant ||
		e.CorrelationID != "req-1" || e.Destination != event.QueueProcessing || e.Status != model.OutboxPending {
		t.Fatalf("unexpected event %+v", e)
	}
}

func TestCompletionRecoveredByReconciliationWritesTheEventThen(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	session, etag1, etag2 := h.uploadedSession(t, "events-2")
	h.svc.uploads = &failOnceTransitionTo{UploadRepository: h.uploads, to: model.UploadStatusCompleted}

	if _, err := h.svc.Complete(ctx, testTenant, testOwner, session.ID, completeInput(etag1, etag2)); !errors.Is(err, ErrOperationPending) {
		t.Fatalf("expected pending, got %v", err)
	}
	if n := len(h.uploads.Outbox()); n != 0 {
		t.Fatalf("a completion that did not commit must leave no event, got %d", n)
	}

	if status, err := h.svc.ResolveCompletion(ctx, testTenant, session.ID); err != nil || status != model.UploadStatusCompleted {
		t.Fatalf("resolve: %s %v", status, err)
	}
	if outbox := h.uploads.Outbox(); len(outbox) != 1 || outbox[0].AggregateID != session.ID {
		t.Fatalf("reconciliation's completion must write the event, got %+v", outbox)
	}
}
