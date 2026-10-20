package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// What event reconciliation looks for.
const (
	// KindNativeDeadLetter: the queue moved a message to its own
	// dead-letter queue; it is recorded in dead_letters and removed there.
	KindNativeDeadLetter = "native_dead_letter"
	// KindUnconsumed: published, but the consumer never received it.
	KindUnconsumed = "unconsumed"
	// KindInboxStalled: a consumer stopped mid-way, or a retry was due long
	// ago, and the message never came back.
	KindInboxStalled = "inbox_stalled"
	// KindMissingEvent: a completed upload with neither an asset nor an
	// upload.completed event.
	KindMissingEvent = "missing_event"
	// KindInconsistent: an event was handled but the work it should have
	// started does not exist.
	KindInconsistent = "inconsistent"
	// KindSourceChanged: an open dead letter whose business state moved on.
	KindSourceChanged = "dead_letter_source_changed"
)

// deadLetterCursor names where the dead-letter check continues; a run
// limited to one tenant keeps its own.
const deadLetterCursor = "events.dead_letters"

type ReconcilerConfig struct {
	// Queues are drained of their native dead letters and measured.
	Queues []string
	// StuckAfter is how long an event may wait at any step before
	// reconciliation acts on it.
	StuckAfter time.Duration
	// Window bounds how far back published events and completed uploads
	// are checked.
	Window time.Duration
	// MaxRepublish is how often an event no consumer received is sent
	// again before it is dead-lettered.
	MaxRepublish int
	BatchSize    int
	TenantID     string
	// DryRun reports what reconciliation would do and changes nothing.
	DryRun bool
}

// Reconciler finds events stuck between the outbox, the queue and the
// inbox, repairs what it can decide safely, and leaves the rest visible.
type Reconciler struct {
	outbox        interfaces.OutboxRepository
	inbox         interfaces.InboxRepository
	dead          interfaces.DeadLetterRepository
	discrepancies interfaces.DiscrepancyRepository
	cursors       interfaces.CursorRepository
	queue         interfaces.MessageQueue
	checks        Checks
	cfg           ReconcilerConfig
	log           *slog.Logger
	metrics       *metrics.Registry
	now           func() time.Time
}

