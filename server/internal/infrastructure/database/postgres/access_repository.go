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

// AccessRepository keeps API tokens, access policies, egress counts, share
// link inventory and security alerts (migration 000032).
type AccessRepository struct {
	db *DB
}

func NewAccessRepository(db *DB) *AccessRepository {
	return &AccessRepository{db: db}
}

// ─── API tokens ──────────────────────────────────────────────────────

const tokenColumns = `id, tenant_id, user_id, name, token_hash, hint, permissions, datasets, networks, allow_links,
	expires_at, created_at, last_used_at, COALESCE(last_used_ip, ''), revoked_at, COALESCE(revoked_by, '')`

func scanToken(row pgx.Row) (model.APIToken, error) {
	var t model.APIToken
	var perms int64
	err := row.Scan(&t.ID, &t.TenantID, &t.UserID, &t.Name, &t.TokenHash, &t.Hint, &perms, &t.Datasets, &t.Networks,
		&t.AllowLinks, &t.ExpiresAt, &t.CreatedAt, &t.LastUsedAt, &t.LastUsedIP, &t.RevokedAt, &t.RevokedBy)
	t.Permissions = model.Permission(perms)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, model.ErrAPITokenNotFound
	}
	return t, err
}

func (r *AccessRepository) CreateToken(ctx context.Context, t model.APIToken) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO api_tokens (id, tenant_id, user_id, name, token_hash, hint, permissions, datasets, networks, allow_links,
			expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		t.ID, t.TenantID, t.UserID, t.Name, t.TokenHash, t.Hint, int64(t.Permissions), nonNil(t.Datasets), nonNil(t.Networks),
		t.AllowLinks, t.ExpiresAt, t.CreatedAt)
	if err != nil {
		return fmt.Errorf("create API token: %w", err)
	}
	return nil
}

func (r *AccessRepository) TokenByHash(ctx context.Context, hash string) (model.APIToken, error) {
	return scanToken(r.db.Pool.QueryRow(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE token_hash = $1`, hash))
}

func (r *AccessRepository) TouchToken(ctx context.Context, id, ip string, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2, last_used_ip = NULLIF($3, '') WHERE id = $1`, id, at, ip)
	return err
}

// ListTokens lists the organization's tokens, or one member's.
func (r *AccessRepository) ListTokens(ctx context.Context, tenantID, userID string) ([]model.APIToken, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+tokenColumns+` FROM api_tokens
		WHERE tenant_id = $1 AND ($2 = '' OR user_id = $2) ORDER BY created_at DESC LIMIT 500`, tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("list API tokens: %w", err)
	}
	return collect(rows, scanToken)
}

