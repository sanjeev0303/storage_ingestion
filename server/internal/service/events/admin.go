package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Admin runs the operator's dead-letter workflow. Nothing is sent again
// without a reason, an actor and a fresh check of the business state.
type Admin struct {
	outbox interfaces.OutboxRepository
	inbox  interfaces.InboxRepository
	dead   interfaces.DeadLetterRepository
	checks Checks
	now    func() time.Time
}

func NewAdmin(outbox interfaces.OutboxRepository, inbox interfaces.InboxRepository, dead interfaces.DeadLetterRepository, checks Checks) *Admin {
	return &Admin{outbox: outbox, inbox: inbox, dead: dead, checks: checks, now: func() time.Time { return time.Now().UTC() }}
}

// DeadLetterDetail is what an operator sees of one dead letter: its
// failure history and the event's metadata, never the event's payload.
type DeadLetterDetail struct {
	DeadLetter model.DeadLetter
	// Event is the outbox row, when the outbox still has it.
	Event *model.OutboxEvent
	// Inbox is the consumer's record of the event, when it has one.
	Inbox *model.InboxRecord
	// Stale says why sending the event again would be refused.
	Stale string
}

func (a *Admin) Inspect(ctx context.Context, id string) (DeadLetterDetail, error) {
	dl, err := a.dead.Get(ctx, id)
	if err != nil {
		return DeadLetterDetail{}, err
	}
	detail := DeadLetterDetail{DeadLetter: dl}

	if dl.Consumer != "" {
		rec, err := a.inbox.Get(ctx, dl.Consumer, dl.EventID)
		if err == nil {
			detail.Inbox = &rec
		} else if !errors.Is(err, model.ErrEventNotFound) {
			return DeadLetterDetail{}, err
		}
	}
	e, err := a.outbox.Get(ctx, dl.EventID)
	if errors.Is(err, model.ErrEventNotFound) {
		detail.Stale = "the outbox has no event to send again"
		return detail, nil
	}
	if err != nil {
		return DeadLetterDetail{}, err
	}
	detail.Event = &e
	detail.Stale, err = a.checks.Stale(ctx, event.FromOutbox(e))
	if err != nil {
		detail.Stale = "cannot check: " + err.Error()
	}
	return detail, nil
}

// Retry sends the event again under its original identity, after the
// underlying problem was fixed. The consumer's inbox still deduplicates
// it, so this is safe only because the event is checked to be current.
func (a *Admin) Retry(ctx context.Context, id, actor, reason string) error {
	if _, err := a.current(ctx, id, actor, reason); err != nil {
		return err
	}
	return a.dead.Retry(ctx, id, actor, reason, a.now())
}

// Replay sends the event again under a new identity, for an operator who
// means a new logical operation. The copy names the original as its cause
// and comes after every earlier event of its aggregate. It returns the new
// event's ID.
func (a *Admin) Replay(ctx context.Context, id, actor, reason string) (string, error) {
	original, err := a.current(ctx, id, actor, reason)
	if err != nil {
		return "", err
	}
	replay := event.Replay(original, a.now())
	if err := a.dead.Replay(ctx, id, actor, reason, replay, a.now()); err != nil {
		return "", err
	}
	return replay.ID, nil
}

// Resolve closes a dead letter whose problem was handled another way.
func (a *Admin) Resolve(ctx context.Context, id, actor, reason string) error {
	if err := needReason(actor, reason); err != nil {
		return err
	}
	return a.dead.Close(ctx, id, model.DeadLetterResolved, actor, reason, a.now())
}

// Discard closes a dead letter that is intentionally dropped.
func (a *Admin) Discard(ctx context.Context, id, actor, reason string) error {
	if err := needReason(actor, reason); err != nil {
		return err
	}
	return a.dead.Close(ctx, id, model.DeadLetterDiscarded, actor, reason, a.now())
}

// current returns the dead letter's event once it is checked to be open,
// still in the outbox and still wanted by the business state.
func (a *Admin) current(ctx context.Context, id, actor, reason string) (model.OutboxEvent, error) {
	if err := needReason(actor, reason); err != nil {
		return model.OutboxEvent{}, err
	}
	dl, err := a.dead.Get(ctx, id)
	if err != nil {
		return model.OutboxEvent{}, err
	}
	if dl.Status != model.DeadLetterOpen {
		return model.OutboxEvent{}, model.ErrDeadLetterClosed
	}
	e, err := a.outbox.Get(ctx, dl.EventID)
	if errors.Is(err, model.ErrEventNotFound) {
		return model.OutboxEvent{}, model.ErrNotReplayable
	}
	if err != nil {
		return model.OutboxEvent{}, err
	}
	stale, err := a.checks.Stale(ctx, event.FromOutbox(e))
	if err != nil {
		return model.OutboxEvent{}, fmt.Errorf("check the event is still current: %w", err)
	}
	if stale != "" {
		return model.OutboxEvent{}, fmt.Errorf("%w: %s; resolve or discard it instead", model.ErrStaleEvent, stale)
	}
	return e, nil
}

func needReason(actor, reason string) error {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("an actor and a reason are required: they are kept with the dead letter")
	}
	return nil
}
