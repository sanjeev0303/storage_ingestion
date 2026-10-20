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

// DeadLetterRepository keeps the dead-letter records in PostgreSQL.
type DeadLetterRepository struct {
	db *DB
}

var _ interfaces.DeadLetterRepository = (*DeadLetterRepository)(nil)

func NewDeadLetterRepository(db *DB) *DeadLetterRepository {
	return &DeadLetterRepository{db: db}
}

const deadLetterColumns = `
	id, event_id, source, consumer, COALESCE(event_type, ''), COALESCE(schema_version, 0), COALESCE(tenant_id, ''),
	COALESCE(aggregate_type, ''), COALESCE(aggregate_id, ''), COALESCE(correlation_id, ''), failure_category,
	last_error, attempts, event_created_at, first_failed_at, last_failed_at, dead_lettered_at,
	COALESCE(message_digest, ''), status, COALESCE(resolution_note, ''), COALESCE(resolved_by, ''), resolved_at,
	COALESCE(replay_event_id, ''), COALESCE(source_changed, ''), updated_at
`

func scanDeadLetter(row pgx.Row) (model.DeadLetter, error) {
	var d model.DeadLetter
	err := row.Scan(&d.ID, &d.EventID, &d.Source, &d.Consumer, &d.EventType, &d.SchemaVersion, &d.TenantID,
		&d.AggregateType, &d.AggregateID, &d.CorrelationID, &d.FailureCategory,
		&d.LastError, &d.Attempts, &d.EventCreatedAt, &d.FirstFailedAt, &d.LastFailedAt, &d.DeadLetteredAt,
		&d.MessageDigest, &d.Status, &d.ResolutionNote, &d.ResolvedBy, &d.ResolvedAt,
		&d.ReplayEventID, &d.SourceChanged, &d.UpdatedAt)
	return d, err
}

// While a dead letter is open for an event and consumer, a repeated
// failure updates it rather than adding a second one.
const insertDeadLetterSQL = `
	INSERT INTO dead_letters (
		id, event_id, source, consumer, event_type, schema_version, tenant_id, aggregate_type, aggregate_id,
		correlation_id, failure_category, last_error, attempts, event_created_at, first_failed_at,
		last_failed_at, dead_lettered_at, message_digest, status, updated_at
	)
	VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, 0), NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''),
	        NULLIF($10, ''), $11, $12, $13, $14, $15, $16, $17, NULLIF($18, ''), 'OPEN', $17)
	ON CONFLICT (event_id, consumer) WHERE status = 'OPEN' DO UPDATE
	SET failure_category = EXCLUDED.failure_category, last_error = EXCLUDED.last_error,
	    attempts = GREATEST(dead_letters.attempts, EXCLUDED.attempts),
	    last_failed_at = EXCLUDED.last_failed_at, updated_at = EXCLUDED.updated_at
`

func deadLetterArgs(d model.DeadLetter) []any {
	return []any{d.ID, d.EventID, d.Source, d.Consumer, d.EventType, d.SchemaVersion, d.TenantID, d.AggregateType, d.AggregateID,
		d.CorrelationID, d.FailureCategory, d.LastError, d.Attempts, d.EventCreatedAt, d.FirstFailedAt,
		d.LastFailedAt, d.DeadLetteredAt, d.MessageDigest}
}

// insertDeadLetter records a dead letter inside a transaction, so it
// commits together with the status change that gave up on the event.
func insertDeadLetter(ctx context.Context, tx pgx.Tx, d model.DeadLetter) error {
	if _, err := tx.Exec(ctx, insertDeadLetterSQL, deadLetterArgs(d)...); err != nil {
		return fmt.Errorf("record dead letter: %w", err)
	}
	return nil
}

func (r *DeadLetterRepository) Add(ctx context.Context, d model.DeadLetter) error {
	if _, err := r.db.Pool.Exec(ctx, insertDeadLetterSQL, deadLetterArgs(d)...); err != nil {
		return fmt.Errorf("record dead letter: %w", err)
	}
	return nil
}

