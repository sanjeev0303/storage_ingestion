package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// PackRepository stores packs, their entries and the jobs that build them.
type PackRepository struct {
	db *DB
}

var _ interfaces.PackRepository = (*PackRepository)(nil)

func NewPackRepository(db *DB) *PackRepository {
	return &PackRepository{db: db}
}

const packColumns = `
	id, tenant_id, storage_connection_id, bucket, object_key, manifest_key, format, format_version, state,
	entry_count, total_bytes, COALESCE(sha256, ''), COALESCE(manifest_sha256, ''), COALESCE(storage_etag, ''),
	COALESCE(storage_version_id, ''), COALESCE(rewrite_of, ''), attempts, max_attempts, next_attempt_at,
	COALESCE(lease_owner, ''), lease_expires_at, COALESCE(last_error, ''), created_at, updated_at,
	published_at, retired_at, deleted_at
`

func scanPack(row pgx.Row) (model.Pack, error) {
	var p model.Pack
	var state string
	err := row.Scan(&p.ID, &p.TenantID, &p.ConnectionID, &p.Bucket, &p.ObjectKey, &p.ManifestKey, &p.Format, &p.FormatVersion,
		&state, &p.EntryCount, &p.TotalBytes, &p.SHA256, &p.ManifestSHA256, &p.StorageETag, &p.StorageVersionID, &p.RewriteOf,
		&p.Attempts, &p.MaxAttempts, &p.NextAttemptAt, &p.LeaseOwner, &p.LeaseExpires, &p.LastError, &p.CreatedAt, &p.UpdatedAt,
		&p.PublishedAt, &p.RetiredAt, &p.DeletedAt)
	p.State = model.PackState(state)
	return p, err
}

