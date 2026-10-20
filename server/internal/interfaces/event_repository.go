package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// OutboxRepository keeps events written in the same transaction as the
// business change that caused them, and leases them to publishers. Every
// method that takes an owner changes the event only while owner holds its
// lease, and returns model.ErrLeaseLost otherwise.
type OutboxRepository interface {
	// Append writes events in a transaction of their own, for events that
	// accompany no business change: a reconciliation repair, an operator
	// replay or a test event.
	Append(ctx context.Context, events ...model.OutboxEvent) error
	// AppendFirst writes e unless its aggregate already has an event of the
	// same type, and reports whether it did. Two reconcilers that find the
	// same missing event emit it once between them.
	AppendFirst(ctx context.Context, e model.OutboxEvent) (bool, error)

	// Claim leases up to limit due PENDING events, of one tenant when
	// tenantID is not empty. An event is claimable only once no earlier
	// event of its aggregate is still pending, so each aggregate's events
	// leave in order. An expired lease (a publisher that stopped mid-way)
	// is claimable again and counts as a failed attempt.
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int, tenantID string) ([]model.OutboxEvent, error)
	// MarkPublished records that the queue accepted the event.
	MarkPublished(ctx context.Context, eventID, owner string, now time.Time) error
	// Retry counts a failed attempt and releases the event until next.
	Retry(ctx context.Context, eventID, owner string, now, next time.Time, lastError string) error
	// Release gives the event back until next without counting an attempt,
	// for an event that was claimed but never tried.
	Release(ctx context.Context, eventID, owner string, now, next time.Time) error
	// Dead ends the event as DEAD after dl.Attempts tries and records its
	// dead letter, in one transaction.
	Dead(ctx context.Context, eventID, owner string, dl model.DeadLetter) error

	// Republish sends a PUBLISHED event again under its own identity: it
	// becomes PENDING with a fresh attempt budget and a new publication
	// generation, so the queue does not drop it as a duplicate. It acts only
	// on the given generation (the Republished count the caller read).
	// model.ErrEventNotFound when no event with that ID is published at it.
	Republish(ctx context.Context, eventID string, generation int, now time.Time) error

	Get(ctx context.Context, eventID string) (model.OutboxEvent, error)
	// List returns events with status ("" for any), newest first.
	List(ctx context.Context, status string, limit int) ([]model.OutboxEvent, error)
	Stats(ctx context.Context, now time.Time) (model.OutboxStats, error)

	// Unconsumed lists events published between from and to that their
	// destination's consumer has never received and that have no open
	// dead letter.
	Unconsumed(ctx context.Context, from, to time.Time, limit int, tenantID string) ([]model.OutboxEvent, error)
	// CompletedUploadsWithoutAsset lists uploads that completed between
	// from and to and still have no asset, with their latest
	// upload.completed event if they have one.
	CompletedUploadsWithoutAsset(ctx context.Context, from, to time.Time, limit int, tenantID string) ([]model.UploadFollowUp, error)
}

