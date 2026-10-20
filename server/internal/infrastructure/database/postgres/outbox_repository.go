package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// OutboxRepository is the transactional outbox in PostgreSQL.
type OutboxRepository struct {
	db *DB
}

var _ interfaces.OutboxRepository = (*OutboxRepository)(nil)

func NewOutboxRepository(db *DB) *OutboxRepository {
	return &OutboxRepository{db: db}
}

const outboxColumns = `
	id, event_type, schema_version, aggregate_type, aggregate_id, aggregate_sequence,
	COALESCE(tenant_id, ''), payload, COALESCE(correlation_id, ''), COALESCE(causation_id, ''), destination,
	status, attempts, next_attempt_at, last_attempt_at, COALESCE(last_error, ''),
	COALESCE(lease_owner, ''), lease_expires_at, published_at, republished, created_at, updated_at
`

func scanOutbox(row pgx.Row) (model.OutboxEvent, error) {
	var e model.OutboxEvent
	err := row.Scan(&e.ID, &e.Type, &e.SchemaVersion, &e.AggregateType, &e.AggregateID, &e.AggregateSequence,
		&e.TenantID, &e.Payload, &e.CorrelationID, &e.CausationID, &e.Destination,
		&e.Status, &e.Attempts, &e.NextAttemptAt, &e.LastAttemptAt, &e.LastError,
		&e.LeaseOwner, &e.LeaseExpiresAt, &e.PublishedAt, &e.Republished, &e.CreatedAt, &e.UpdatedAt)
	return e, err
}

// insertOutbox writes events inside the caller's transaction, so they
// commit or roll back together with the business change. Each event takes
// the next sequence number of its aggregate.
func insertOutbox(ctx context.Context, tx pgx.Tx, events ...model.OutboxEvent) error {
	for _, e := range events {
		if err := lockAggregate(ctx, tx, e.AggregateType, e.AggregateID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (
				id, event_type, schema_version, aggregate_type, aggregate_id, aggregate_sequence,
				tenant_id, payload, correlation_id, causation_id, destination,
				status, next_attempt_at, created_at, updated_at
			)
			SELECT $1, $2, $3::int, $4, $5, COALESCE(MAX(aggregate_sequence), 0) + 1,
			       NULLIF($6, ''), $7::jsonb, NULLIF($8, ''), NULLIF($9, ''), $10,
			       'PENDING', $11::timestamptz, $12::timestamptz, $12::timestamptz
			FROM outbox_events
			WHERE aggregate_type = $4 AND aggregate_id = $5
		`, e.ID, e.Type, e.SchemaVersion, e.AggregateType, e.AggregateID,
			e.TenantID, e.Payload, e.CorrelationID, e.CausationID, e.Destination,
			e.NextAttemptAt, e.CreatedAt)
		if err != nil {
			return fmt.Errorf("write outbox event %s: %w", e.Type, err)
		}
	}
	return nil
}

// lockAggregate serializes the writers of one aggregate's events until the
// transaction ends, so two of them cannot both number an event MAX+1. Not
// every writer locks a business row first (operator replays,
// reconciliation), and a collision would fail the other call, which can be
// a user's request. Statements after the lock see what the previous holder
// committed (READ COMMITTED). Taking it twice in one transaction is fine.
func lockAggregate(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2, 0))`, aggregateType, aggregateID)
	if err != nil {
		return fmt.Errorf("lock aggregate %s/%s for its event: %w", aggregateType, aggregateID, err)
	}
	return nil
}

func (r *OutboxRepository) Append(ctx context.Context, events ...model.OutboxEvent) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin outbox append: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := insertOutbox(ctx, tx, events...); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit outbox append: %w", err)
	}
	return nil
}