func collectPacks(rows pgx.Rows, err error) ([]model.Pack, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Pack
	for rows.Next() {
		p, err := scanPack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const packEntryColumns = `
	e.pack_id, e.tenant_id, e.ordinal, e.asset_id, COALESCE(e.object_id, ''), e.version, e.byte_offset, e.stored_length,
	e.size_bytes, e.checksum_sha256, e.source_key, COALESCE(e.source_etag, ''), COALESCE(e.source_version_id, ''),
	e.state, e.source_retired_at, e.deleted_at
`

func collectPackEntries(rows pgx.Rows, err error) ([]model.PackEntry, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PackEntry
	for rows.Next() {
		var e model.PackEntry
		var state string
		if err := rows.Scan(&e.PackID, &e.TenantID, &e.Ordinal, &e.AssetID, &e.ObjectID, &e.Version, &e.Offset, &e.Length,
			&e.SizeBytes, &e.ChecksumSHA256, &e.SourceKey, &e.SourceETag, &e.SourceVersionID, &state,
			&e.SourceRetiredAt, &e.DeletedAt); err != nil {
			return nil, err
		}
		e.State = model.PackEntryState(state)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *PackRepository) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin pack transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pack transaction: %w", err)
	}
	return nil
}

// ─── Reading ──────────────────────────────────────────────────────────

func (r *PackRepository) PackedLocation(ctx context.Context, tenantID, assetID string) (model.PackedLocation, bool, error) {
	var loc model.PackedLocation
	err := r.db.Pool.QueryRow(ctx, `
		SELECT p.id, p.storage_connection_id, p.bucket, p.object_key, COALESCE(p.storage_etag, ''),
		       COALESCE(p.storage_version_id, ''), p.total_bytes, e.byte_offset, e.stored_length
		FROM pack_entries e
		JOIN packs p ON p.id = e.pack_id AND p.tenant_id = e.tenant_id
		WHERE e.tenant_id = $1 AND e.asset_id = $2 AND e.state = 'PUBLISHED' AND p.state = 'PUBLISHED'
	`, tenantID, assetID).Scan(&loc.PackID, &loc.ConnectionID, &loc.Bucket, &loc.ObjectKey, &loc.StorageETag,
		&loc.StorageVersionID, &loc.PackBytes, &loc.Offset, &loc.Length)
	if errors.Is(err, pgx.ErrNoRows) {
		return loc, false, nil
	}
	if err != nil {
		return loc, false, fmt.Errorf("locate packed asset: %w", err)
	}
	// The row says where to read; never read outside the pack.
	if loc.Offset < 0 || loc.Length < 0 || loc.Length > math.MaxInt64-loc.Offset || loc.Offset+loc.Length > loc.PackBytes {
		return loc, false, fmt.Errorf("%w: entry of asset %s lies outside pack %s", model.ErrPackCorrupt, assetID, loc.PackID)
	}
	return loc, true, nil
}

func (r *PackRepository) ReleaseAsset(ctx context.Context, tenantID, assetID string, now time.Time) (bool, error) {
	if _, err := r.db.Pool.Exec(ctx, `
		UPDATE pack_entries SET state = 'DELETED', deleted_at = $3, updated_at = $3
		WHERE tenant_id = $1 AND asset_id = $2 AND state = 'PUBLISHED'
	`, tenantID, assetID, now); err != nil {
		return false, fmt.Errorf("release packed asset: %w", err)
	}
	var holding bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pack_entries e JOIN packs p ON p.id = e.pack_id
			WHERE e.tenant_id = $1 AND e.asset_id = $2 AND p.state <> 'DELETED')
	`, tenantID, assetID).Scan(&holding)
	if err != nil {
		return false, fmt.Errorf("find packs holding an asset: %w", err)
	}
	return holding, nil
}

func (r *PackRepository) GetPack(ctx context.Context, tenantID, packID string) (model.Pack, error) {
	p, err := scanPack(r.db.Pool.QueryRow(ctx, `SELECT `+packColumns+` FROM packs WHERE tenant_id = $1 AND id = $2`, tenantID, packID))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, model.ErrPackNotFound
	}
	if err != nil {
		return p, fmt.Errorf("read pack: %w", err)
	}
	return p, nil
}

func (r *PackRepository) Entries(ctx context.Context, packID string) ([]model.PackEntry, error) {
	entries, err := collectPackEntries(r.db.Pool.Query(ctx, `
		SELECT `+packEntryColumns+` FROM pack_entries e WHERE e.pack_id = $1 ORDER BY e.ordinal
	`, packID))
	if err != nil {
		return nil, fmt.Errorf("read pack entries: %w", err)
	}
	return entries, nil
}

// ─── Planning ─────────────────────────────────────────────────────────

func (r *PackRepository) PackingTenants(ctx context.Context, after string, limit int) ([]string, error) {
	// A skip scan: one index probe per tenant, however many assets each has.
	rows, err := r.db.Pool.Query(ctx, `
		WITH RECURSIVE t AS (
			(SELECT tenant_id FROM assets WHERE status = 'AVAILABLE' AND tenant_id > $1 ORDER BY tenant_id LIMIT 1)
			UNION ALL
			SELECT (SELECT a.tenant_id FROM assets a WHERE a.status = 'AVAILABLE' AND a.tenant_id > t.tenant_id
			        ORDER BY a.tenant_id LIMIT 1)
			FROM t WHERE t.tenant_id IS NOT NULL
		)
		SELECT tenant_id FROM t WHERE tenant_id IS NOT NULL LIMIT $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list packing tenants: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (r *PackRepository) PackCandidates(ctx context.Context, tenantID, afterAssetID string, f interfaces.PackCandidateFilter, limit int) ([]model.Asset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+`
		FROM assets
		WHERE tenant_id = $1 AND status = 'AVAILABLE' AND id > $2
		  AND size_bytes BETWEEN $3 AND $4
		  AND COALESCE(published_at, updated_at) <= $5
		  AND COALESCE(storage_etag, '') <> '' AND COALESCE(checksum_sha256, '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM pack_entries e
		                  WHERE e.asset_id = assets.id AND e.state IN ('RESERVED', 'PUBLISHED', 'EXCLUDED'))
		  AND NOT EXISTS (SELECT 1 FROM legal_holds h
		                  WHERE h.tenant_id = assets.tenant_id AND h.released_at IS NULL
		                    AND (h.scope = 'TENANT'
		                         OR (h.scope = 'DATASET' AND COALESCE(assets.dataset, '') <> '' AND h.scope_key = assets.dataset)
		                         OR (h.scope = 'ASSET' AND h.scope_key = assets.id)))
		ORDER BY id
		LIMIT $6
	`, tenantID, afterAssetID, f.MinBytes, f.MaxBytes, f.PublishedBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("list packing candidates: %w", err)
	}
	defer rows.Close()
	var out []model.Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (r *PackRepository) BuildingPacks(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := r.db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM packs WHERE tenant_id = $1 AND state IN ('PLANNED', 'WRITING', 'VERIFYING')
	`, tenantID).Scan(&n)
	return n, err
}

func (r *PackRepository) CreatePack(ctx context.Context, p model.Pack, entries []model.PackEntry, outbox ...model.OutboxEvent) error {
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO packs (id, tenant_id, storage_connection_id, bucket, object_key, manifest_key, format, format_version,
			                   state, entry_count, total_bytes, rewrite_of, max_attempts, next_attempt_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'PLANNED', $9, $10, NULLIF($11, ''), $12, $13, $13, $13)
		`, p.ID, p.TenantID, p.ConnectionID, p.Bucket, p.ObjectKey, p.ManifestKey, p.Format, p.FormatVersion,
			len(entries), p.TotalBytes, p.RewriteOf, p.MaxAttempts, p.CreatedAt)
		if err != nil {
			return err
		}
		rows := make([][]any, len(entries))
		for i, e := range entries {
			rows[i] = []any{p.ID, p.TenantID, e.Ordinal, e.AssetID, nullable(e.ObjectID), e.Version, e.Offset, e.Length,
				e.SizeBytes, e.ChecksumSHA256, e.SourceKey, nullable(e.SourceETag), nullable(e.SourceVersionID),
				string(model.PackEntryReserved), p.CreatedAt, p.CreatedAt}
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"pack_entries"}, []string{
			"pack_id", "tenant_id", "ordinal", "asset_id", "object_id", "version", "byte_offset", "stored_length",
			"size_bytes", "checksum_sha256", "source_key", "source_etag", "source_version_id", "state", "created_at", "updated_at",
		}, pgx.CopyFromRows(rows))
		if err != nil {
			return err
		}
		return insertOutbox(ctx, tx, outbox...)
	})
	if isUniqueViolation(err) {
		return model.ErrPackConflict
	}
	if err != nil {
		return fmt.Errorf("create pack: %w", err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ─── Building ─────────────────────────────────────────────────────────

func (r *PackRepository) ClaimPacks(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]model.Pack, error) {
	packs, err := collectPacks(r.db.Pool.Query(ctx, `
		UPDATE packs SET lease_owner = $1, lease_expires_at = $2::timestamptz + make_interval(secs => $3),
		                 attempts = attempts + 1, updated_at = $2
		WHERE id IN (
			SELECT id FROM packs
			WHERE state IN ('PLANNED', 'WRITING', 'VERIFYING') AND next_attempt_at <= $2
			  AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
			ORDER BY next_attempt_at, created_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED)
		RETURNING `+packColumns, owner, now, lease.Seconds(), limit))
	if err != nil {
		return nil, fmt.Errorf("claim packs: %w", err)
	}
	return packs, nil
}

func (r *PackRepository) ClaimPack(ctx context.Context, tenantID, packID, owner string, now time.Time, lease time.Duration) (model.Pack, bool, error) {
	p, err := scanPack(r.db.Pool.QueryRow(ctx, `
		UPDATE packs SET lease_owner = $3, lease_expires_at = $4::timestamptz + make_interval(secs => $5),
		                 attempts = attempts + 1, updated_at = $4
		WHERE tenant_id = $1 AND id = $2 AND state IN ('PLANNED', 'WRITING', 'VERIFYING') AND next_attempt_at <= $4
		  AND (lease_expires_at IS NULL OR lease_expires_at <= $4)
		RETURNING `+packColumns, tenantID, packID, owner, now, lease.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("claim pack: %w", err)
	}
	return p, true, nil
}

func (r *PackRepository) RenewPack(ctx context.Context, packID, owner string, now time.Time, lease time.Duration) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE packs SET lease_expires_at = $3::timestamptz + make_interval(secs => $4)
		WHERE id = $1 AND lease_owner = $2 AND lease_expires_at > $3 AND state IN ('PLANNED', 'WRITING', 'VERIFYING')
	`, packID, owner, now, lease.Seconds())
	if err != nil {
		return fmt.Errorf("renew pack lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *PackRepository) AdvancePack(ctx context.Context, packID, owner string, from, to model.PackState, c interfaces.PackChange, now time.Time) error {
	if !model.CanTransitionPack(from, to) || !to.Building() {
		return fmt.Errorf("pack cannot go from %s to %s here", from, to)
	}
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE packs SET state = $4,
		    sha256 = COALESCE(NULLIF($5, ''), sha256),
		    manifest_sha256 = COALESCE(NULLIF($6, ''), manifest_sha256),
		    storage_etag = COALESCE(NULLIF($7, ''), storage_etag),
		    storage_version_id = COALESCE(NULLIF($8, ''), storage_version_id),
		    updated_at = $9
		WHERE id = $1 AND lease_owner = $2 AND state = $3
	`, packID, owner, from, to, c.SHA256, c.ManifestSHA256, c.StorageETag, c.StorageVersionID, now)
	if err != nil {
		return fmt.Errorf("advance pack: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *PackRepository) RetryPack(ctx context.Context, packID, owner string, now, next time.Time, lastError string) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE packs SET lease_owner = NULL, lease_expires_at = NULL, next_attempt_at = $3, last_error = NULLIF($4, ''), updated_at = $5
		WHERE id = $1 AND lease_owner = $2 AND state IN ('PLANNED', 'WRITING', 'VERIFYING')
	`, packID, owner, next, lastError, now)
	if err != nil {
		return fmt.Errorf("reschedule pack: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrLeaseLost
	}
	return nil
}

func (r *PackRepository) FailPack(ctx context.Context, packID, owner string, now time.Time, lastError string, excluded []string) error {
	if excluded == nil {
		excluded = []string{} // NULL would match nothing in ANY
	}
	return r.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE packs SET state = 'FAILED', lease_owner = NULL, lease_expires_at = NULL, last_error = NULLIF($3, ''), updated_at = $4
			WHERE id = $1 AND lease_owner = $2 AND state IN ('PLANNED', 'WRITING', 'VERIFYING')
		`, packID, owner, lastError, now)
		if err != nil {
			return fmt.Errorf("fail pack: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return model.ErrLeaseLost
		}
		_, err = tx.Exec(ctx, `
			UPDATE pack_entries SET state = CASE WHEN asset_id = ANY($2) THEN 'EXCLUDED' ELSE 'RELEASED' END, updated_at = $3
			WHERE pack_id = $1 AND state = 'RESERVED'
		`, packID, excluded, now)
		if err != nil {
			return fmt.Errorf("release pack entries: %w", err)
		}
		return nil
	})
}

