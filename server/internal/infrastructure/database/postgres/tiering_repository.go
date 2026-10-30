package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// TieringRepository keeps tier rules, moves, archive restores and cost
// budgets, and reads what costs and snapshot expiry need
// (docs/tiering.md).
type TieringRepository struct {
	db *DB
}

func NewTieringRepository(db *DB) *TieringRepository {
	return &TieringRepository{db: db}
}

// ─── Rules ────────────────────────────────────────────────────────────

const tierRuleColumns = `id, tenant_id, dataset_id, name, tier, min_age_days, unread_days, enabled, created_by, created_at, updated_at`

func collectRules(rows pgx.Rows, err error) ([]model.TierRule, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.TierRule{}
	for rows.Next() {
		var r model.TierRule
		var tier string
		if err := rows.Scan(&r.ID, &r.TenantID, &r.DatasetID, &r.Name, &tier, &r.MinAgeDays, &r.UnreadDays, &r.Enabled,
			&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Tier = model.StorageTier(tier)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (r *TieringRepository) PutRule(ctx context.Context, rule model.TierRule) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO tier_rules (`+tierRuleColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET name = $4, tier = $5, min_age_days = $6, unread_days = $7, enabled = $8, updated_at = $11
	`, rule.ID, rule.TenantID, rule.DatasetID, rule.Name, string(rule.Tier), rule.MinAgeDays, rule.UnreadDays, rule.Enabled,
		rule.CreatedBy, rule.CreatedAt, rule.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store tier rule: %w", err)
	}
	return nil
}

func (r *TieringRepository) Rule(ctx context.Context, tenantID, id string) (model.TierRule, error) {
	rules, err := collectRules(r.db.Pool.Query(ctx, `SELECT `+tierRuleColumns+` FROM tier_rules WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if err != nil {
		return model.TierRule{}, fmt.Errorf("read tier rule: %w", err)
	}
	if len(rules) == 0 {
		return model.TierRule{}, model.ErrTierRuleNotFound
	}
	return rules[0], nil
}

func (r *TieringRepository) Rules(ctx context.Context, tenantID string) ([]model.TierRule, error) {
	return collectRules(r.db.Pool.Query(ctx, `SELECT `+tierRuleColumns+` FROM tier_rules WHERE tenant_id = $1 ORDER BY created_at`, tenantID))
}

func (r *TieringRepository) DeleteRule(ctx context.Context, tenantID, id string) error {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM tier_rules WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete tier rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrTierRuleNotFound
	}
	return nil
}

// EnabledRules lists every organization's enabled rules.
func (r *TieringRepository) EnabledRules(ctx context.Context) ([]model.TierRule, error) {
	return collectRules(r.db.Pool.Query(ctx, `SELECT `+tierRuleColumns+` FROM tier_rules WHERE enabled ORDER BY tenant_id, created_at`))
}

// tierEligible is what a rule asks of an asset a, packed or not:
// published and old enough, not read since $5 (and published before it: a
// new upload has had no chance to be read), in the rule's dataset ($4)
// when it has one, and none of these: under a legal hold or an Object
// Lock; adopted; a member of a snapshot a running training run reads or
// keeps checkpoints in, or of a dataset's clean point.
const tierEligible = `
	a.tenant_id = $1 AND a.status = 'AVAILABLE' AND a.adoption IS NULL
	AND COALESCE(a.published_at, a.created_at) <= LEAST($3::timestamptz, $5::timestamptz)
	AND ($4 = '' OR a.id IN (
		SELECT COALESCE(dm.asset_id, o.current_asset_id) FROM dataset_members dm
		LEFT JOIN catalog_objects o ON o.id = dm.object_id AND o.tenant_id = dm.tenant_id
		WHERE dm.tenant_id = $1 AND dm.dataset_id = $4
		UNION
		SELECT vm.asset_id FROM dataset_version_members vm JOIN dataset_versions v ON v.id = vm.version_id
		WHERE v.tenant_id = $1 AND v.dataset_id = $4))
	AND NOT EXISTS (SELECT 1 FROM delivery_audit_events d
	                WHERE d.tenant_id = a.tenant_id AND d.asset_id = a.id AND d.outcome = 'ALLOWED' AND d.created_at > $5)
	AND NOT EXISTS (SELECT 1 FROM legal_holds h
	                WHERE h.tenant_id = a.tenant_id AND h.released_at IS NULL
	                  AND (h.scope = 'TENANT'
	                       OR (h.scope = 'DATASET' AND COALESCE(a.dataset, '') <> '' AND h.scope_key = a.dataset)
	                       OR (h.scope = 'ASSET' AND h.scope_key = a.id)))
	AND NOT EXISTS (SELECT 1 FROM object_locks l WHERE l.asset_id = a.id AND (l.legal_hold OR l.retain_until > $6))
	AND NOT EXISTS (
	      SELECT 1 FROM dataset_version_members vm
	      WHERE vm.asset_id = a.id AND vm.version_id IN (
	          SELECT ri.version_id FROM training_run_inputs ri JOIN training_runs t ON t.id = ri.run_id
	          WHERE t.tenant_id = a.tenant_id AND t.status = 'RUNNING'
	          UNION
	          SELECT rc.version_id FROM run_checkpoints rc JOIN training_runs t ON t.id = rc.run_id
	          WHERE t.tenant_id = a.tenant_id AND t.status = 'RUNNING'
	          UNION
	          SELECT clean_version_id FROM datasets WHERE tenant_id = a.tenant_id AND clean_version_id IS NOT NULL))
`

// Candidates lists standalone assets the rule may move now: eligible
// (tierEligible), in a warmer tier, and not packed, not sharing stored
// bytes, nor moved and failed in the last day. Packs move whole
// (PackCandidates).
func (r *TieringRepository) Candidates(ctx context.Context, rule model.TierRule, warmer []string, publishedBefore, unreadBefore, now time.Time, limit int) ([]model.TierCandidate, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+`, provider
		FROM (
			SELECT a.*, COALESCE(c.provider, 'PLATFORM_MANAGED') AS provider
			FROM assets a
			LEFT JOIN storage_connections c ON c.id = a.storage_connection_id
			WHERE `+tierEligible+`
			  AND a.storage_tier = ANY($2)
			  AND COALESCE(a.storage_etag, '') <> '' AND COALESCE(a.shared_from, '') = ''
			  AND NOT EXISTS (SELECT 1 FROM pack_entries e WHERE e.asset_id = a.id AND e.state IN ('RESERVED', 'PUBLISHED'))
			  AND NOT EXISTS (SELECT 1 FROM assets s WHERE s.shared_from = a.id AND s.status <> 'DELETED')
			  AND NOT EXISTS (SELECT 1 FROM tier_moves m WHERE m.asset_id = a.id AND m.state = 'FAILED' AND m.created_at > $6::timestamptz - interval '1 day')
			ORDER BY a.id
			LIMIT $7
		) assets
	`, rule.TenantID, warmer, publishedBefore, rule.DatasetID, unreadBefore, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list tiering candidates: %w", err)
	}
	defer rows.Close()
	var out []model.TierCandidate
	for rows.Next() {
		var c model.TierCandidate
		var provider string
		a, err := scanAssetWith(rows, &provider)
		if err != nil {
			return nil, err
		}
		c.Asset, c.Provider = *a, model.StorageProviderType(provider)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PackCandidates lists published packs the rule may move whole: in a
// warmer tier, not being rewritten or repacked, not moved and failed in
// the last day, and every file served from them eligible.
func (r *TieringRepository) PackCandidates(ctx context.Context, rule model.TierRule, warmer []string, publishedBefore, unreadBefore, now time.Time, limit int) ([]model.TierPack, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT p.id, p.tenant_id, p.storage_connection_id, p.bucket, p.object_key, COALESCE(p.storage_etag, ''),
		       COALESCE(p.storage_version_id, ''), p.total_bytes, p.storage_tier, COALESCE(c.provider, 'PLATFORM_MANAGED'),
		       (SELECT count(*) FROM pack_entries e WHERE e.pack_id = p.id AND e.state = 'PUBLISHED')
		FROM packs p
		LEFT JOIN storage_connections c ON c.id = p.storage_connection_id
		WHERE p.tenant_id = $1 AND p.state = 'PUBLISHED' AND p.storage_tier = ANY($2) AND COALESCE(p.storage_etag, '') <> ''
		  AND EXISTS (SELECT 1 FROM pack_entries e WHERE e.pack_id = p.id AND e.state = 'PUBLISHED')
		  AND NOT EXISTS (SELECT 1 FROM pack_entries w WHERE w.source_pack_id = p.id AND w.state = 'RESERVED')
		  AND NOT EXISTS (SELECT 1 FROM tier_moves m WHERE m.asset_id = p.id AND m.state = 'FAILED' AND m.created_at > $6::timestamptz - interval '1 day')
		  AND NOT EXISTS (
		        SELECT 1 FROM pack_entries x JOIN assets a ON a.id = x.asset_id AND a.tenant_id = x.tenant_id
		        WHERE x.pack_id = p.id AND x.state = 'PUBLISHED' AND NOT (`+tierEligible+`))
		ORDER BY p.id
		LIMIT $7
	`, rule.TenantID, warmer, publishedBefore, rule.DatasetID, unreadBefore, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list tiering packs: %w", err)
	}
	defer rows.Close()
	var out []model.TierPack
	for rows.Next() {
		var p model.TierPack
		var tier, provider string
		if err := rows.Scan(&p.ID, &p.TenantID, &p.ConnectionID, &p.Bucket, &p.ObjectKey, &p.ETag, &p.VersionID, &p.Bytes,
			&tier, &provider, &p.Files); err != nil {
			return nil, err
		}
		p.Tier, p.Provider = model.StorageTier(tier), model.StorageProviderType(provider)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPackTier records a pack's new tier and object, only if it is still
// the object the move started from, and gives its files the same tier.
func (r *TieringRepository) SetPackTier(ctx context.Context, tenantID, packID, fromETag string, tier model.StorageTier, etag, versionID string, now time.Time) (bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `
		UPDATE packs SET storage_tier = $4, readable_until = NULL, storage_etag = NULLIF($5, ''),
		       storage_version_id = NULLIF($6, ''), updated_at = $7
		WHERE tenant_id = $1 AND id = $2 AND storage_etag = $3 AND state = 'PUBLISHED'
	`, tenantID, packID, fromETag, string(tier), etag, versionID, now)
	if err != nil {
		return false, fmt.Errorf("record pack tier: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE assets SET storage_tier = $3, tiered_at = $4, readable_until = NULL, updated_at = $4
		WHERE tenant_id = $1 AND id IN (SELECT asset_id FROM pack_entries WHERE pack_id = $2 AND state = 'PUBLISHED')
	`, tenantID, packID, string(tier), now); err != nil {
		return false, fmt.Errorf("record packed files' tier: %w", err)
	}
	return true, tx.Commit(ctx)
}

// PackOf is the published pack an asset is read from, if any.
func (r *TieringRepository) PackOf(ctx context.Context, tenantID, assetID string) (model.TierPack, bool, error) {
	var p model.TierPack
	var tier string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT p.id, p.tenant_id, p.storage_connection_id, p.bucket, p.object_key, COALESCE(p.storage_etag, ''),
		       COALESCE(p.storage_version_id, ''), p.total_bytes, p.storage_tier
		FROM pack_entries e JOIN packs p ON p.id = e.pack_id
		WHERE e.tenant_id = $1 AND e.asset_id = $2 AND e.state = 'PUBLISHED' AND p.state = 'PUBLISHED'
	`, tenantID, assetID).Scan(&p.ID, &p.TenantID, &p.ConnectionID, &p.Bucket, &p.ObjectKey, &p.ETag, &p.VersionID, &p.Bytes, &tier)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("find an asset's pack: %w", err)
	}
	p.Tier = model.StorageTier(tier)
	return p, true, nil
}

// SetPackReadable records until when an archived pack, and every file in
// it, can be read.
func (r *TieringRepository) SetPackReadable(ctx context.Context, tenantID, packID string, tier model.StorageTier, until *time.Time, now time.Time) error {
	if _, err := r.db.Pool.Exec(ctx, `UPDATE packs SET storage_tier = $3, readable_until = $4, updated_at = $5 WHERE tenant_id = $1 AND id = $2`,
		tenantID, packID, string(tier), until, now); err != nil {
		return fmt.Errorf("record pack readable: %w", err)
	}
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE assets SET storage_tier = $3, readable_until = $4, updated_at = $5
		WHERE tenant_id = $1 AND id IN (SELECT asset_id FROM pack_entries WHERE pack_id = $2 AND state = 'PUBLISHED')
	`, tenantID, packID, string(tier), until, now)
	if err != nil {
		return fmt.Errorf("record packed files readable: %w", err)
	}
	return nil
}

func (r *TieringRepository) RecordMove(ctx context.Context, m model.TierMove) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO tier_moves (id, tenant_id, asset_id, rule_id, from_tier, to_tier, size_bytes, state, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, m.ID, m.TenantID, m.AssetID, m.RuleID, string(m.From), string(m.To), m.SizeBytes, m.State, m.Reason, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("record tier move: %w", err)
	}
	return nil
}

func (r *TieringRepository) Moves(ctx context.Context, tenantID string, limit int) ([]model.TierMove, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, asset_id, rule_id, from_tier, to_tier, size_bytes, state, reason, created_at
		FROM tier_moves WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tier moves: %w", err)
	}
	defer rows.Close()
	out := []model.TierMove{}
	for rows.Next() {
		var m model.TierMove
		var from, to string
		if err := rows.Scan(&m.ID, &m.AssetID, &m.RuleID, &from, &to, &m.SizeBytes, &m.State, &m.Reason, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.From, m.To = model.StorageTier(from), model.StorageTier(to)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetTier records an asset's new tier and the object it is now, only if
// it is still the object the move started from.
func (r *TieringRepository) SetTier(ctx context.Context, tenantID, assetID, fromETag string, tier model.StorageTier, etag, versionID string, now time.Time) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE assets SET storage_tier = $4, tiered_at = $7, readable_until = NULL,
		       storage_etag = NULLIF($5, ''), storage_version_id = NULLIF($6, ''), updated_at = $7
		WHERE tenant_id = $1 AND id = $2 AND storage_etag = $3 AND status = 'AVAILABLE'
	`, tenantID, assetID, fromETag, string(tier), etag, versionID, now)
	if err != nil {
		return false, fmt.Errorf("record asset tier: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetReadable records until when an archived asset can be read, and its
// tier as the provider reports it.
func (r *TieringRepository) SetReadable(ctx context.Context, tenantID, assetID string, tier model.StorageTier, until *time.Time, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE assets SET storage_tier = $3, readable_until = $4, updated_at = $5 WHERE tenant_id = $1 AND id = $2
	`, tenantID, assetID, string(tier), until, now)
	if err != nil {
		return fmt.Errorf("record readable until: %w", err)
	}
	return nil
}

// ─── Restores ─────────────────────────────────────────────────────────

const restoreColumns = `id, tenant_id, asset_id, version_id, state, days, speed, error, requested_by, created_at, updated_at,
	ready_at, readable_until`

func collectRestores(rows pgx.Rows, err error) ([]model.ArchiveRestore, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ArchiveRestore{}
	for rows.Next() {
		var x model.ArchiveRestore
		var speed string
		if err := rows.Scan(&x.ID, &x.TenantID, &x.AssetID, &x.VersionID, &x.State, &x.Days, &speed, &x.Error, &x.RequestedBy,
			&x.CreatedAt, &x.UpdatedAt, &x.ReadyAt, &x.ReadableUntil); err != nil {
			return nil, err
		}
		x.Speed = model.RestoreSpeed(speed)
		out = append(out, x)
	}
	return out, rows.Err()
}

// CreateRestore stores a restore, or returns the one already open for the
// asset (created false).
func (r *TieringRepository) CreateRestore(ctx context.Context, x model.ArchiveRestore) (model.ArchiveRestore, bool, error) {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO archive_restores (id, tenant_id, asset_id, version_id, state, days, speed, requested_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'PENDING', $5, $6, $7, $8, $8)
	`, x.ID, x.TenantID, x.AssetID, x.VersionID, x.Days, string(x.Speed), x.RequestedBy, x.CreatedAt)
	if isUniqueViolation(err) {
		open, err := collectRestores(r.db.Pool.Query(ctx, `SELECT `+restoreColumns+` FROM archive_restores
			WHERE tenant_id = $1 AND asset_id = $2 AND state IN ('PENDING', 'RESTORING')`, x.TenantID, x.AssetID))
		if err != nil || len(open) == 0 {
			return model.ArchiveRestore{}, false, fmt.Errorf("read open restore: %v", err)
		}
		return open[0], false, nil
	}
	if err != nil {
		return model.ArchiveRestore{}, false, fmt.Errorf("create restore: %w", err)
	}
	x.State, x.UpdatedAt = "PENDING", x.CreatedAt
	return x, true, nil
}

func (r *TieringRepository) Restores(ctx context.Context, tenantID, assetID, versionID string, limit int) ([]model.ArchiveRestore, error) {
	return collectRestores(r.db.Pool.Query(ctx, `
		SELECT `+restoreColumns+` FROM archive_restores
		WHERE tenant_id = $1 AND ($2 = '' OR asset_id = $2) AND ($3 = '' OR version_id = $3)
		ORDER BY created_at DESC LIMIT $4`, tenantID, assetID, versionID, limit))
}

func (r *TieringRepository) OpenRestores(ctx context.Context, limit int) ([]model.ArchiveRestore, error) {
	return collectRestores(r.db.Pool.Query(ctx, `
		SELECT `+restoreColumns+` FROM archive_restores WHERE state IN ('PENDING', 'RESTORING') ORDER BY updated_at LIMIT $1`, limit))
}

func (r *TieringRepository) UpdateRestore(ctx context.Context, x model.ArchiveRestore) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE archive_restores SET state = $2, error = $3, ready_at = $4, readable_until = $5, updated_at = $6 WHERE id = $1
	`, x.ID, x.State, x.Error, x.ReadyAt, x.ReadableUntil, x.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update restore: %w", err)
	}
	return nil
}

// ExpireRestores ends restores whose readable copy has lapsed.
func (r *TieringRepository) ExpireRestores(ctx context.Context, now time.Time) (int, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE archive_restores SET state = 'EXPIRED', updated_at = $1 WHERE state = 'READY' AND readable_until <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("expire restores: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ArchivedMembers lists a version's members whose assets are archived.
func (r *TieringRepository) ArchivedMembers(ctx context.Context, tenantID, versionID string) ([]model.TierCandidate, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+`, provider
		FROM (
			SELECT a.*, COALESCE(c.provider, 'PLATFORM_MANAGED') AS provider
			FROM dataset_version_members vm
			JOIN assets a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id
			LEFT JOIN storage_connections c ON c.id = a.storage_connection_id
			WHERE vm.tenant_id = $1 AND vm.version_id = $2 AND a.status = 'AVAILABLE'
			  AND a.storage_tier IN ('ARCHIVE', 'DEEP_ARCHIVE')
		) assets`, tenantID, versionID)
	if err != nil {
		return nil, fmt.Errorf("list archived members: %w", err)
	}
	defer rows.Close()
	var out []model.TierCandidate
	for rows.Next() {
		var c model.TierCandidate
		var provider string
		a, err := scanAssetWith(rows, &provider)
		if err != nil {
			return nil, err
		}
		c.Asset, c.Provider = *a, model.StorageProviderType(provider)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ProviderOf is a connection's provider; the platform's for "platform".
func (r *TieringRepository) ProviderOf(ctx context.Context, tenantID, connectionID string) (model.StorageProviderType, error) {
	if connectionID == "" || connectionID == model.PlatformConnectionID {
		return model.ProviderPlatform, nil
	}
	var provider string
	err := r.db.Pool.QueryRow(ctx, `SELECT provider FROM storage_connections WHERE id = $1 AND tenant_id = $2`, connectionID, tenantID).Scan(&provider)
	if err != nil {
		return "", fmt.Errorf("read connection provider: %w", err)
	}
	return model.StorageProviderType(provider), nil
}

// ─── Budgets ──────────────────────────────────────────────────────────

func (r *TieringRepository) Budget(ctx context.Context, tenantID string) (model.CostBudget, bool, error) {
	var b model.CostBudget
	err := r.db.Pool.QueryRow(ctx, `
		SELECT monthly_usd::float8, monthly_requests, action, updated_by, updated_at FROM cost_budgets WHERE tenant_id = $1`, tenantID).
		Scan(&b.MonthlyUSD, &b.MonthlyRequests, &b.Action, &b.UpdatedBy, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.CostBudget{TenantID: tenantID, Action: "ALERT"}, false, nil
	}
	if err != nil {
		return b, false, fmt.Errorf("read cost budget: %w", err)
	}
	b.TenantID = tenantID
	return b, true, nil
}

func (r *TieringRepository) SetBudget(ctx context.Context, b model.CostBudget) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO cost_budgets (tenant_id, monthly_usd, monthly_requests, action, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id) DO UPDATE SET monthly_usd = $2, monthly_requests = $3, action = $4, updated_by = $5, updated_at = $6
	`, b.TenantID, b.MonthlyUSD, b.MonthlyRequests, b.Action, b.UpdatedBy, b.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store cost budget: %w", err)
	}
	return nil
}

// ─── Usage, for costs ─────────────────────────────────────────────────

// StoredBytes is what an organization keeps, by provider and tier. Bytes
// count once: a copy sharing another's stored object adds nothing.
type StoredBytes struct {
	ConnectionID string
	Provider     model.StorageProviderType
	Tier         model.StorageTier
	Objects      int64
	Bytes        int64
}

// Stored sums the organization's stored bytes; with datasetID, those of
// the dataset's members (its draft and snapshots).
func (r *TieringRepository) Stored(ctx context.Context, tenantID, datasetID string) ([]StoredBytes, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT COALESCE(a.storage_connection_id, 'platform'), COALESCE(c.provider, 'PLATFORM_MANAGED'), a.storage_tier,
		       count(*), COALESCE(sum(a.size_bytes), 0)
		FROM assets a
		LEFT JOIN storage_connections c ON c.id = a.storage_connection_id
		WHERE a.tenant_id = $1 AND a.status IN ('AVAILABLE', 'SOFT_DELETED') AND COALESCE(a.shared_from, '') = ''
		  AND ($2 = '' OR a.id IN (
		        SELECT COALESCE(dm.asset_id, o.current_asset_id) FROM dataset_members dm
		        LEFT JOIN catalog_objects o ON o.id = dm.object_id AND o.tenant_id = dm.tenant_id
		        WHERE dm.tenant_id = $1 AND dm.dataset_id = $2
		        UNION
		        SELECT vm.asset_id FROM dataset_version_members vm JOIN dataset_versions v ON v.id = vm.version_id
		        WHERE v.tenant_id = $1 AND v.dataset_id = $2))
		GROUP BY 1, 2, 3 ORDER BY 1, 3`, tenantID, datasetID)
	if err != nil {
		return nil, fmt.Errorf("sum stored bytes: %w", err)
	}
	defer rows.Close()
	var out []StoredBytes
	for rows.Next() {
		var s StoredBytes
		var provider, tier string
		if err := rows.Scan(&s.ConnectionID, &provider, &tier, &s.Objects, &s.Bytes); err != nil {
			return nil, err
		}
		s.Provider, s.Tier = model.StorageProviderType(provider), model.StorageTier(tier)
		out = append(out, s)
	}
	return out, rows.Err()
}

// MonthUsage is an organization's activity since from: reads and bytes
// served (downloads and links), objects written, and restores.
type MonthUsage struct {
	Reads, EgressBytes, Writes, RestoredBytes int64
}

func (r *TieringRepository) Usage(ctx context.Context, tenantID string, from time.Time) (MonthUsage, error) {
	var u MonthUsage
	err := r.db.Pool.QueryRow(ctx, `
		SELECT (SELECT COALESCE(sum(reads), 0) FROM egress_usage WHERE tenant_id = $1 AND day >= $2::date),
		       (SELECT COALESCE(sum(bytes), 0) FROM egress_usage WHERE tenant_id = $1 AND day >= $2::date),
		       (SELECT count(*) FROM assets WHERE tenant_id = $1 AND created_at >= $2),
		       (SELECT COALESCE(sum(a.size_bytes), 0) FROM archive_restores x JOIN assets a ON a.id = x.asset_id
		        WHERE x.tenant_id = $1 AND x.created_at >= $2 AND x.state IN ('RESTORING', 'READY', 'EXPIRED'))
	`, tenantID, from).Scan(&u.Reads, &u.EgressBytes, &u.Writes, &u.RestoredBytes)
	if err != nil {
		return u, fmt.Errorf("read usage: %w", err)
	}
	return u, nil
}

// Savings are what packing and deduplication saved: files served from
// packs (and the packs they sit in), and copies that share stored bytes.
type Savings struct {
	PackedFiles, Packs, SharedCopies, SharedBytes int64
}

func (r *TieringRepository) Savings(ctx context.Context, tenantID string) (Savings, error) {
	var s Savings
	err := r.db.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pack_entries WHERE tenant_id = $1 AND state = 'PUBLISHED'),
		       (SELECT count(*) FROM packs WHERE tenant_id = $1 AND state = 'PUBLISHED'),
		       (SELECT count(*) FROM assets WHERE tenant_id = $1 AND status = 'AVAILABLE' AND COALESCE(shared_from, '') <> ''),
		       (SELECT COALESCE(sum(size_bytes), 0) FROM assets WHERE tenant_id = $1 AND status = 'AVAILABLE' AND COALESCE(shared_from, '') <> '')
	`, tenantID).Scan(&s.PackedFiles, &s.Packs, &s.SharedCopies, &s.SharedBytes)
	if err != nil {
		return s, fmt.Errorf("read savings: %w", err)
	}
	return s, nil
}

// ─── Snapshot expiry ──────────────────────────────────────────────────

// VersionUse is a dataset's version with what keeps it, if anything.
type VersionUse struct {
	ID, Reason string
	Version    int
	Members    int64
	Bytes      int64
	CreatedAt  time.Time
}

// VersionUses lists a dataset's unexpired versions, newest first, each
// with the first reason it must stay: the dataset's current version or
// clean point, a branch's fork or merge base, a derived dataset's source,
// a training run's input or checkpoint, or a legal hold on the dataset.
func (r *TieringRepository) VersionUses(ctx context.Context, tenantID, datasetID string) ([]VersionUse, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT v.id, v.version, v.member_count, v.total_bytes, v.created_at,
		  CASE
		    WHEN EXISTS (SELECT 1 FROM datasets d WHERE d.tenant_id = $1 AND d.current_version_id = v.id) THEN 'the dataset''s current version'
		    WHEN EXISTS (SELECT 1 FROM datasets d WHERE d.tenant_id = $1 AND d.clean_version_id = v.id) THEN 'the dataset''s clean point'
		    WHEN EXISTS (SELECT 1 FROM datasets d WHERE d.tenant_id = $1 AND (d.fork_version_id = v.id OR d.merge_base_version_id = v.id))
		      THEN 'a branch was made from it'
		    WHEN EXISTS (SELECT 1 FROM datasets d WHERE d.tenant_id = $1 AND d.derivation -> 'source_version_ids' ? v.id)
		      THEN 'a derived dataset came from it'
		    WHEN EXISTS (SELECT 1 FROM training_run_inputs ri WHERE ri.version_id = v.id) THEN 'a training run read it'
		    WHEN EXISTS (SELECT 1 FROM run_checkpoints rc WHERE rc.version_id = v.id) THEN 'it holds a training checkpoint'
		    WHEN EXISTS (SELECT 1 FROM legal_holds h JOIN datasets d ON d.id = v.dataset_id
		                 WHERE h.tenant_id = $1 AND h.released_at IS NULL
		                   AND (h.scope = 'TENANT' OR (h.scope = 'DATASET' AND h.scope_key IN (d.name, d.id))))
		      THEN 'a legal hold covers the dataset'
		    ELSE ''
		  END
		FROM dataset_versions v
		WHERE v.tenant_id = $1 AND v.dataset_id = $2 AND v.expired_at IS NULL
		ORDER BY v.version DESC`, tenantID, datasetID)
	if err != nil {
		return nil, fmt.Errorf("list version uses: %w", err)
	}
	defer rows.Close()
	var out []VersionUse
	for rows.Next() {
		var u VersionUse
		if err := rows.Scan(&u.ID, &u.Version, &u.Members, &u.Bytes, &u.CreatedAt, &u.Reason); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Freed lists the assets only the given versions hold: in no other
// unexpired version, no dataset's draft, and not their object's current
// version. Copies sharing stored bytes free nothing on their own.
func (r *TieringRepository) Freed(ctx context.Context, tenantID string, versionIDs []string) ([]model.Asset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+` FROM assets
		WHERE tenant_id = $1 AND status = 'AVAILABLE'
		  AND id IN (SELECT asset_id FROM dataset_version_members WHERE tenant_id = $1 AND version_id = ANY($2))
		  AND NOT EXISTS (SELECT 1 FROM dataset_version_members vm JOIN dataset_versions v ON v.id = vm.version_id
		                  WHERE vm.asset_id = assets.id AND v.expired_at IS NULL AND NOT (v.id = ANY($2)))
		  AND NOT EXISTS (SELECT 1 FROM dataset_members dm WHERE dm.asset_id = assets.id)
		  AND NOT EXISTS (SELECT 1 FROM catalog_objects o WHERE o.current_asset_id = assets.id)
		ORDER BY id`, tenantID, versionIDs)
	if err != nil {
		return nil, fmt.Errorf("list freed assets: %w", err)
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

// Expire marks versions expired and drops their member lists; the version
// records (number, manifest hash, counts) stay.
func (r *TieringRepository) Expire(ctx context.Context, tenantID string, versionIDs []string, by string, now time.Time) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `UPDATE dataset_versions SET expired_at = $3, expired_by = $4
		WHERE tenant_id = $1 AND id = ANY($2) AND expired_at IS NULL`, tenantID, versionIDs, now, by); err != nil {
		return fmt.Errorf("expire versions: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM dataset_version_members WHERE tenant_id = $1 AND version_id = ANY($2)`,
		tenantID, versionIDs); err != nil {
		return fmt.Errorf("drop expired members: %w", err)
	}
	return tx.Commit(ctx)
}

// VersionProfile is how a snapshot's readable members are stored, per
// provider and tier: files, bytes, how many are packed and in how many
// packs, and the archived ones not readable at now.
type VersionProfile struct {
	Provider                         model.StorageProviderType
	Tier                             model.StorageTier
	Files, Bytes, PackedFiles, Packs int64
	SealedFiles, SealedBytes         int64
}

func (r *TieringRepository) VersionProfile(ctx context.Context, tenantID, versionID string, now time.Time) ([]VersionProfile, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT COALESCE(c.provider, 'PLATFORM_MANAGED'), a.storage_tier, count(*), COALESCE(sum(a.size_bytes), 0),
		       count(e.pack_id), count(DISTINCT e.pack_id),
		       count(*) FILTER (WHERE a.readable_until IS NULL OR a.readable_until <= $3),
		       COALESCE(sum(a.size_bytes) FILTER (WHERE a.readable_until IS NULL OR a.readable_until <= $3), 0)
		FROM dataset_version_members vm
		JOIN assets a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id AND a.status = 'AVAILABLE'
		LEFT JOIN storage_connections c ON c.id = a.storage_connection_id
		LEFT JOIN pack_entries e ON e.asset_id = a.id AND e.state = 'PUBLISHED'
		WHERE vm.tenant_id = $1 AND vm.version_id = $2
		GROUP BY 1, 2`, tenantID, versionID, now)
	if err != nil {
		return nil, fmt.Errorf("profile version: %w", err)
	}
	defer rows.Close()
	var out []VersionProfile
	for rows.Next() {
		var v VersionProfile
		var provider, tier string
		if err := rows.Scan(&provider, &tier, &v.Files, &v.Bytes, &v.PackedFiles, &v.Packs, &v.SealedFiles, &v.SealedBytes); err != nil {
			return nil, err
		}
		v.Provider, v.Tier = model.StorageProviderType(provider), model.StorageTier(tier)
		out = append(out, v)
	}
	return out, rows.Err()
}

// VersionObject is a snapshot member and where its bytes are stored.
type VersionObject struct {
	Path, AssetID, ConnectionID, ObjectKey, StorageVersionID, SHA256 string
	Size                                                             int64
	Tier                                                             model.StorageTier
	Packed                                                           bool
}

// VersionObjects pages a version's readable members in path order after
// afterPath.
func (r *TieringRepository) VersionObjects(ctx context.Context, tenantID, versionID, afterPath string, limit int) ([]VersionObject, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT vm.path, a.id, COALESCE(a.storage_connection_id, 'platform'), a.object_key, COALESCE(a.storage_version_id, ''),
		       COALESCE(a.checksum_sha256, ''), a.size_bytes, a.storage_tier,
		       EXISTS (SELECT 1 FROM pack_entries e WHERE e.asset_id = a.id AND e.state = 'PUBLISHED')
		FROM dataset_version_members vm
		JOIN assets a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id AND a.status = 'AVAILABLE'
		WHERE vm.tenant_id = $1 AND vm.version_id = $2 AND vm.path COLLATE "C" > $3 COLLATE "C"
		ORDER BY vm.path COLLATE "C"
		LIMIT $4`, tenantID, versionID, afterPath, limit)
	if err != nil {
		return nil, fmt.Errorf("list version objects: %w", err)
	}
	defer rows.Close()
	var out []VersionObject
	for rows.Next() {
		var o VersionObject
		var tier string
		if err := rows.Scan(&o.Path, &o.AssetID, &o.ConnectionID, &o.ObjectKey, &o.StorageVersionID, &o.SHA256, &o.Size, &tier, &o.Packed); err != nil {
			return nil, err
		}
		o.Tier = model.StorageTier(tier)
		out = append(out, o)
	}
	return out, rows.Err()
}
