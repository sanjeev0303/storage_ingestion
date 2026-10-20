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

// InventoryRepository stores inventory scans and orphan candidates.
type InventoryRepository struct {
	db *DB
}

var _ interfaces.InventoryRepository = (*InventoryRepository)(nil)

func NewInventoryRepository(db *DB) *InventoryRepository {
	return &InventoryRepository{db: db}
}

const scanColumns = `
	id, bucket, prefix, status, COALESCE(cursor, ''), pages, objects_seen, candidates_marked,
	errors, COALESCE(last_error, ''), started_at, updated_at, completed_at
`

func scanInventoryScan(row pgx.Row) (model.InventoryScan, error) {
	var s model.InventoryScan
	err := row.Scan(&s.ID, &s.Bucket, &s.Prefix, &s.Status, &s.Cursor, &s.Pages, &s.ObjectsSeen, &s.CandidatesMarked,
		&s.Errors, &s.LastError, &s.StartedAt, &s.UpdatedAt, &s.CompletedAt)
	return s, err
}

func (r *InventoryRepository) OpenScan(ctx context.Context, bucket, prefix, owner, newID string, now time.Time, lease time.Duration) (model.InventoryScan, bool, error) {
	// Resume the running scan when its lease is free, expired or ours.
	scan, err := scanInventoryScan(r.db.Pool.QueryRow(ctx, `
		UPDATE inventory_scans
		SET lease_owner = $3, lease_expires_at = $4::timestamptz + make_interval(secs => $5), updated_at = $4
		WHERE bucket = $1 AND prefix = $2 AND status = 'RUNNING'
		  AND (lease_expires_at IS NULL OR lease_expires_at <= $4 OR lease_owner = $3)
		RETURNING `+scanColumns,
		bucket, prefix, owner, now, lease.Seconds()))
	if err == nil {
		return scan, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return model.InventoryScan{}, false, fmt.Errorf("resume inventory scan: %w", err)
	}

	// No resumable scan: start one, unless another worker holds the
	// running one (the partial unique index decides).
	scan, err = scanInventoryScan(r.db.Pool.QueryRow(ctx, `
		INSERT INTO inventory_scans (id, bucket, prefix, status, lease_owner, lease_expires_at, started_at, updated_at)
		VALUES ($1, $2, $3, 'RUNNING', $4, $5::timestamptz + make_interval(secs => $6), $5, $5)
		ON CONFLICT (bucket, prefix) WHERE status = 'RUNNING' DO NOTHING
		RETURNING `+scanColumns,
		newID, bucket, prefix, owner, now, lease.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.InventoryScan{}, false, nil
	}
	if err != nil {
		return model.InventoryScan{}, false, fmt.Errorf("start inventory scan: %w", err)
	}
	return scan, true, nil
}

func (r *InventoryRepository) SaveProgress(ctx context.Context, scan model.InventoryScan, owner string, now time.Time, lease time.Duration) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE inventory_scans
		SET cursor = NULLIF($3, ''), pages = $4, objects_seen = $5, candidates_marked = $6,
		    errors = $7, last_error = NULLIF($8, ''),
		    lease_expires_at = $9::timestamptz + make_interval(secs => $10), updated_at = $9
		WHERE id = $1 AND lease_owner = $2 AND status = 'RUNNING'
	`, scan.ID, owner, scan.Cursor, scan.Pages, scan.ObjectsSeen, scan.CandidatesMarked,
		scan.Errors, scan.LastError, now, lease.Seconds())
	if err != nil {
		return fmt.Errorf("save inventory scan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *InventoryRepository) FinishScan(ctx context.Context, scan model.InventoryScan, owner string, now time.Time, status string) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE inventory_scans
		SET status = $4, last_error = NULLIF($5, ''), cursor = NULL,
		    pages = $6, objects_seen = $7, candidates_marked = $8, errors = $9,
		    completed_at = CASE WHEN $4 = 'COMPLETED' THEN $3::timestamptz ELSE NULL END,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND status = 'RUNNING'
	`, scan.ID, owner, now, status, scan.LastError, scan.Pages, scan.ObjectsSeen, scan.CandidatesMarked, scan.Errors)
	if err != nil {
		return fmt.Errorf("finish inventory scan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *InventoryRepository) LastCompleted(ctx context.Context, bucket, prefix string) (model.InventoryScan, bool, error) {
	scan, err := scanInventoryScan(r.db.Pool.QueryRow(ctx, `
		SELECT `+scanColumns+`
		FROM inventory_scans
		WHERE bucket = $1 AND prefix = $2 AND status = 'COMPLETED'
		ORDER BY completed_at DESC
		LIMIT 1
	`, bucket, prefix))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.InventoryScan{}, false, nil
	}
	if err != nil {
		return model.InventoryScan{}, false, fmt.Errorf("get last completed scan: %w", err)
	}
	return scan, true, nil
}

