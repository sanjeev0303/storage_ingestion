package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// GovernanceRepository keeps governance settings, approvals and
// disposition certificates (migration 000030), and answers the scope
// questions of dry runs and hold proofs.
type GovernanceRepository struct {
	db *DB
}

func NewGovernanceRepository(db *DB) *GovernanceRepository {
	return &GovernanceRepository{db: db}
}

func (r *GovernanceRepository) GetSettings(ctx context.Context, tenantID string) (model.GovernanceSettings, error) {
	g := model.GovernanceSettings{TenantID: tenantID, Profile: model.ProfileStandard}
	var floor int64
	err := r.db.Pool.QueryRow(ctx, `
		SELECT profile, two_person, approve_deletes, retention_floor_seconds, updated_by, updated_at
		FROM tenant_governance WHERE tenant_id = $1`, tenantID).
		Scan(&g.Profile, &g.TwoPerson, &g.ApproveDeletes, &floor, &g.UpdatedBy, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, nil
	}
	if err != nil {
		return g, fmt.Errorf("read governance settings: %w", err)
	}
	g.RetentionFloor = time.Duration(floor) * time.Second
	return g, nil
}

func (r *GovernanceRepository) PutSettings(ctx context.Context, g model.GovernanceSettings) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO tenant_governance (tenant_id, profile, two_person, approve_deletes, retention_floor_seconds, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id) DO UPDATE SET profile = $2, two_person = $3, approve_deletes = $4,
			retention_floor_seconds = $5, updated_by = $6, updated_at = $7`,
		g.TenantID, g.Profile, g.TwoPerson, g.ApproveDeletes, int64(g.RetentionFloor/time.Second), g.UpdatedBy, g.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save governance settings: %w", err)
	}
	return nil
}

const approvalColumns = `id, tenant_id, kind, summary, payload, state, reason, requested_by, requested_at, expires_at,
	COALESCE(decided_by, ''), decided_at, decision_note, result`

func scanApproval(row pgx.Row) (model.Approval, error) {
	var a model.Approval
	err := row.Scan(&a.ID, &a.TenantID, &a.Kind, &a.Summary, &a.Payload, &a.State, &a.Reason, &a.RequestedBy,
		&a.RequestedAt, &a.ExpiresAt, &a.DecidedBy, &a.DecidedAt, &a.DecisionNote, &a.Result)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, model.ErrApprovalNotFound
	}
	return a, err
}

func (r *GovernanceRepository) CreateApproval(ctx context.Context, a model.Approval) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO governance_approvals (id, tenant_id, kind, summary, payload, state, reason, requested_by, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'PENDING', $6, $7, $8, $9)`,
		a.ID, a.TenantID, a.Kind, a.Summary, []byte(a.Payload), a.Reason, a.RequestedBy, a.RequestedAt, a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("record approval request: %w", err)
	}
	return nil
}

func (r *GovernanceRepository) GetApproval(ctx context.Context, tenantID, id string) (model.Approval, error) {
	return scanApproval(r.db.Pool.QueryRow(ctx, `SELECT `+approvalColumns+` FROM governance_approvals WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

// ListApprovals lists the newest approvals, PENDING ones first; state ""
// lists every state. Pending ones past their expiry are expired first.
func (r *GovernanceRepository) ListApprovals(ctx context.Context, tenantID, state string, now time.Time, limit int) ([]model.Approval, error) {
	if _, err := r.db.Pool.Exec(ctx, `
		UPDATE governance_approvals SET state = 'EXPIRED', decided_at = $2
		WHERE tenant_id = $1 AND state = 'PENDING' AND expires_at <= $2`, tenantID, now); err != nil {
		return nil, fmt.Errorf("expire approvals: %w", err)
	}
	rows, err := r.db.Pool.Query(ctx, `SELECT `+approvalColumns+` FROM governance_approvals
		WHERE tenant_id = $1 AND ($2 = '' OR state = $2)
		ORDER BY state = 'PENDING' DESC, requested_at DESC LIMIT $3`, tenantID, state, limit)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	return collect(rows, scanApproval)
}

// Decide moves a PENDING, unexpired approval to state, once: of two
// deciders racing, one wins and the other gets ErrApprovalDecided.
func (r *GovernanceRepository) Decide(ctx context.Context, tenantID, id, state, by, note string, at time.Time) (model.Approval, error) {
	a, err := scanApproval(r.db.Pool.QueryRow(ctx, `
		UPDATE governance_approvals SET state = $3, decided_by = $4, decision_note = $5, decided_at = $6
		WHERE tenant_id = $1 AND id = $2 AND state = 'PENDING' AND expires_at > $6
		RETURNING `+approvalColumns, tenantID, id, state, by, note, at))
	if errors.Is(err, model.ErrApprovalNotFound) {
		if _, getErr := r.GetApproval(ctx, tenantID, id); getErr != nil {
			return a, getErr
		}
		return a, model.ErrApprovalDecided
	}
	return a, err
}

// Finish records how an executed approval ended.
func (r *GovernanceRepository) Finish(ctx context.Context, id, state, result string) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE governance_approvals SET state = $2, result = $3 WHERE id = $1`, id, state, result)
	return err
}

// PutCertificate stores a deletion's certificate once; a retried
// completion keeps the first.
func (r *GovernanceRepository) PutCertificate(ctx context.Context, c model.DispositionCertificate) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO disposition_certificates (id, tenant_id, deletion_id, asset_id, body, body_sha256, key_id, signature, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (deletion_id) DO NOTHING`,
		c.ID, c.TenantID, c.DeletionID, c.AssetID, string(c.Body), c.BodySHA256, c.KeyID, c.Signature, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("store disposition certificate: %w", err)
	}
	return nil
}

const certificateColumns = `id, tenant_id, deletion_id, asset_id, body, body_sha256, key_id, signature, created_at`

func scanCertificate(row pgx.Row) (model.DispositionCertificate, error) {
	var c model.DispositionCertificate
	err := row.Scan(&c.ID, &c.TenantID, &c.DeletionID, &c.AssetID, &c.SignedBody, &c.BodySHA256, &c.KeyID, &c.Signature, &c.CreatedAt)
	c.Body = json.RawMessage(c.SignedBody)
	return c, err
}

// Certificates lists an asset's certificates, or one deletion's.
func (r *GovernanceRepository) Certificates(ctx context.Context, tenantID, assetID, deletionID string) ([]model.DispositionCertificate, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+certificateColumns+` FROM disposition_certificates
		WHERE tenant_id = $1 AND ($2 = '' OR asset_id = $2) AND ($3 = '' OR deletion_id = $3)
		ORDER BY created_at DESC LIMIT 100`, tenantID, assetID, deletionID)
	if err != nil {
		return nil, fmt.Errorf("list disposition certificates: %w", err)
	}
	return collect(rows, scanCertificate)
}

// QueueWebhook queues an event for the tenant's subscribed endpoints.
func (r *GovernanceRepository) QueueWebhook(ctx context.Context, tenantID, eventType string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := r.db.Pool.Exec(ctx, `SELECT queue_webhook($1, $2, $3::jsonb)`, tenantID, eventType, string(body)); err != nil {
		return fmt.Errorf("queue webhook: %w", err)
	}
	return nil
}

// ScopeImpact counts the available assets and bytes a policy scope covers.
func (r *GovernanceRepository) ScopeImpact(ctx context.Context, tenantID, scope, key string) (int64, int64, error) {
	var n, bytes int64
	err := r.db.Pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(size_bytes), 0) FROM assets
		WHERE tenant_id = $1 AND status = 'AVAILABLE'
		  AND CASE $2 WHEN 'TENANT' THEN true WHEN 'DATASET' THEN dataset = $3 ELSE id = $3 END`,
		tenantID, scope, key).Scan(&n, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("measure policy scope: %w", err)
	}
	return n, bytes, nil
}