func (r *DeadLetterRepository) Get(ctx context.Context, id string) (model.DeadLetter, error) {
	d, err := scanDeadLetter(r.db.Pool.QueryRow(ctx, `SELECT `+deadLetterColumns+` FROM dead_letters WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeadLetter{}, model.ErrDeadLetterNotFound
	}
	if err != nil {
		return model.DeadLetter{}, fmt.Errorf("get dead letter: %w", err)
	}
	return d, nil
}

func (r *DeadLetterRepository) List(ctx context.Context, status string, limit int) ([]model.DeadLetter, error) {
	return r.query(ctx, `
		SELECT `+deadLetterColumns+` FROM dead_letters
		WHERE ($1 = '' OR status = $1)
		ORDER BY dead_lettered_at DESC, id
		LIMIT $2
	`, status, limit)
}

func (r *DeadLetterRepository) ListOpenAfter(ctx context.Context, afterID string, limit int, tenantID string) ([]model.DeadLetter, error) {
	return r.query(ctx, `
		SELECT `+deadLetterColumns+` FROM dead_letters
		WHERE status = 'OPEN' AND id > $1 AND ($3 = '' OR tenant_id = $3)
		ORDER BY id
		LIMIT $2
	`, afterID, limit, tenantID)
}

func (r *DeadLetterRepository) Stats(ctx context.Context, now time.Time) (model.DeadLetterStats, error) {
	var stats model.DeadLetterStats
	var oldest *time.Time
	err := r.db.Pool.QueryRow(ctx, `
		SELECT count(*), min(dead_lettered_at) FROM dead_letters WHERE status = 'OPEN'
	`).Scan(&stats.Open, &oldest)
	if err != nil {
		return model.DeadLetterStats{}, fmt.Errorf("read dead letter stats: %w", err)
	}
	if oldest != nil {
		stats.Oldest = now.Sub(*oldest)
	}
	return stats, nil
}

func (r *DeadLetterRepository) Retry(ctx context.Context, id, actor, note string, now time.Time) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin dead letter retry: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	dl, err := lockOpenDeadLetter(ctx, tx, id)
	if err != nil {
		return err
	}

	// The same event again, in a new publication generation so the queue
	// does not drop it as a duplicate of the first.
	tag, err := tx.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'PENDING', attempts = 0,
		    republished = republished + CASE WHEN status = 'PENDING' THEN 0 ELSE 1 END,
		    next_attempt_at = $2, last_error = NULL, lease_owner = NULL, lease_expires_at = NULL, updated_at = $2
		WHERE id = $1
	`, dl.EventID, now)
	if err != nil {
		return fmt.Errorf("requeue outbox event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotReplayable
	}
	if dl.Consumer != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE event_inbox
			SET status = 'FAILED', attempts = 0, next_attempt_at = $3, updated_at = $3
			WHERE consumer = $1 AND event_id = $2 AND status = 'DEAD'
		`, dl.Consumer, dl.EventID, now); err != nil {
			return fmt.Errorf("revive inbox record: %w", err)
		}
	}
	if err := closeDeadLetter(ctx, tx, id, model.DeadLetterRetried, actor, note, "", now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit dead letter retry: %w", err)
	}
	return nil
}

func (r *DeadLetterRepository) Replay(ctx context.Context, id, actor, note string, replay model.OutboxEvent, now time.Time) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin dead letter replay: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := lockOpenDeadLetter(ctx, tx, id); err != nil {
		return err
	}
	if err := insertOutbox(ctx, tx, replay); err != nil {
		return err
	}
	if err := closeDeadLetter(ctx, tx, id, model.DeadLetterReplayed, actor, note, replay.ID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit dead letter replay: %w", err)
	}
	return nil
}

func (r *DeadLetterRepository) Close(ctx context.Context, id, status, actor, note string, now time.Time) error {
	if status != model.DeadLetterResolved && status != model.DeadLetterDiscarded {
		return fmt.Errorf("a dead letter is closed as RESOLVED or DISCARDED, not %q", status)
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin dead letter close: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := lockOpenDeadLetter(ctx, tx, id); err != nil {
		return err
	}
	if err := closeDeadLetter(ctx, tx, id, status, actor, note, "", now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit dead letter close: %w", err)
	}
	return nil
}

func (r *DeadLetterRepository) MarkSourceChanged(ctx context.Context, id, note string, now time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE dead_letters SET source_changed = $2, updated_at = $3 WHERE id = $1 AND status = 'OPEN'
	`, id, note, now)
	if err != nil {
		return fmt.Errorf("mark dead letter source changed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrDeadLetterClosed
	}
	return nil
}

// lockOpenDeadLetter reads a dead letter for update and checks it is open.
func lockOpenDeadLetter(ctx context.Context, tx pgx.Tx, id string) (model.DeadLetter, error) {
	dl, err := scanDeadLetter(tx.QueryRow(ctx, `SELECT `+deadLetterColumns+` FROM dead_letters WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DeadLetter{}, model.ErrDeadLetterNotFound
	}
	if err != nil {
		return model.DeadLetter{}, fmt.Errorf("lock dead letter: %w", err)
	}
	if dl.Status != model.DeadLetterOpen {
		return model.DeadLetter{}, model.ErrDeadLetterClosed
	}
	return dl, nil
}

func closeDeadLetter(ctx context.Context, tx pgx.Tx, id, status, actor, note, replayEventID string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE dead_letters
		SET status = $2, resolved_by = $3, resolution_note = $4, replay_event_id = NULLIF($5, ''),
		    resolved_at = $6, updated_at = $6
		WHERE id = $1
	`, id, status, actor, note, replayEventID, now)
	if err != nil {
		return fmt.Errorf("close dead letter: %w", err)
	}
	return nil
}

func (r *DeadLetterRepository) query(ctx context.Context, sql string, args ...any) ([]model.DeadLetter, error) {
	rows, err := r.db.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query dead letters: %w", err)
	}
	defer rows.Close()

	var out []model.DeadLetter
	for rows.Next() {
		d, err := scanDeadLetter(rows)
		if err != nil {
			return nil, fmt.Errorf("scan dead letter: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DiscrepancyRepository records what event reconciliation found and did.
type DiscrepancyRepository struct {
	db *DB
}

var _ interfaces.DiscrepancyRepository = (*DiscrepancyRepository)(nil)

func NewDiscrepancyRepository(db *DB) *DiscrepancyRepository {
	return &DiscrepancyRepository{db: db}
}

func (r *DiscrepancyRepository) Record(ctx context.Context, d model.EventDiscrepancy) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO event_discrepancies (
			id, kind, event_id, consumer, tenant_id, aggregate_type, aggregate_id, detail, action, outcome, created_at
		)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10, $11)
	`, d.ID, d.Kind, d.EventID, d.Consumer, d.TenantID, d.AggregateType, d.AggregateID, d.Detail, d.Action, d.Outcome, d.CreatedAt)
	if err != nil {
		return fmt.Errorf("record event discrepancy: %w", err)
	}
	return nil
}

func (r *DiscrepancyRepository) List(ctx context.Context, limit int) ([]model.EventDiscrepancy, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, kind, COALESCE(event_id, ''), COALESCE(consumer, ''), COALESCE(tenant_id, ''),
		       COALESCE(aggregate_type, ''), COALESCE(aggregate_id, ''), detail, action, outcome, created_at
		FROM event_discrepancies
		ORDER BY created_at DESC, id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list event discrepancies: %w", err)
	}
	defer rows.Close()

	var out []model.EventDiscrepancy
	for rows.Next() {
		var d model.EventDiscrepancy
		if err := rows.Scan(&d.ID, &d.Kind, &d.EventID, &d.Consumer, &d.TenantID,
			&d.AggregateType, &d.AggregateID, &d.Detail, &d.Action, &d.Outcome, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event discrepancy: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