func (r *AccessRepository) GetToken(ctx context.Context, tenantID, id string) (model.APIToken, error) {
	return scanToken(r.db.Pool.QueryRow(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

func (r *AccessRepository) RevokeToken(ctx context.Context, tenantID, id, by string, at time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE api_tokens SET revoked_at = $3, revoked_by = $4
		WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`, tenantID, id, at, by)
	if err != nil {
		return fmt.Errorf("revoke API token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrAPITokenNotFound
	}
	return nil
}

// ─── Policy ──────────────────────────────────────────────────────────

func (r *AccessRepository) Policy(ctx context.Context, tenantID string) (model.AccessPolicy, error) {
	p := model.DefaultAccessPolicy(tenantID)
	var ttl int64
	err := r.db.Pool.QueryRow(ctx, `
		SELECT links_enabled, max_link_ttl_seconds, egress_monthly_bytes, egress_daily_member_bytes, egress_action,
		       mass_link_threshold, anomaly_action, updated_by, updated_at
		FROM tenant_access_policy WHERE tenant_id = $1`, tenantID).
		Scan(&p.LinksEnabled, &ttl, &p.EgressMonthlyBytes, &p.EgressDailyMemberBytes, &p.EgressAction,
			&p.MassLinkThreshold, &p.AnomalyAction, &p.UpdatedBy, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	p.MaxLinkTTL = time.Duration(ttl) * time.Second
	return p, err
}

func (r *AccessRepository) PutPolicy(ctx context.Context, p model.AccessPolicy) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO tenant_access_policy (tenant_id, links_enabled, max_link_ttl_seconds, egress_monthly_bytes,
			egress_daily_member_bytes, egress_action, mass_link_threshold, anomaly_action, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id) DO UPDATE SET links_enabled = $2, max_link_ttl_seconds = $3, egress_monthly_bytes = $4,
			egress_daily_member_bytes = $5, egress_action = $6, mass_link_threshold = $7, anomaly_action = $8,
			updated_by = $9, updated_at = $10`,
		p.TenantID, p.LinksEnabled, int64(p.MaxLinkTTL/time.Second), p.EgressMonthlyBytes, p.EgressDailyMemberBytes,
		p.EgressAction, p.MassLinkThreshold, p.AnomalyAction, p.UpdatedBy, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save access policy: %w", err)
	}
	return nil
}

// ─── Egress ──────────────────────────────────────────────────────────

func (r *AccessRepository) AddEgress(ctx context.Context, tenantID, principalID string, bytes int64, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO egress_usage (tenant_id, day, principal_id, bytes, reads) VALUES ($1, $2::date, $3, $4, 1)
		ON CONFLICT (tenant_id, day, principal_id) DO UPDATE SET bytes = egress_usage.bytes + $4, reads = egress_usage.reads + 1`,
		tenantID, at.UTC().Format("2006-01-02"), principalID, bytes)
	return err
}

// Egress is the organization's bytes this month and the member's today.
func (r *AccessRepository) Egress(ctx context.Context, tenantID, principalID string, at time.Time) (month, day int64, err error) {
	at = at.UTC()
	first := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	err = r.db.Pool.QueryRow(ctx, `
		SELECT COALESCE(sum(bytes), 0), COALESCE(sum(bytes) FILTER (WHERE day = $3::date AND principal_id = $4), 0)
		FROM egress_usage WHERE tenant_id = $1 AND day >= $2::date`,
		tenantID, first.Format("2006-01-02"), at.Format("2006-01-02"), principalID).Scan(&month, &day)
	return month, day, err
}

// EgressByMember lists each member's downloads since from.
func (r *AccessRepository) EgressByMember(ctx context.Context, tenantID string, from time.Time) ([]model.EgressUse, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT principal_id, sum(bytes), sum(reads) FROM egress_usage WHERE tenant_id = $1 AND day >= $2::date
		GROUP BY principal_id ORDER BY 2 DESC LIMIT 200`, tenantID, from.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (model.EgressUse, error) {
		var u model.EgressUse
		err := row.Scan(&u.PrincipalID, &u.Bytes, &u.Reads)
		return u, err
	})
}

// ─── Share links ─────────────────────────────────────────────────────

// Links lists download tokens, newest first; active only when asked.
func (r *AccessRepository) Links(ctx context.Context, tenantID, principalID string, active bool, now time.Time, limit int) ([]model.ShareLink, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT t.id, t.asset_id, COALESCE(a.file_name, ''), t.principal_id, t.created_at, t.expires_at, t.uses, t.last_used_at,
		       t.revoked_at, COALESCE(t.revoked_by, '')
		FROM download_tokens AS t LEFT JOIN assets AS a ON a.id = t.asset_id
		WHERE t.tenant_id = $1 AND ($2 = '' OR t.principal_id = $2)
		  AND (NOT $3 OR (t.revoked_at IS NULL AND t.expires_at > $4))
		ORDER BY t.created_at DESC LIMIT $5`, tenantID, principalID, active, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list share links: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.ShareLink, error) {
		var l model.ShareLink
		err := row.Scan(&l.ID, &l.AssetID, &l.FileName, &l.PrincipalID, &l.CreatedAt, &l.ExpiresAt, &l.Uses, &l.LastUsedAt,
			&l.RevokedAt, &l.RevokedBy)
		return l, err
	})
}

// LinkFilter selects live links to revoke; empty fields match anything,
// but at least one must be set or All.
type LinkFilter struct {
	IDs, AssetIDs []string
	PrincipalID   string
	All           bool
}

// RevokeLinks revokes the live links that match.
func (r *AccessRepository) RevokeLinks(ctx context.Context, tenantID string, f LinkFilter, by string, at time.Time) (int64, error) {
	if !f.All && len(f.IDs) == 0 && len(f.AssetIDs) == 0 && f.PrincipalID == "" {
		return 0, errors.New("choose links to revoke")
	}
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE download_tokens SET revoked_at = $6, revoked_by = $7
		WHERE tenant_id = $1 AND revoked_at IS NULL AND expires_at > $6
		  AND (cardinality($2::text[]) = 0 OR id = ANY($2))
		  AND (cardinality($3::text[]) = 0 OR asset_id = ANY($3))
		  AND ($4 = '' OR principal_id = $4)
		  AND ($5 OR cardinality($2::text[]) > 0 OR cardinality($3::text[]) > 0 OR $4 <> '')`,
		tenantID, nonNil(f.IDs), nonNil(f.AssetIDs), f.PrincipalID, f.All, at, by)
	if err != nil {
		return 0, fmt.Errorf("revoke share links: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LinkUsed counts one download through a link.
func (r *AccessRepository) LinkUsed(ctx context.Context, tokenHash string, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE download_tokens SET uses = uses + 1, last_used_at = $2 WHERE token_hash = $1`, tokenHash, at)
	return err
}

// ─── Classification ──────────────────────────────────────────────────

// SetClassification sets an asset's class; with raiseOnly, only to a
// more restricted one.
func (r *AccessRepository) SetClassification(ctx context.Context, tenantID, assetID, class string, raiseOnly bool) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE assets SET classification = $3
		WHERE tenant_id = $1 AND id = $2 AND (NOT $4 OR
			array_position(ARRAY['PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED'], COALESCE(classification, 'PUBLIC'))
			< array_position(ARRAY['PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED'], $3))`, tenantID, assetID, class, raiseOnly)
	if err != nil {
		return fmt.Errorf("classify asset: %w", err)
	}
	if tag.RowsAffected() == 0 && !raiseOnly {
		return model.ErrAssetNotFound
	}
	return nil
}

// ─── Alerts ──────────────────────────────────────────────────────────

const alertColumns = `id, tenant_id, principal_id, kind, detail, metrics, action_taken, state, dedup_key, created_at,
	COALESCE(acknowledged_by, ''), acknowledged_at`

func scanAlert(row pgx.Row) (model.SecurityAlert, error) {
	var a model.SecurityAlert
	var metrics []byte
	err := row.Scan(&a.ID, &a.TenantID, &a.PrincipalID, &a.Kind, &a.Detail, &metrics, &a.ActionTaken, &a.State, &a.DedupKey,
		&a.CreatedAt, &a.AcknowledgedBy, &a.AcknowledgedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, model.ErrAlertNotFound
	}
	_ = json.Unmarshal(metrics, &a.Metrics)
	return a, err
}

// AddAlert records an alert once per dedup key; false when it already was.
func (r *AccessRepository) AddAlert(ctx context.Context, a model.SecurityAlert) (bool, error) {
	metrics, _ := json.Marshal(a.Metrics)
	tag, err := r.db.Pool.Exec(ctx, `
		INSERT INTO security_alerts (id, tenant_id, principal_id, kind, detail, metrics, action_taken, dedup_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (tenant_id, dedup_key) DO NOTHING`,
		a.ID, a.TenantID, a.PrincipalID, a.Kind, a.Detail, metrics, a.ActionTaken, a.DedupKey, a.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("record security alert: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *AccessRepository) Alerts(ctx context.Context, tenantID, state string, limit int) ([]model.SecurityAlert, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+alertColumns+` FROM security_alerts
		WHERE tenant_id = $1 AND ($2 = '' OR state = $2) ORDER BY created_at DESC LIMIT $3`, tenantID, state, limit)
	if err != nil {
		return nil, fmt.Errorf("list security alerts: %w", err)
	}
	return collect(rows, scanAlert)
}

func (r *AccessRepository) Acknowledge(ctx context.Context, tenantID, id, by string, at time.Time) (model.SecurityAlert, error) {
	return scanAlert(r.db.Pool.QueryRow(ctx, `
		UPDATE security_alerts SET state = 'ACKNOWLEDGED', acknowledged_by = $3, acknowledged_at = $4
		WHERE tenant_id = $1 AND id = $2 RETURNING `+alertColumns, tenantID, id, by, at))
}

// ─── Read statistics, for anomaly detection ──────────────────────────

// ReadCount is one member's reads in a window.
type ReadCount struct {
	TenantID, PrincipalID string
	Reads, Links          int64
}

// RecentReads counts each member's allowed reads, and the links among them,
// since from.
func (r *AccessRepository) RecentReads(ctx context.Context, from time.Time) ([]ReadCount, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT tenant_id, principal_id, count(*), count(*) FILTER (WHERE operation = 'DOWNLOAD_URL')
		FROM delivery_audit_events
		WHERE outcome = 'ALLOWED' AND created_at >= $1 AND operation IN ('DOWNLOAD_URL', 'PROXY_DOWNLOAD', 'TOKEN_DOWNLOAD')
		GROUP BY 1, 2`, from)
	if err != nil {
		return nil, fmt.Errorf("count recent reads: %w", err)
	}
	return collect(rows, func(row pgx.Row) (ReadCount, error) {
		var c ReadCount
		err := row.Scan(&c.TenantID, &c.PrincipalID, &c.Reads, &c.Links)
		return c, err
	})
}

// HourlyReads counts a member's reads per hour in [from, to), and when
// their first read on record was.
func (r *AccessRepository) HourlyReads(ctx context.Context, tenantID, principalID string, from, to time.Time) (map[time.Time]int64, time.Time, error) {
	out := map[time.Time]int64{}
	var first *time.Time
	if err := r.db.Pool.QueryRow(ctx, `SELECT min(created_at) FROM delivery_audit_events
		WHERE tenant_id = $1 AND principal_id = $2 AND outcome = 'ALLOWED'`, tenantID, principalID).Scan(&first); err != nil {
		return nil, time.Time{}, err
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT date_trunc('hour', created_at), count(*) FROM delivery_audit_events
		WHERE tenant_id = $1 AND principal_id = $2 AND outcome = 'ALLOWED' AND created_at >= $3 AND created_at < $4
		  AND operation IN ('DOWNLOAD_URL', 'PROXY_DOWNLOAD', 'TOKEN_DOWNLOAD')
		GROUP BY 1`, tenantID, principalID, from, to)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var h time.Time
		var n int64
		if err := rows.Scan(&h, &n); err != nil {
			return nil, time.Time{}, err
		}
		out[h.UTC()] = n
	}
	var f time.Time
	if first != nil {
		f = *first
	}
	return out, f, rows.Err()
}

// SourceIPs lists the distinct addresses a member read from in [from, to).
func (r *AccessRepository) SourceIPs(ctx context.Context, tenantID, principalID string, from, to time.Time) ([]string, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT source_ip FROM delivery_audit_events
		WHERE tenant_id = $1 AND principal_id = $2 AND outcome = 'ALLOWED' AND source_ip IS NOT NULL
		  AND created_at >= $3 AND created_at < $4 LIMIT 1000`, tenantID, principalID, from, to)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (string, error) {
		var ip string
		err := row.Scan(&ip)
		return ip, err
	})
}
