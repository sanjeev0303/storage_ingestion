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

// LifecycleEventRepository appends lifecycle audit events.
type LifecycleEventRepository struct {
	db *DB
}

var _ interfaces.LifecycleEventRepository = (*LifecycleEventRepository)(nil)

func NewLifecycleEventRepository(db *DB) *LifecycleEventRepository {
	return &LifecycleEventRepository{db: db}
}

func (r *LifecycleEventRepository) Record(ctx context.Context, event model.LifecycleEvent) error {
	_, err := r.db.Pool.Exec(ctx, insertEventSQL, eventArgs(event)...)
	if err != nil {
		return fmt.Errorf("record lifecycle event: %w", err)
	}
	return nil
}

const insertEventSQL = `
	INSERT INTO lifecycle_events (id, tenant_id, asset_id, operation_id, event, outcome, reason, actor, created_at)
	VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9)
`

func eventArgs(e model.LifecycleEvent) []any {
	return []any{e.ID, e.TenantID, e.AssetID, e.OperationID, e.Event, e.Outcome, e.Reason, e.Actor, e.CreatedAt}
}

// insertEvent records an event inside a transaction, so the event and the
// change it describes commit together.
func insertEvent(ctx context.Context, tx pgx.Tx, event model.LifecycleEvent) error {
	if _, err := tx.Exec(ctx, insertEventSQL, eventArgs(event)...); err != nil {
		return fmt.Errorf("record lifecycle event: %w", err)
	}
	return nil
}

func (r *LifecycleEventRepository) List(ctx context.Context, tenantID, assetID string, limit int) ([]model.LifecycleEvent, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, COALESCE(tenant_id, ''), COALESCE(asset_id, ''), COALESCE(operation_id, ''), event,
		       COALESCE(outcome, ''), COALESCE(reason, ''), actor, created_at
		FROM lifecycle_events
		WHERE tenant_id = $1 AND ($2 = '' OR asset_id = $2)
		ORDER BY created_at, id
		LIMIT $3
	`, tenantID, assetID, limit)
	if err != nil {
		return nil, fmt.Errorf("list lifecycle events: %w", err)
	}
	defer rows.Close()

	var events []model.LifecycleEvent
	for rows.Next() {
		var e model.LifecycleEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.AssetID, &e.OperationID, &e.Event, &e.Outcome, &e.Reason, &e.Actor, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan lifecycle event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (r *LifecycleEventRepository) RecordedSince(ctx context.Context, tenantID, assetID, event string, since time.Time) (bool, error) {
	var found bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM lifecycle_events
			WHERE tenant_id = $1 AND asset_id = $2 AND event = $3 AND created_at >= $4
		)
	`, tenantID, assetID, event, since).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("look up lifecycle event: %w", err)
	}
	return found, nil
}

// CursorRepository keeps where resumable database scans continue.
type CursorRepository struct {
	db *DB
}

var _ interfaces.CursorRepository = (*CursorRepository)(nil)

func NewCursorRepository(db *DB) *CursorRepository {
	return &CursorRepository{db: db}
}

func (r *CursorRepository) Get(ctx context.Context, name string) (string, error) {
	var position string
	err := r.db.Pool.QueryRow(ctx, `SELECT position FROM lifecycle_cursors WHERE name = $1`, name).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get cursor: %w", err)
	}
	return position, nil
}

func (r *CursorRepository) Set(ctx context.Context, name, position string, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO lifecycle_cursors (name, position, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET position = EXCLUDED.position, updated_at = EXCLUDED.updated_at
	`, name, position, at)
	if err != nil {
		return fmt.Errorf("save cursor: %w", err)
	}
	return nil
}