func (r *PackRepository) PublishPack(ctx context.Context, packID, owner, manifestSHA256 string, now time.Time) (int, int, error) {
	var published, dead int
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		p, err := scanPack(tx.QueryRow(ctx, `
			SELECT `+packColumns+` FROM packs WHERE id = $1 AND lease_owner = $2 AND state = 'VERIFYING' FOR UPDATE
		`, packID, owner))
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("lock pack: %w", err)
		}

		// The assets are locked against a concurrent deletion while their
		// entries are decided. An entry is served only if its asset is
		// still the same published bytes: unchanged and not deleted, and,
		// in a rewrite, still served from the pack being rewritten.
		var live []string
		if p.RewriteOf == "" {
			live, err = collectStrings(tx.Query(ctx, `
				SELECT a.id FROM assets a
				JOIN pack_entries e ON e.asset_id = a.id AND e.tenant_id = a.tenant_id
				WHERE e.pack_id = $1 AND e.state = 'RESERVED'
				  AND a.status IN ('AVAILABLE', 'SOFT_DELETED') AND a.storage_etag = e.source_etag
				FOR SHARE OF a
			`, packID))
		} else {
			live, err = collectStrings(tx.Query(ctx, `
				UPDATE pack_entries o SET state = 'MOVED', updated_at = $3
				WHERE o.pack_id = $2 AND o.state = 'PUBLISHED'
				  AND o.asset_id IN (SELECT asset_id FROM pack_entries WHERE pack_id = $1 AND state = 'RESERVED')
				RETURNING o.asset_id
			`, packID, p.RewriteOf, now))
		}
		if err != nil {
			return fmt.Errorf("decide pack entries: %w", err)
		}
		if live == nil {
			live = []string{}
		}

		// A rewrite's sources are the old pack's entries: they go with it.
		tag, err := tx.Exec(ctx, `
			UPDATE pack_entries SET
			    state = CASE WHEN asset_id = ANY($2) THEN 'PUBLISHED' ELSE 'DELETED' END,
			    deleted_at = CASE WHEN asset_id = ANY($2) THEN NULL ELSE $3::timestamptz END,
			    source_retired_at = CASE WHEN $4 AND asset_id = ANY($2) THEN $3::timestamptz END,
			    updated_at = $3
			WHERE pack_id = $1 AND state = 'RESERVED'
		`, packID, live, now, p.RewriteOf != "")
		if err != nil {
			return fmt.Errorf("publish pack entries: %w", err)
		}
		published, dead = len(live), int(tag.RowsAffected())-len(live)

		if _, err := tx.Exec(ctx, `
			UPDATE packs SET state = 'PUBLISHED', published_at = $2, manifest_sha256 = $3, lease_owner = NULL,
			                 lease_expires_at = NULL, last_error = NULL, updated_at = $2
			WHERE id = $1
		`, packID, now, manifestSHA256); err != nil {
			return fmt.Errorf("publish pack: %w", err)
		}
		if p.RewriteOf != "" {
			if _, err := tx.Exec(ctx, `
				UPDATE packs SET state = 'RETIRING', retired_at = $2, updated_at = $2
				WHERE id = $1 AND state = 'PUBLISHED'
				  AND NOT EXISTS (SELECT 1 FROM pack_entries WHERE pack_id = $1 AND state = 'PUBLISHED')
			`, p.RewriteOf, now); err != nil {
				return fmt.Errorf("retire rewritten pack: %w", err)
			}
		}
		return nil
	})
	if isUniqueViolation(err) {
		return 0, 0, model.ErrPackConflict
	}
	return published, dead, err
}

