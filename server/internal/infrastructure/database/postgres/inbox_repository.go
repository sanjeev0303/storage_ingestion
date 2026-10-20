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

// InboxRepository records each consumer's processing of each event.
type InboxRepository struct {
	db *DB
}

var _ interfaces.InboxRepository = (*InboxRepository)(nil)

func NewInboxRepository(db *DB) *InboxRepository {
	return &InboxRepository{db: db}
}

const inboxColumns = `
	consumer, event_id, event_type, schema_version, COALESCE(tenant_id, ''), aggregate_type, aggregate_id,
	aggregate_sequence, COALESCE(correlation_id, ''), status, COALESCE(outcome, ''), attempts,
	COALESCE(lease_owner, ''), lease_expires_at, next_attempt_at, COALESCE(last_error, ''),
	first_seen_at, last_attempt_at, completed_at, updated_at
`

func scanInbox(row pgx.Row) (model.InboxRecord, error) {
	var r model.InboxRecord
	err := row.Scan(&r.Consumer, &r.EventID, &r.EventType, &r.SchemaVersion, &r.TenantID, &r.AggregateType, &r.AggregateID,
		&r.AggregateSequence, &r.CorrelationID, &r.Status, &r.Outcome, &r.Attempts,
		&r.LeaseOwner, &r.LeaseExpiresAt, &r.NextAttemptAt, &r.LastError,
		&r.FirstSeenAt, &r.LastAttemptAt, &r.CompletedAt, &r.UpdatedAt)
	return r, err
}

func (r *InboxRepository) Claim(ctx context.Context, rec model.InboxRecord, owner string, now time.Time, lease time.Duration) (string, model.InboxRecord, error) {
	if owner == "" || lease <= 0 {
		return "", model.InboxRecord{}, errors.New("claim needs an owner and a positive lease")
	}
	// One statement decides it: a new event is inserted, and an existing
	// one is taken only when its retry is due or its last lease ran out.
	// A concurrent duplicate waits on the row and then finds it taken.
	claimed, err := scanInbox(r.db.Pool.QueryRow(ctx, `
		INSERT INTO event_inbox (
			consumer, event_id, event_type, schema_version, tenant_id, aggregate_type, aggregate_id,
			aggregate_sequence, correlation_id, status, attempts, lease_owner, lease_expires_at,
			first_seen_at, last_attempt_at, updated_at
		)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, NULLIF($9, ''), 'PROCESSING', 1,
		        $10, $11::timestamptz + make_interval(secs => $12), $11, $11, $11)
		ON CONFLICT (consumer, event_id) DO UPDATE
		SET status = 'PROCESSING', attempts = event_inbox.attempts + 1,
		    lease_owner = EXCLUDED.lease_owner, lease_expires_at = EXCLUDED.lease_expires_at,
		    last_attempt_at = EXCLUDED.last_attempt_at, updated_at = EXCLUDED.updated_at
		WHERE (event_inbox.status = 'FAILED'
		       AND (event_inbox.next_attempt_at IS NULL OR event_inbox.next_attempt_at <= EXCLUDED.updated_at))
		   OR (event_inbox.status = 'PROCESSING' AND event_inbox.lease_expires_at <= EXCLUDED.updated_at)
		RETURNING `+inboxColumns,
		rec.Consumer, rec.EventID, rec.EventType, rec.SchemaVersion, rec.TenantID, rec.AggregateType, rec.AggregateID,
		rec.AggregateSequence, rec.CorrelationID, owner, now, lease.Seconds()))
	if err == nil {
		return model.ClaimAcquired, claimed, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", model.InboxRecord{}, fmt.Errorf("claim inbox record: %w", err)
	}

	// The record exists and is not ours to take. Say why.
	current, err := r.Get(ctx, rec.Consumer, rec.EventID)
	if err != nil {
		return "", model.InboxRecord{}, err
	}
	switch current.Status {
	case model.InboxCompleted, model.InboxSkipped:
		return model.ClaimDone, current, nil
	case model.InboxDead:
		return model.ClaimDead, current, nil
	}
	return model.ClaimBusy, current, nil
}