// InboxRepository records each consumer's processing of each event. Every
// method that takes an owner changes the record only while owner holds its
// lease, and returns model.ErrLeaseLost otherwise.
type InboxRepository interface {
	// Claim takes the event in rec for processing by owner until
	// now+lease and counts the attempt. It returns what it found:
	// model.ClaimAcquired for a new event, a failed one whose retry is due
	// or one whose previous lease expired; ClaimDone, ClaimBusy (another
	// worker holds it, or its retry is not due yet) or ClaimDead otherwise.
	// The record is returned as it now is.
	Claim(ctx context.Context, rec model.InboxRecord, owner string, now time.Time, lease time.Duration) (string, model.InboxRecord, error)
	// NewerHandled returns the sequence of a later event of the same
	// aggregate that the consumer already completed or skipped, or 0.
	NewerHandled(ctx context.Context, consumer, aggregateType, aggregateID string, sequence int64) (int64, error)
	Extend(ctx context.Context, consumer, eventID, owner string, now time.Time, lease time.Duration) error
	// Complete records success. An open dead letter for the event and
	// consumer (one escalated while the event looked lost) is resolved in
	// the same transaction.
	Complete(ctx context.Context, consumer, eventID, owner string, now time.Time, outcome string) error
	// Skip records an event that needed no action: stale, or no longer
	// applicable.
	Skip(ctx context.Context, consumer, eventID, owner string, now time.Time, reason string) error
	// Fail records a failed attempt; the next delivery after next retries.
	Fail(ctx context.Context, consumer, eventID, owner string, now, next time.Time, lastError string) error
	// Dead ends the event as DEAD for the consumer and records its dead
	// letter, in one transaction.
	Dead(ctx context.Context, consumer, eventID, owner string, dl model.DeadLetter) error
	// Bury does the same without a lease, for an event the queue moved to
	// its dead-letter queue; the record is created when missing. It returns
	// the record's status: DEAD once buried. A finished record, or one
	// another worker is processing right now, is left alone.
	Bury(ctx context.Context, rec model.InboxRecord, dl model.DeadLetter) (string, error)
	// Abandon marks a stalled record FAILED and due now, so the next
	// delivery of the event takes it.
	Abandon(ctx context.Context, consumer, eventID string, now time.Time, note string) error

	Get(ctx context.Context, consumer, eventID string) (model.InboxRecord, error)
	// List returns a consumer's records ("" for every consumer) with status
	// ("" for any), most recently updated first.
	List(ctx context.Context, consumer, status string, limit int) ([]model.InboxRecord, error)
	// Stalled lists unfinished records untouched since before whose lease
	// expired or whose retry was due by then, and that have no open dead
	// letter.
	Stalled(ctx context.Context, before time.Time, limit int, tenantID string) ([]model.InboxRecord, error)
}

// DeadLetterRepository is the one authoritative record of events that could
// not be published or consumed.
type DeadLetterRepository interface {
	// Add records a dead letter. While one is open for the same event and
	// consumer, that one is updated instead.
	Add(ctx context.Context, dl model.DeadLetter) error
	Get(ctx context.Context, id string) (model.DeadLetter, error)
	// List returns dead letters with status ("" for any), newest first.
	List(ctx context.Context, status string, limit int) ([]model.DeadLetter, error)
	// ListOpenAfter pages through open dead letters in ID order, of one
	// tenant when tenantID is not empty.
	ListOpenAfter(ctx context.Context, afterID string, limit int, tenantID string) ([]model.DeadLetter, error)
	Stats(ctx context.Context, now time.Time) (model.DeadLetterStats, error)

	// Retry closes an open dead letter as RETRIED and, in the same
	// transaction, sends the event again under its own identity: the
	// outbox row becomes PENDING in a new publication generation, and the
	// consumer's DEAD inbox record becomes FAILED with a fresh budget.
	// model.ErrNotReplayable when the outbox no longer has the event.
	Retry(ctx context.Context, id, actor, note string, now time.Time) error
	// Replay closes it as REPLAYED and appends replay, the event under a
	// new identity, in the same transaction.
	Replay(ctx context.Context, id, actor, note string, replay model.OutboxEvent, now time.Time) error
	// Close resolves or discards it, with the operator's reason.
	Close(ctx context.Context, id, status, actor, note string, now time.Time) error
	// MarkSourceChanged notes that the business state the dead letter is
	// about has moved on since.
	MarkSourceChanged(ctx context.Context, id, note string, now time.Time) error
}

// DiscrepancyRepository records what event reconciliation found and did.
type DiscrepancyRepository interface {
	Record(ctx context.Context, d model.EventDiscrepancy) error
	// List returns the newest discrepancies first.
	List(ctx context.Context, limit int) ([]model.EventDiscrepancy, error)
}