func (r *InventoryRepository) RecentScans(ctx context.Context, limit int) ([]model.InventoryScan, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+scanColumns+` FROM inventory_scans ORDER BY started_at DESC, id LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list inventory scans: %w", err)
	}
	defer rows.Close()

	var scans []model.InventoryScan
	for rows.Next() {
		scan, err := scanInventoryScan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan inventory scan: %w", err)
		}
		scans = append(scans, scan)
	}
	return scans, rows.Err()
}

const candidateColumns = `
	id, bucket, object_key, etag, size_bytes, category, tenant_id, reason, status, observations,
	first_seen_at, last_seen_at, last_scan_id, attempt_count, COALESCE(last_error, ''), resolved_at,
	created_at, updated_at
`

func scanCandidate(row pgx.Row) (model.OrphanCandidate, error) {
	var c model.OrphanCandidate
	err := row.Scan(&c.ID, &c.Bucket, &c.ObjectKey, &c.ETag, &c.SizeBytes, &c.Category, &c.TenantID, &c.Reason, &c.Status,
		&c.Observations, &c.FirstSeenAt, &c.LastSeenAt, &c.LastScanID, &c.AttemptCount, &c.LastError, &c.ResolvedAt,
		&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// Mark counts one sighting per scan: replaying a page of the same scan
// changes nothing, and a different ETag is a different object whose count
// starts over.
func (r *InventoryRepository) Mark(ctx context.Context, c model.OrphanCandidate, scanID string, now time.Time) (model.OrphanCandidate, error) {
	stored, err := scanCandidate(r.db.Pool.QueryRow(ctx, `
		INSERT INTO orphan_candidates (
			id, bucket, object_key, etag, size_bytes, category, tenant_id, reason, status, observations,
			first_seen_at, last_seen_at, last_scan_id, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'MARKED', 1, $9, $9, $10, $9, $9)
		ON CONFLICT (bucket, object_key) WHERE status IN ('MARKED', 'FAILED')
		DO UPDATE SET
			observations = CASE
				WHEN orphan_candidates.etag <> EXCLUDED.etag THEN 1
				WHEN orphan_candidates.last_scan_id = EXCLUDED.last_scan_id THEN orphan_candidates.observations
				ELSE orphan_candidates.observations + 1
			END,
			first_seen_at = CASE WHEN orphan_candidates.etag <> EXCLUDED.etag
			                     THEN EXCLUDED.first_seen_at ELSE orphan_candidates.first_seen_at END,
			attempt_count = CASE WHEN orphan_candidates.etag <> EXCLUDED.etag
			                     THEN 0 ELSE orphan_candidates.attempt_count END,
			etag = EXCLUDED.etag,
			size_bytes = EXCLUDED.size_bytes,
			category = EXCLUDED.category,
			tenant_id = EXCLUDED.tenant_id,
			reason = EXCLUDED.reason,
			last_seen_at = EXCLUDED.last_seen_at,
			last_scan_id = EXCLUDED.last_scan_id,
			updated_at = EXCLUDED.updated_at
		RETURNING `+candidateColumns,
		c.ID, c.Bucket, c.ObjectKey, c.ETag, c.SizeBytes, c.Category, c.TenantID, c.Reason, now, scanID))
	if err != nil {
		return model.OrphanCandidate{}, fmt.Errorf("mark orphan candidate: %w", err)
	}
	return stored, nil
}

func (r *InventoryRepository) DueCandidates(ctx context.Context, bucket, keyPrefix string, seenBefore time.Time, minObservations, maxAttempts, limit int) ([]model.OrphanCandidate, error) {
	return r.candidates(ctx, `
		SELECT c.id, c.bucket, c.object_key, c.etag, c.size_bytes, c.category, c.tenant_id, c.reason, c.status,
		       c.observations, c.first_seen_at, c.last_seen_at, c.last_scan_id, c.attempt_count,
		       COALESCE(c.last_error, ''), c.resolved_at, c.created_at, c.updated_at
		FROM orphan_candidates AS c
		JOIN inventory_scans AS s ON s.id = c.last_scan_id AND s.status = 'COMPLETED'
		WHERE c.bucket = $1
		  AND left(c.object_key, length($2)) = $2
		  AND c.status IN ('MARKED', 'FAILED')
		  AND c.observations >= $4
		  AND c.first_seen_at <= $3
		  AND c.attempt_count < $5
		ORDER BY c.first_seen_at, c.id
		LIMIT $6
	`, bucket, keyPrefix, seenBefore, minObservations, maxAttempts, limit)
}

func (r *InventoryRepository) Resolve(ctx context.Context, candidateID, status, reason string, now time.Time) error {
	if status != model.CandidateDeleted && status != model.CandidateCleared {
		return fmt.Errorf("a candidate resolves as DELETED or CLEARED, not %s", status)
	}
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE orphan_candidates
		SET status = $2, reason = $3, resolved_at = $4, updated_at = $4
		WHERE id = $1 AND status IN ('MARKED', 'FAILED')
	`, candidateID, status, reason, now)
	if err != nil {
		return fmt.Errorf("resolve orphan candidate: %w", err)
	}
	return nil
}

func (r *InventoryRepository) FailCandidate(ctx context.Context, candidateID, lastError string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE orphan_candidates
		SET status = 'FAILED', attempt_count = attempt_count + 1, last_error = $2, updated_at = $3
		WHERE id = $1 AND status IN ('MARKED', 'FAILED')
	`, candidateID, lastError, now)
	if err != nil {
		return fmt.Errorf("record orphan sweep failure: %w", err)
	}
	return nil
}

func (r *InventoryRepository) ListCandidates(ctx context.Context, status string, limit int) ([]model.OrphanCandidate, error) {
	return r.candidates(ctx, `
		SELECT `+candidateColumns+`
		FROM orphan_candidates
		WHERE $1 = '' OR status = $1
		ORDER BY updated_at DESC, id
		LIMIT $2
	`, status, limit)
}

func (r *InventoryRepository) candidates(ctx context.Context, sql string, args ...any) ([]model.OrphanCandidate, error) {
	rows, err := r.db.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query orphan candidates: %w", err)
	}
	defer rows.Close()

	var out []model.OrphanCandidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan orphan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