func (r *InboxRepository) NewerHandled(ctx context.Context, consumer, aggregateType, aggregateID string, sequence int64) (int64, error) {
	var newer int64
	err := r.db.Pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(aggregate_sequence), 0) FROM event_inbox
		WHERE consumer = $1 AND aggregate_type = $2 AND aggregate_id = $3
		  AND aggregate_sequence > $4 AND status IN ('COMPLETED', 'SKIPPED')
	`, consumer, aggregateType, aggregateID, sequence).Scan(&newer)
	if err != nil {
		return 0, fmt.Errorf("look up newer events: %w", err)
	}
	return newer, nil
}

func (r *InboxRepository) Extend(ctx context.Context, consumer, eventID, owner string, now time.Time, lease time.Duration) error {
	return r.leased(ctx, `
		UPDATE event_inbox
		SET lease_expires_at = $4::timestamptz + make_interval(secs => $5), updated_at = $4
		WHERE consumer = $1 AND event_id = $2 AND lease_owner = $3 AND status = 'PROCESSING'
	`, consumer, eventID, owner, now, lease.Seconds())
}

func (r *InboxRepository) Complete(ctx context.Context, consumer, eventID, owner string, now time.Time, outcome string) error {
	return r.finish(ctx, consumer, eventID, owner, now, model.InboxCompleted, outcome)
}

func (r *InboxRepository) Skip(ctx context.Context, consumer, eventID, owner string, now time.Time, reason string) error {
	return r.finish(ctx, consumer, eventID, owner, now, model.InboxSkipped, reason)
}

// finish records the outcome and resolves any dead letter still open for
// the event: one escalated while the event looked lost, or a publisher
// failure whose message reached the queue after all.
func (r *InboxRepository) finish(ctx context.Context, consumer, eventID, owner string, now time.Time, status, outcome string) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin inbox completion: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, `
		UPDATE event_inbox
		SET status = $5, outcome = NULLIF($6, ''), completed_at = $4, next_attempt_at = NULL,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $4
		WHERE consumer = $1 AND event_id = $2 AND lease_owner = $3 AND status = 'PROCESSING'
	`, consumer, eventID, owner, now, status, outcome)
	if err != nil {
		return fmt.Errorf("finish inbox record: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `
		UPDATE dead_letters
		SET status = 'RESOLVED', resolution_note = 'handled by a later delivery', resolved_by = 'system',
		    resolved_at = $3, updated_at = $3
		WHERE event_id = $2 AND consumer IN ($1, '') AND status = 'OPEN'
	`, consumer, eventID, now); err != nil {
		return fmt.Errorf("resolve dead letters: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit inbox completion: %w", err)
	}
	return nil
}

func (r *InboxRepository) Fail(ctx context.Context, consumer, eventID, owner string, now, next time.Time, lastError string) error {
	return r.leased(ctx, `
		UPDATE event_inbox
		SET status = 'FAILED', last_error = NULLIF($6, ''), next_attempt_at = $5,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $4
		WHERE consumer = $1 AND event_id = $2 AND lease_owner = $3 AND status = 'PROCESSING'
	`, consumer, eventID, owner, now, next, lastError)
}

func (r *InboxRepository) Dead(ctx context.Context, consumer, eventID, owner string, dl model.DeadLetter) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin inbox dead letter: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, `
		UPDATE event_inbox
		SET status = 'DEAD', last_error = NULLIF($4, ''), next_attempt_at = NULL,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $5
		WHERE consumer = $1 AND event_id = $2 AND lease_owner = $3 AND status = 'PROCESSING'
	`, consumer, eventID, owner, dl.LastError, dl.DeadLetteredAt)
	if err != nil {
		return fmt.Errorf("mark inbox record dead: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	if err := insertDeadLetter(ctx, tx, dl); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit inbox dead letter: %w", err)
	}
	return nil
}

func (r *InboxRepository) Bury(ctx context.Context, rec model.InboxRecord, dl model.DeadLetter) (string, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin inbox burial: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var status string
	err = tx.QueryRow(ctx, `
		INSERT INTO event_inbox (
			consumer, event_id, event_type, schema_version, tenant_id, aggregate_type, aggregate_id,
			aggregate_sequence, correlation_id, status, attempts, last_error, first_seen_at, updated_at
		)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, NULLIF($9, ''), 'DEAD', $10, NULLIF($11, ''), $12, $12)
		ON CONFLICT (consumer, event_id) DO UPDATE
		SET status = 'DEAD', last_error = EXCLUDED.last_error, next_attempt_at = NULL,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = EXCLUDED.updated_at
		WHERE event_inbox.status IN ('FAILED', 'DEAD')
		   OR (event_inbox.status = 'PROCESSING' AND event_inbox.lease_expires_at <= EXCLUDED.updated_at)
		RETURNING status
	`, rec.Consumer, rec.EventID, rec.EventType, rec.SchemaVersion, rec.TenantID, rec.AggregateType, rec.AggregateID,
		rec.AggregateSequence, rec.CorrelationID, dl.Attempts, dl.LastError, dl.DeadLetteredAt).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		// Finished, or being processed right now: leave it alone.
		current, getErr := r.Get(ctx, rec.Consumer, rec.EventID)
		if getErr != nil {
			return "", getErr
		}
		return current.Status, nil
	}
	if err != nil {
		return "", fmt.Errorf("bury inbox record: %w", err)
	}
	if err := insertDeadLetter(ctx, tx, dl); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit inbox burial: %w", err)
	}
	return status, nil
}

func (r *InboxRepository) Abandon(ctx context.Context, consumer, eventID string, now time.Time, note string) error {
	return r.leased(ctx, `
		UPDATE event_inbox
		SET status = 'FAILED', next_attempt_at = $3, last_error = NULLIF($4, ''),
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE consumer = $1 AND event_id = $2 AND status IN ('PROCESSING', 'FAILED')
		  AND (lease_expires_at IS NULL OR lease_expires_at <= $3)
	`, consumer, eventID, now, note)
}

func (r *InboxRepository) Get(ctx context.Context, consumer, eventID string) (model.InboxRecord, error) {
	rec, err := scanInbox(r.db.Pool.QueryRow(ctx, `
		SELECT `+inboxColumns+` FROM event_inbox WHERE consumer = $1 AND event_id = $2
	`, consumer, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.InboxRecord{}, model.ErrEventNotFound
	}
	if err != nil {
		return model.InboxRecord{}, fmt.Errorf("get inbox record: %w", err)
	}
	return rec, nil
}

func (r *InboxRepository) List(ctx context.Context, consumer, status string, limit int) ([]model.InboxRecord, error) {
	return r.query(ctx, `
		SELECT `+inboxColumns+` FROM event_inbox
		WHERE ($1 = '' OR consumer = $1) AND ($2 = '' OR status = $2)
		ORDER BY updated_at DESC, event_id
		LIMIT $3
	`, consumer, status, limit)
}

func (r *InboxRepository) Stalled(ctx context.Context, before time.Time, limit int, tenantID string) ([]model.InboxRecord, error) {
	return r.query(ctx, `
		SELECT `+inboxColumns+` FROM event_inbox AS i
		WHERE i.status IN ('PROCESSING', 'FAILED')
		  AND i.updated_at <= $1
		  AND COALESCE(i.lease_expires_at, i.next_attempt_at, i.updated_at) <= $1
		  AND ($3 = '' OR i.tenant_id = $3)
		  AND NOT EXISTS (
			SELECT 1 FROM dead_letters AS d
			WHERE d.event_id = i.event_id AND d.consumer IN (i.consumer, '') AND d.status = 'OPEN'
		  )
		ORDER BY i.updated_at
		LIMIT $2
	`, before, limit, tenantID)
}

// leased runs an update that must hit exactly the leased record.
func (r *InboxRepository) leased(ctx context.Context, sql string, args ...any) error {
	tag, err := r.db.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("update inbox record: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *InboxRepository) query(ctx context.Context, sql string, args ...any) ([]model.InboxRecord, error) {
	rows, err := r.db.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query inbox: %w", err)
	}
	defer rows.Close()

	var records []model.InboxRecord
	for rows.Next() {
		rec, err := scanInbox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan inbox record: %w", err)
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}