// HoldAssets lists the assets (any state but deleted) a hold covers, with
// their recorded Object Lock, after afterID.
func (r *GovernanceRepository) HoldAssets(ctx context.Context, hold model.LegalHold, afterID string, limit int) ([]model.HoldCoverage, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT a.*, l.retain_until, l.legal_hold, l.applied_at, COALESCE(l.mode, ''), l.verified_at, COALESCE(l.verify_result, '')
		FROM (
			SELECT `+assetColumns+` FROM assets
			WHERE tenant_id = $1 AND status <> 'DELETED' AND id > $4
			  AND CASE $2 WHEN 'TENANT' THEN true WHEN 'DATASET' THEN dataset = $3 WHEN 'ASSET' THEN id = $3
			      ELSE EXISTS (SELECT 1 FROM legal_holds AS h WHERE h.tenant_id = $1 AND h.id = $5 AND `+matchesQuery("h", "assets.id")+`) END
			ORDER BY id LIMIT $6
		) AS a
		LEFT JOIN object_locks AS l ON l.asset_id = a.id
		ORDER BY a.id`, hold.TenantID, hold.Scope, hold.ScopeKey, afterID, hold.ID, limit)
	if err != nil {
		return nil, fmt.Errorf("list held assets: %w", err)
	}
	defer rows.Close()
	var out []model.HoldCoverage
	for rows.Next() {
		var until, verified *time.Time
		var held *bool
		var applied *time.Time
		var mode, result string
		scanned, err := scanAssetWith(rows, &until, &held, &applied, &mode, &verified, &result)
		if err != nil {
			return nil, err
		}
		a := *scanned
		c := model.HoldCoverage{Asset: a}
		if applied != nil {
			c.Lock = &model.ObjectLock{AssetID: a.ID, TenantID: a.TenantID, RetainUntil: until, LegalHold: held != nil && *held,
				AppliedAt: *applied, Mode: mode, VerifiedAt: verified, VerifyResult: result}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FindAssets resolves a bulk selection to asset IDs: by dataset, path
// prefix, metadata or explicit IDs, among assets not already deleted.
func (r *GovernanceRepository) FindAssets(ctx context.Context, tenantID string, q model.HoldQuery, ids []string, limit int) ([]model.Asset, error) {
	if ids == nil {
		ids = []string{} // NULL would make cardinality() NULL and match nothing
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+` FROM assets
		WHERE tenant_id = $1 AND status IN ('AVAILABLE', 'REJECTED')
		  AND (cardinality($2::text[]) = 0 OR id = ANY($2))
		  AND ($3 = '' OR dataset = $3)
		  AND ($4 = '' OR starts_with(COALESCE((SELECT qo.path FROM catalog_objects AS qo
		        WHERE qo.id = assets.object_id AND qo.tenant_id = assets.tenant_id), file_name), $4))
		  AND COALESCE(metadata, '{}'::jsonb) @> $5::jsonb
		ORDER BY id LIMIT $6`,
		tenantID, ids, q.Dataset, q.PathPrefix, metadataJSON(q.Metadata), limit)
	if err != nil {
		return nil, fmt.Errorf("select assets: %w", err)
	}
	found, err := collect(rows, scanAsset)
	if err != nil {
		return nil, err
	}
	out := make([]model.Asset, len(found))
	for i, a := range found {
		out[i] = *a
	}
	return out, nil
}

func metadataJSON(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}