func (r *OutboxRepository) AppendFirst(ctx context.Context, e model.OutboxEvent) (bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin outbox append: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Held from the check to the commit, so a second writer waits and then
	// sees this event.
	if err := lockAggregate(ctx, tx, e.AggregateType, e.AggregateID); err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM outbox_events
		               WHERE aggregate_type = $1 AND aggregate_id = $2 AND event_type = $3)
	`, e.AggregateType, e.AggregateID, e.Type).Scan(&exists); err != nil {
		return false, fmt.Errorf("check for an existing %s event: %w", e.Type, err)
	}
	if exists {
		return false, nil
	}
	if err := insertOutbox(ctx, tx, e); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit outbox append: %w", err)
	}
	return true, nil
}

func (r *OutboxRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int, tenantID string) ([]model.OutboxEvent, error) {
	if owner == "" || lease <= 0 || limit <= 0 {
		return nil, errors.New("claim needs an owner, a positive lease and a positive limit")
	}
	// SKIP LOCKED keeps concurrent publishers on different rows. The NOT
	// EXISTS holds an event back while an earlier one of its aggregate is
	// still pending; a DEAD event does not block the ones after it.
	return r.query(ctx, `
		WITH due AS (
			SELECT o.id AS due_id
			FROM outbox_events AS o
			WHERE o.status = 'PENDING'
			  AND o.next_attempt_at <= $2
			  AND (o.lease_expires_at IS NULL OR o.lease_expires_at <= $2)
			  AND ($5 = '' OR o.tenant_id = $5)
			  AND NOT EXISTS (
				SELECT 1 FROM outbox_events AS earlier
				WHERE earlier.aggregate_type = o.aggregate_type
				  AND earlier.aggregate_id = o.aggregate_id
				  AND earlier.aggregate_sequence < o.aggregate_sequence
				  AND earlier.status = 'PENDING'
			  )
			ORDER BY o.next_attempt_at, o.aggregate_sequence
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events AS o
		SET lease_owner = $1,
		    lease_expires_at = $2::timestamptz + make_interval(secs => $3),
		    -- A lease that ran out means its publisher stopped mid-attempt.
		    attempts = o.attempts + CASE WHEN o.lease_owner IS NULL THEN 0 ELSE 1 END,
		    last_attempt_at = $2,
		    updated_at = $2
		FROM due
		WHERE o.id = due.due_id
		RETURNING `+outboxColumns,
		owner, now, lease.Seconds(), limit, tenantID)
}

func (r *OutboxRepository) MarkPublished(ctx context.Context, eventID, owner string, now time.Time) error {
	// The publication happened whether or not the lease ran out meanwhile,
	// so only another publisher's claim stops this write.
	return r.leased(ctx, `
		UPDATE outbox_events
		SET status = 'PUBLISHED', published_at = $3, last_error = NULL,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND status = 'PENDING'
	`, eventID, owner, now)
}

func (r *OutboxRepository) Retry(ctx context.Context, eventID, owner string, now, next time.Time, lastError string) error {
	return r.leased(ctx, `
		UPDATE outbox_events
		SET attempts = attempts + 1, next_attempt_at = $4, last_error = NULLIF($5, ''),
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND status = 'PENDING'
	`, eventID, owner, now, next, lastError)
}

func (r *OutboxRepository) Release(ctx context.Context, eventID, owner string, now, next time.Time) error {
	return r.leased(ctx, `
		UPDATE outbox_events
		SET next_attempt_at = $4, lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND status = 'PENDING'
	`, eventID, owner, now, next)
}

func (r *OutboxRepository) Dead(ctx context.Context, eventID, owner string, dl model.DeadLetter) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin outbox dead letter: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// attempts comes from the dead letter: the publisher counts the tries,
	// including one abandoned by a stopped publisher and already counted.
	tag, err := tx.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'DEAD', attempts = $5, last_error = NULLIF($3, ''),
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $4
		WHERE id = $1 AND lease_owner = $2 AND status = 'PENDING'
	`, eventID, owner, dl.LastError, dl.DeadLetteredAt, dl.Attempts)
	if err != nil {
		return fmt.Errorf("mark outbox event dead: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	if err := insertDeadLetter(ctx, tx, dl); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit outbox dead letter: %w", err)
	}
	return nil
}