func collectStrings(rows pgx.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ─── Retirement ───────────────────────────────────────────────────────

func (r *PackRepository) SourcesToRetire(ctx context.Context, cutoff time.Time, limit int) ([]interfaces.PackSource, error) {
	entries, err := collectPackEntries(r.db.Pool.Query(ctx, `
		SELECT `+packEntryColumns+`
		FROM pack_entries e JOIN packs p ON p.id = e.pack_id
		WHERE e.state = 'PUBLISHED' AND e.source_retired_at IS NULL
		  AND p.state = 'PUBLISHED' AND p.published_at <= $1 AND p.rewrite_of IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM assets a JOIN legal_holds h ON h.tenant_id = a.tenant_id AND h.released_at IS NULL
			WHERE a.id = e.asset_id
			  AND (h.scope = 'TENANT'
			       OR (h.scope = 'DATASET' AND COALESCE(a.dataset, '') <> '' AND h.scope_key = a.dataset)
			       OR (h.scope = 'ASSET' AND h.scope_key = a.id)))
		ORDER BY p.published_at, e.pack_id, e.ordinal
		LIMIT $2
	`, cutoff, limit))
	if err != nil {
		return nil, fmt.Errorf("list sources to retire: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.AssetID
	}
	rows, err := r.db.Pool.Query(ctx, `SELECT `+assetColumns+` FROM assets WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read source assets: %w", err)
	}
	defer rows.Close()
	assets := make(map[string]model.Asset, len(ids))
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		assets[a.ID] = *a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]interfaces.PackSource, 0, len(entries))
	for _, e := range entries {
		if a, ok := assets[e.AssetID]; ok && a.TenantID == e.TenantID {
			out = append(out, interfaces.PackSource{Entry: e, Asset: a})
		}
	}
	return out, nil
}

func (r *PackRepository) MarkSourceRetired(ctx context.Context, packID string, ordinal int, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE pack_entries SET source_retired_at = $3, updated_at = $3
		WHERE pack_id = $1 AND ordinal = $2 AND source_retired_at IS NULL
	`, packID, ordinal, now)
	if err != nil {
		return fmt.Errorf("record retired source: %w", err)
	}
	return nil
}

func (r *PackRepository) RetireEmptyPacks(ctx context.Context, now time.Time) (int, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE packs p SET state = 'RETIRING', retired_at = $1, updated_at = $1
		WHERE p.state = 'PUBLISHED'
		  AND NOT EXISTS (SELECT 1 FROM pack_entries e WHERE e.pack_id = p.id AND e.state = 'PUBLISHED')
	`, now)
	if err != nil {
		return 0, fmt.Errorf("retire empty packs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *PackRepository) PacksToRewrite(ctx context.Context, ratio float64, waitedBefore time.Time, limit int) ([]model.Pack, error) {
	packs, err := collectPacks(r.db.Pool.Query(ctx, `
		SELECT `+packColumns+`
		FROM packs p
		WHERE p.state = 'PUBLISHED'
		  AND EXISTS (SELECT 1 FROM pack_entries e WHERE e.pack_id = p.id AND e.state = 'PUBLISHED')
		  AND NOT EXISTS (SELECT 1 FROM packs w WHERE w.rewrite_of = p.id AND w.state IN ('PLANNED', 'WRITING', 'VERIFYING'))
		  AND EXISTS (
			SELECT 1 FROM pack_entries d WHERE d.pack_id = p.id AND d.state = 'DELETED'
			HAVING sum(d.stored_length) >= $1 * p.total_bytes OR min(d.deleted_at) <= $2)
		ORDER BY p.published_at
		LIMIT $3
	`, ratio, waitedBefore, limit))
	if err != nil {
		return nil, fmt.Errorf("list packs to rewrite: %w", err)
	}
	return packs, nil
}

func (r *PackRepository) PacksToRemove(ctx context.Context, cutoff time.Time, limit int) ([]model.Pack, error) {
	packs, err := collectPacks(r.db.Pool.Query(ctx, `
		SELECT `+packColumns+` FROM packs
		WHERE state IN ('RETIRING', 'FAILED') AND updated_at <= $1
		ORDER BY updated_at
		LIMIT $2
	`, cutoff, limit))
	if err != nil {
		return nil, fmt.Errorf("list packs to remove: %w", err)
	}
	return packs, nil
}

func (r *PackRepository) MarkPackDeleted(ctx context.Context, packID string, from model.PackState, now time.Time) error {
	if from != model.PackRetiring && from != model.PackFailed {
		return fmt.Errorf("pack cannot be deleted from %s", from)
	}
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE packs SET state = 'DELETED', deleted_at = $3, updated_at = $3 WHERE id = $1 AND state = $2
	`, packID, from, now)
	if err != nil {
		return fmt.Errorf("record deleted pack: %w", err)
	}
	return nil
}

func (r *PackRepository) PackPlaces(ctx context.Context) ([]model.Pack, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT tenant_id, storage_connection_id FROM packs ORDER BY tenant_id, storage_connection_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list pack places: %w", err)
	}
	defer rows.Close()
	var out []model.Pack
	for rows.Next() {
		var p model.Pack
		if err := rows.Scan(&p.TenantID, &p.ConnectionID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