func NewReconciler(outbox interfaces.OutboxRepository, inbox interfaces.InboxRepository, dead interfaces.DeadLetterRepository,
	discrepancies interfaces.DiscrepancyRepository, cursors interfaces.CursorRepository, queue interfaces.MessageQueue,
	checks Checks, cfg ReconcilerConfig, log *slog.Logger, registry *metrics.Registry) (*Reconciler, error) {
	if outbox == nil || inbox == nil || dead == nil || discrepancies == nil || cursors == nil || queue == nil || log == nil {
		return nil, errors.New("reconciler dependencies are required")
	}
	if cfg.StuckAfter <= 0 || cfg.Window <= cfg.StuckAfter || cfg.MaxRepublish < 0 || cfg.BatchSize <= 0 {
		return nil, errors.New("reconciler needs a positive stuck-after, a longer window and a positive batch size")
	}
	return &Reconciler{
		outbox: outbox, inbox: inbox, dead: dead, discrepancies: discrepancies, cursors: cursors, queue: queue,
		checks: checks, cfg: cfg, log: log, metrics: registry,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// ReconcileReport is what one pass found and did, or would do.
type ReconcileReport struct {
	Outbox      model.OutboxStats
	DeadLetters model.DeadLetterStats
	Queues      map[string]interfaces.QueueStats
	// Findings are the repairs made and the problems left for an operator.
	Findings []model.EventDiscrepancy
}

// RunOnce runs every check once, in bounded batches. A failing check is
// reported and does not stop the others. No transaction is held while the
// queue is called.
func (r *Reconciler) RunOnce(ctx context.Context) (ReconcileReport, error) {
	report := ReconcileReport{Queues: map[string]interfaces.QueueStats{}}
	// Draining comes first, so a redriven event is not mistaken for a
	// stalled one and sent again.
	steps := []func(context.Context, *ReconcileReport) error{
		r.drainDeadLetterQueues,
		r.republishUnconsumed,
		r.recoverStalledInbox,
		r.emitMissingEvents,
		r.checkDeadLetters,
		r.measure,
	}
	var errs []error
	for _, step := range steps {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, step(ctx, &report))
	}
	return report, errors.Join(errs...)
}

func (r *Reconciler) drainDeadLetterQueues(ctx context.Context, report *ReconcileReport) error {
	if r.cfg.DryRun {
		return nil // measure reports their depth
	}
	var errs []error
	for _, queue := range r.cfg.Queues {
		drained := 0
		for drained < r.cfg.BatchSize {
			messages, err := r.queue.ReceiveDeadLetters(ctx, queue, min(10, r.cfg.BatchSize-drained), time.Minute)
			if err != nil {
				errs = append(errs, fmt.Errorf("receive dead letters of %s: %w", queue, err))
				break
			}
			if len(messages) == 0 {
				break
			}
			for _, m := range messages {
				drained++
				if err := r.drainOne(ctx, queue, m, report); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// drainOne records one native dead letter in dead_letters, then removes it
// from the queue. An event the consumer already finished needs no dead
// letter: its acknowledgement was lost.
func (r *Reconciler) drainOne(ctx context.Context, queue string, m interfaces.QueueMessage, report *ReconcileReport) error {
	env, decodeErr := event.Decode(m.Body)
	category := model.FailureRedrive
	reason := fmt.Sprintf("the queue gave up after %d deliveries", m.ReceiveCount)
	if decodeErr != nil {
		category, reason = failureCategory(decodeErr), retry.Sanitize(decodeErr)
	}
	dl := deadLetterOf(env, model.SourceNativeDLQ, queue, category, reason, m.ReceiveCount, r.now())
	finding := r.finding(KindNativeDeadLetter, env, queue, reason)
	finding.Action, finding.Outcome = "record in dead_letters", model.DiscrepancyRepaired

	if env.ID == "" {
		dl.EventID, dl.MessageDigest = unreadable(m)
		finding.EventID = dl.EventID
		if err := r.dead.Add(ctx, dl); err != nil {
			return fmt.Errorf("record native dead letter: %w", err)
		}
	} else {
		status, err := r.inbox.Bury(ctx, inboxRecordOf(queue, env), dl)
		if err != nil {
			return fmt.Errorf("record native dead letter: %w", err)
		}
		switch status {
		case model.InboxProcessing:
			return nil // a consumer is working on it right now; next pass
		case model.InboxCompleted, model.InboxSkipped:
			finding.Detail = "redriven after it was handled: its acknowledgement was lost"
			finding.Action, finding.Outcome = "remove from the dead-letter queue", model.DiscrepancyReported
		}
	}

	if err := r.queue.AckDeadLetter(ctx, queue, m.Receipt); err != nil {
		return fmt.Errorf("remove native dead letter: %w", err)
	}
	r.record(ctx, report, finding)
	return nil
}

func (r *Reconciler) republishUnconsumed(ctx context.Context, report *ReconcileReport) error {
	now := r.now()
	events, err := r.outbox.Unconsumed(ctx, now.Add(-r.cfg.Window), now.Add(-r.cfg.StuckAfter), r.cfg.BatchSize, r.cfg.TenantID)
	if err != nil {
		return fmt.Errorf("find unconsumed events: %w", err)
	}
	for _, e := range events {
		env := event.FromOutbox(e)
		finding := r.finding(KindUnconsumed, env, e.Destination,
			fmt.Sprintf("published %d time(s), last at %s, and %s never received it",
				e.Republished+1, e.PublishedAt.Format(time.RFC3339), e.Destination))

		if e.Republished >= r.cfg.MaxRepublish {
			finding.Action, finding.Outcome = "dead-letter", model.DiscrepancyRepaired
			if !r.cfg.DryRun {
				dl := deadLetterOf(env, model.SourceConsumer, e.Destination, model.FailureUnconsumed,
					finding.Detail, e.Republished+1, now)
				finding.Outcome = outcomeOf(r.dead.Add(ctx, dl), &finding)
			}
		} else {
			finding.Action, finding.Outcome = "republish", model.DiscrepancyRepaired
			if !r.cfg.DryRun {
				finding.Outcome = outcomeOf(r.outbox.Republish(ctx, e.ID, e.Republished, now), &finding)
			}
		}
		r.record(ctx, report, finding)
	}
	return nil
}

func (r *Reconciler) recoverStalledInbox(ctx context.Context, report *ReconcileReport) error {
	now := r.now()
	records, err := r.inbox.Stalled(ctx, now.Add(-r.cfg.StuckAfter), r.cfg.BatchSize, r.cfg.TenantID)
	if err != nil {
		return fmt.Errorf("find stalled inbox records: %w", err)
	}
	for _, rec := range records {
		finding := model.EventDiscrepancy{
			Kind: KindInboxStalled, EventID: rec.EventID, Consumer: rec.Consumer, TenantID: rec.TenantID,
			AggregateType: rec.AggregateType, AggregateID: rec.AggregateID,
			Detail: fmt.Sprintf("%s since %s after %d attempt(s); the message did not come back",
				rec.Status, rec.UpdatedAt.Format(time.RFC3339), rec.Attempts),
			Action: "mark failed and republish", Outcome: model.DiscrepancyRepaired,
		}
		published, err := r.outbox.Get(ctx, rec.EventID)
		switch {
		case errors.Is(err, model.ErrEventNotFound):
			finding.Detail += "; the outbox has no published event to send again"
			finding.Action, finding.Outcome = "none: left for an operator", model.DiscrepancyReported

		case err != nil:
			finding.Outcome = outcomeOf(err, &finding)

		case published.Republished >= r.cfg.MaxRepublish:
			// Sent again as often as allowed and still not received: stop
			// and leave it to an operator. The open dead letter keeps this
			// record out of later passes.
			finding.Action = "dead-letter"
			if !r.cfg.DryRun {
				dl := deadLetterOf(event.FromOutbox(published), model.SourceConsumer, rec.Consumer, model.FailureUnconsumed,
					finding.Detail, rec.Attempts, now)
				finding.Outcome = outcomeOf(r.dead.Add(ctx, dl), &finding)
			}

		case !r.cfg.DryRun:
			err := r.inbox.Abandon(ctx, rec.Consumer, rec.EventID, now, "delivery lost; republished by reconciliation")
			if err == nil {
				err = r.outbox.Republish(ctx, rec.EventID, published.Republished, now)
			}
			if errors.Is(err, model.ErrEventNotFound) {
				finding.Detail += "; the outbox has no published event to send again"
				finding.Action, finding.Outcome = "none: left for an operator", model.DiscrepancyReported
			} else {
				finding.Outcome = outcomeOf(err, &finding)
			}
		}
		r.record(ctx, report, finding)
	}
	return nil
}

func (r *Reconciler) emitMissingEvents(ctx context.Context, report *ReconcileReport) error {
	now := r.now()
	uploads, err := r.outbox.CompletedUploadsWithoutAsset(ctx, now.Add(-r.cfg.Window), now.Add(-r.cfg.StuckAfter), r.cfg.BatchSize, r.cfg.TenantID)
	if err != nil {
		return fmt.Errorf("find completed uploads without asset: %w", err)
	}
	for _, u := range uploads {
		if u.EventID == "" {
			e := event.NewUploadCompleted(u.TenantID, u.UploadID, now, "")
			finding := r.finding(KindMissingEvent, event.FromOutbox(e), event.QueueProcessing,
				"completed upload has neither an asset nor an upload.completed event")
			finding.Action, finding.Outcome = "emit upload.completed", model.DiscrepancyRepaired
			if !r.cfg.DryRun {
				added, err := r.outbox.AppendFirst(ctx, e)
				finding.Outcome = outcomeOf(err, &finding)
				if err == nil && !added {
					finding.Detail += "; another reconciler emitted it first"
					finding.Action, finding.Outcome = "none", model.DiscrepancyReported
				}
			}
			r.record(ctx, report, finding)
			continue
		}

		// The event exists. While it is on its way the other checks watch
		// it; handled without an asset is a contradiction to look at.
		rec, err := r.inbox.Get(ctx, event.QueueProcessing, u.EventID)
		if err != nil || (rec.Status != model.InboxCompleted && rec.Status != model.InboxSkipped) {
			continue
		}
		r.record(ctx, report, model.EventDiscrepancy{
			Kind: KindInconsistent, EventID: u.EventID, Consumer: event.QueueProcessing, TenantID: u.TenantID,
			AggregateType: event.AggregateUpload, AggregateID: u.UploadID,
			Detail: fmt.Sprintf("upload.completed was %s (%s) but the upload has no asset", rec.Status, rec.Outcome),
			Action: "none: left for an operator", Outcome: model.DiscrepancyReported,
		})
	}
	return nil
}

// checkDeadLetters pages through open dead letters and marks the ones
// whose business state moved on, so an operator can discard them. Nothing
// is resolved automatically.
func (r *Reconciler) checkDeadLetters(ctx context.Context, report *ReconcileReport) error {
	cursor := deadLetterCursor
	if r.cfg.TenantID != "" {
		cursor += ":" + r.cfg.TenantID
	}
	after, err := r.cursors.Get(ctx, cursor)
	if err != nil {
		return err
	}
	letters, err := r.dead.ListOpenAfter(ctx, after, r.cfg.BatchSize, r.cfg.TenantID)
	if err != nil {
		return fmt.Errorf("list open dead letters: %w", err)
	}

	var errs []error
	for _, dl := range letters {
		if dl.SourceChanged != "" {
			continue
		}
		e, err := r.outbox.Get(ctx, dl.EventID)
		if errors.Is(err, model.ErrEventNotFound) {
			continue // an unreadable message: nothing to compare with
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		stale, err := r.checks.Stale(ctx, event.FromOutbox(e))
		if err != nil || stale == "" {
			continue
		}
		finding := r.finding(KindSourceChanged, event.FromOutbox(e), dl.Consumer, stale)
		finding.Action, finding.Outcome = "mark the dead letter for an operator", model.DiscrepancyReported
		if !r.cfg.DryRun {
			if err := r.dead.MarkSourceChanged(ctx, dl.ID, stale, r.now()); err != nil {
				errs = append(errs, err)
			}
		}
		r.record(ctx, report, finding)
	}

	// A short page means the end: start from the beginning next time.
	next := ""
	if len(letters) == r.cfg.BatchSize {
		next = letters[len(letters)-1].ID
	}
	if !r.cfg.DryRun {
		errs = append(errs, r.cursors.Set(ctx, cursor, next, r.now()))
	}
	return errors.Join(errs...)
}

// measure refreshes the gauges. Expired publisher leases and old pending
// events need no repair here (the next claim takes them), but they say a
// publisher stopped, so they are logged.
func (r *Reconciler) measure(ctx context.Context, report *ReconcileReport) error {
	now := r.now()
	var errs []error

	stats, err := r.outbox.Stats(ctx, now)
	if err != nil {
		errs = append(errs, err)
	} else {
		report.Outbox = stats
		r.metrics.Set("outbox_pending", int64(stats.Pending))
		r.metrics.Set("outbox_oldest_pending_seconds", int64(stats.OldestPending.Seconds()))
		r.metrics.Set("outbox_expired_leases", int64(stats.ExpiredLeases))
		r.metrics.Set("outbox_dead", int64(stats.Dead))
		if stats.ExpiredLeases > 0 {
			r.log.Warn("outbox events have expired publisher leases; the next claim takes them again",
				"count", stats.ExpiredLeases)
		}
		if stats.OldestPending > r.cfg.StuckAfter {
			r.log.Warn("outbox events are waiting longer than expected; is the publisher running?",
				"pending", stats.Pending, "oldest", stats.OldestPending.Round(time.Second).String())
		}
	}

	dead, err := r.dead.Stats(ctx, now)
	if err != nil {
		errs = append(errs, err)
	} else {
		report.DeadLetters = dead
		r.metrics.Set("dead_letters_open", int64(dead.Open))
		r.metrics.Set("dead_letters_oldest_seconds", int64(dead.Oldest.Seconds()))
	}

	for _, queue := range r.cfg.Queues {
		qs, err := r.queue.Stats(ctx, queue)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s backlog: %w", queue, err))
			continue
		}
		report.Queues[queue] = qs
		r.metrics.Set("queue_"+queue+"_waiting", int64(qs.Waiting))
		r.metrics.Set("queue_"+queue+"_in_flight", int64(qs.InFlight))
		r.metrics.Set("queue_"+queue+"_delayed", int64(qs.Delayed))
		r.metrics.Set("queue_"+queue+"_dead_letters", int64(qs.DeadLetters))
	}
	return errors.Join(errs...)
}

func (r *Reconciler) finding(kind string, env event.Envelope, consumer, detail string) model.EventDiscrepancy {
	return model.EventDiscrepancy{
		Kind: kind, EventID: env.ID, Consumer: consumer, TenantID: env.TenantID,
		AggregateType: env.AggregateType, AggregateID: env.AggregateID, Detail: detail,
	}
}

// record keeps a finding: in the report, the log, the metrics and, unless
// this is a dry run, the event_discrepancies table.
func (r *Reconciler) record(ctx context.Context, report *ReconcileReport, d model.EventDiscrepancy) {
	d.ID = event.NewID()
	d.CreatedAt = r.now()
	if r.cfg.DryRun {
		d.Action, d.Outcome = "dry run: "+d.Action, model.DiscrepancyReported
	}
	report.Findings = append(report.Findings, d)
	r.metrics.Add("reconcile_"+d.Kind, 1)
	r.log.Info("event reconciliation", "kind", d.Kind, "event_id", d.EventID, "consumer", d.Consumer,
		"tenant_id", d.TenantID, "aggregate", d.AggregateType+"/"+d.AggregateID,
		"detail", d.Detail, "action", d.Action, "outcome", d.Outcome)
	if r.cfg.DryRun {
		return
	}
	if err := r.discrepancies.Record(ctx, d); err != nil {
		r.log.Warn("record event discrepancy", "error", retry.Sanitize(err))
	}
}

// outcomeOf turns a repair's error into the finding's outcome.
func outcomeOf(err error, d *model.EventDiscrepancy) string {
	if err == nil {
		return model.DiscrepancyRepaired
	}
	d.Detail += "; repair failed: " + retry.Sanitize(err)
	return model.DiscrepancyFailed
}