func (r *OutboxRepository) Republish(ctx context.Context, eventID string, generation int, now time.Time) error {
	// Only the publication the caller looked at: a reconciler working from
	// an older read, or a second one acting on the same event, changes
	// nothing instead of sending it again and spending another generation.
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'PENDING', attempts = 0, republished = republished + 1,
		    next_attempt_at = $2, last_error = NULL, updated_at = $2
		WHERE id = $1 AND status = 'PUBLISHED' AND republished = $3
	`, eventID, now, generation)
	if err != nil {
		return fmt.Errorf("republish outbox event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrEventNotFound
	}
	return nil
}

func (r *OutboxRepository) Get(ctx context.Context, eventID string) (model.OutboxEvent, error) {
	e, err := scanOutbox(r.db.Pool.QueryRow(ctx, `SELECT `+outboxColumns+` FROM outbox_events WHERE id = $1`, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.OutboxEvent{}, model.ErrEventNotFound
	}
	if err != nil {
		return model.OutboxEvent{}, fmt.Errorf("get outbox event: %w", err)
	}
	return e, nil
}

func (r *OutboxRepository) List(ctx context.Context, status string, limit int) ([]model.OutboxEvent, error) {
	return r.query(ctx, `
		SELECT `+outboxColumns+` FROM outbox_events
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at DESC, id
		LIMIT $2
	`, status, limit)
}

func (r *OutboxRepository) Stats(ctx context.Context, now time.Time) (model.OutboxStats, error) {
	var stats model.OutboxStats
	var oldest *time.Time
	// Separate counts, so each one can use its partial index.
	err := r.db.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM outbox_events WHERE status = 'PENDING'),
		       (SELECT min(created_at) FROM outbox_events WHERE status = 'PENDING'),
		       (SELECT count(*) FROM outbox_events WHERE status = 'PENDING' AND lease_expires_at <= $1),
		       (SELECT count(*) FROM outbox_events WHERE status = 'DEAD')
	`, now).Scan(&stats.Pending, &oldest, &stats.ExpiredLeases, &stats.Dead)
	if err != nil {
		return model.OutboxStats{}, fmt.Errorf("read outbox stats: %w", err)
	}
	if oldest != nil {
		stats.OldestPending = now.Sub(*oldest)
	}
	return stats, nil
}

func (r *OutboxRepository) Unconsumed(ctx context.Context, from, to time.Time, limit int, tenantID string) ([]model.OutboxEvent, error) {
	// Each queue has one consumer, named after it: the event's destination.
	return r.query(ctx, `
		SELECT `+outboxColumns+` FROM outbox_events AS o
		WHERE o.status = 'PUBLISHED'
		  AND o.published_at > $1 AND o.published_at <= $2
		  AND ($4 = '' OR o.tenant_id = $4)
		  AND NOT EXISTS (SELECT 1 FROM event_inbox AS i WHERE i.consumer = o.destination AND i.event_id = o.id)
		  -- An open dead letter is being looked at; a resolved, discarded or
		  -- replayed one is an operator's decision that must stick. Only a
		  -- RETRIED one, a deliberate resend, leaves the event to this check.
		  AND NOT EXISTS (SELECT 1 FROM dead_letters AS d WHERE d.event_id = o.id AND d.status <> 'RETRIED')
		ORDER BY o.published_at
		LIMIT $3
	`, from, to, limit, tenantID)
}

func (r *OutboxRepository) CompletedUploadsWithoutAsset(ctx context.Context, from, to time.Time, limit int, tenantID string) ([]model.UploadFollowUp, error) {
	// Uploads with no event come first: they are the ones to repair, and a
	// backlog of uploads whose event is still on its way must not fill the
	// batch and keep them from being looked at.
	rows, err := r.db.Pool.Query(ctx, `
		SELECT f.tenant_id, f.id, f.event_id
		FROM (
			SELECT s.tenant_id, s.id, s.updated_at,
			       COALESCE((SELECT o.id FROM outbox_events AS o
			                 WHERE o.aggregate_type = 'upload' AND o.aggregate_id = s.id
			                   AND o.event_type = 'upload.completed'
			                 ORDER BY o.aggregate_sequence DESC
			                 LIMIT 1), '') AS event_id
			FROM upload_sessions AS s
			WHERE s.status IN ('COMPLETED', 'VERIFIED')
			  AND s.updated_at > $1 AND s.updated_at <= $2
			  AND ($4 = '' OR s.tenant_id = $4)
			  AND NOT EXISTS (SELECT 1 FROM assets AS a WHERE a.upload_id = s.id)
		) AS f
		ORDER BY f.event_id <> '', f.updated_at
		LIMIT $3
	`, from, to, limit, tenantID)
	if err != nil {
		return nil, fmt.Errorf("find completed uploads without asset: %w", err)
	}
	defer rows.Close()

	var out []model.UploadFollowUp
	for rows.Next() {
		var f model.UploadFollowUp
		if err := rows.Scan(&f.TenantID, &f.UploadID, &f.EventID); err != nil {
			return nil, fmt.Errorf("scan upload follow-up: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// leased runs an update that must hit exactly the leased event.
func (r *OutboxRepository) leased(ctx context.Context, sql string, args ...any) error {
	tag, err := r.db.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("update outbox event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *OutboxRepository) query(ctx context.Context, sql string, args ...any) ([]model.OutboxEvent, error) {
	rows, err := r.db.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query outbox: %w", err)
	}
	defer rows.Close()

	var events []model.OutboxEvent
	for rows.Next() {
		e, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan outbox event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
