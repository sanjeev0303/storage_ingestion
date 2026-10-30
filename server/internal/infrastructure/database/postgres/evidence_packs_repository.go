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

// CustodyRow is a snapshot member with who brought it in and how its
// stored version is protected.
type CustodyRow struct {
	Ordinal                      int64
	Path, AssetID, ObjectID      string
	SHA256, ContentType          string
	Size                         int64
	UploadedBy                   string
	UploadedAt                   *time.Time
	ConnectionID, StorageVersion string
	Tier                         string
	LockMode                     string
	RetainUntil                  *time.Time
	LegalHold                    bool
	Status                       string
}

func (r *EvidenceRepository) Custody(ctx context.Context, tenantID, versionID string) ([]CustodyRow, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT vm.ordinal, vm.path, vm.asset_id, vm.object_id, COALESCE(vm.checksum_sha256, ''), vm.content_type, vm.size_bytes,
		       COALESCE(a.owner_id, ''), COALESCE(a.published_at, a.created_at), COALESCE(a.storage_connection_id, 'platform'),
		       COALESCE(a.storage_version_id, ''), COALESCE(a.storage_tier, 'STANDARD'), COALESCE(l.mode, ''), l.retain_until,
		       COALESCE(l.legal_hold, false), COALESCE(a.status, 'DELETED')
		FROM dataset_version_members vm
		LEFT JOIN assets a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id
		LEFT JOIN object_locks l ON l.asset_id = vm.asset_id
		WHERE vm.tenant_id = $1 AND vm.version_id = $2
		ORDER BY vm.ordinal`, tenantID, versionID)
	if err != nil {
		return nil, fmt.Errorf("read custody: %w", err)
	}
	defer rows.Close()
	var out []CustodyRow
	for rows.Next() {
		var c CustodyRow
		if err := rows.Scan(&c.Ordinal, &c.Path, &c.AssetID, &c.ObjectID, &c.SHA256, &c.ContentType, &c.Size, &c.UploadedBy, &c.UploadedAt,
			&c.ConnectionID, &c.StorageVersion, &c.Tier, &c.LockMode, &c.RetainUntil, &c.LegalHold, &c.Status); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReadRow is one read of an asset.
type ReadRow struct {
	Principal string    `json:"principal"`
	Operation string    `json:"operation"`
	SourceIP  string    `json:"source_ip,omitempty"`
	At        time.Time `json:"at"`
}

// Reads lists the reads of each asset, newest first, at most limit each.
func (r *EvidenceRepository) Reads(ctx context.Context, tenantID string, assetIDs []string, limit int) (map[string][]ReadRow, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT asset_id, principal_id, operation, COALESCE(source_ip, ''), created_at FROM (
			SELECT d.*, row_number() OVER (PARTITION BY asset_id ORDER BY created_at DESC) AS n
			FROM delivery_audit_events d WHERE tenant_id = $1 AND asset_id = ANY($2) AND outcome = 'ALLOWED'
		) x WHERE n <= $3 ORDER BY asset_id, created_at DESC`, tenantID, assetIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("read reads: %w", err)
	}
	defer rows.Close()
	out := map[string][]ReadRow{}
	for rows.Next() {
		var id string
		var x ReadRow
		if err := rows.Scan(&id, &x.Principal, &x.Operation, &x.SourceIP, &x.At); err != nil {
			return nil, err
		}
		out[id] = append(out[id], x)
	}
	return out, rows.Err()
}

func (r *EvidenceRepository) Digests(ctx context.Context, assetIDs []string) (map[string]map[string]string, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT asset_id, digests FROM asset_digests WHERE asset_id = ANY($1)`, assetIDs)
	if err != nil {
		return nil, fmt.Errorf("read digests: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		d := map[string]string{}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		out[id] = d
	}
	return out, rows.Err()
}

// PutDigests adds digests to those kept for an asset.
func (r *EvidenceRepository) PutDigests(ctx context.Context, tenantID, assetID string, digests map[string]string, at time.Time) error {
	raw, err := json.Marshal(digests)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `
		INSERT INTO asset_digests (asset_id, tenant_id, digests, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (asset_id) DO UPDATE SET digests = asset_digests.digests || $3`, assetID, tenantID, raw, at)
	if err != nil {
		return fmt.Errorf("record digests: %w", err)
	}
	return nil
}

// ─── Models ───────────────────────────────────────────────────────────

// ModelRow is a model and the run that produced it.
type ModelRow struct {
	ID, Name, Version, URI, RunID, RunName string
	CreatedAt, RunCreatedAt                time.Time
	SummaryPublishedAt                     *time.Time
	VersionIDs                             []string
}

func (r *EvidenceRepository) Model(ctx context.Context, tenantID, modelID string) (ModelRow, error) {
	var m ModelRow
	err := r.db.Pool.QueryRow(ctx, `
		SELECT m.id, m.name, m.version, m.uri, m.run_id, t.name, m.created_at, t.created_at, m.summary_published_at
		FROM models m JOIN training_runs t ON t.id = m.run_id
		WHERE m.tenant_id = $1 AND m.id = $2`, tenantID, modelID).
		Scan(&m.ID, &m.Name, &m.Version, &m.URI, &m.RunID, &m.RunName, &m.CreatedAt, &m.RunCreatedAt, &m.SummaryPublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, model.ErrModelNotFound
	}
	if err != nil {
		return m, fmt.Errorf("read model: %w", err)
	}
	rows, err := r.db.Pool.Query(ctx, `SELECT version_id FROM training_run_inputs WHERE run_id = $1 ORDER BY version_id`, m.RunID)
	if err != nil {
		return m, err
	}
	m.VersionIDs, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return m, err
}

// ContentProfile is what a snapshot holds, for a training summary: by
// modality (the content type's first part), the collection period, and
// the licences and sources its members name.
type ContentProfile struct {
	Modalities map[string][2]int64 // files, bytes
	First      *time.Time
	Last       *time.Time
	Licenses   map[string]int64
	Sources    map[string]int64
}

func (r *EvidenceRepository) VersionProfile(ctx context.Context, tenantID, versionID string) (ContentProfile, error) {
	p := ContentProfile{Modalities: map[string][2]int64{}, Licenses: map[string]int64{}, Sources: map[string]int64{}}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT split_part(content_type, '/', 1), count(*), COALESCE(sum(size_bytes), 0)
		FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2 GROUP BY 1`, tenantID, versionID)
	if err != nil {
		return p, fmt.Errorf("profile modalities: %w", err)
	}
	for rows.Next() {
		var kind string
		var n, b int64
		if err := rows.Scan(&kind, &n, &b); err != nil {
			rows.Close()
			return p, err
		}
		p.Modalities[kind] = [2]int64{n, b}
	}
	rows.Close()
	if err := r.db.Pool.QueryRow(ctx, `
		SELECT min(COALESCE(a.published_at, a.created_at)), max(COALESCE(a.published_at, a.created_at))
		FROM dataset_version_members vm JOIN assets a ON a.id = vm.asset_id
		WHERE vm.tenant_id = $1 AND vm.version_id = $2`, tenantID, versionID).Scan(&p.First, &p.Last); err != nil {
		return p, err
	}
	for key, into := range map[string]map[string]int64{"license": p.Licenses, "source": p.Sources} {
		rows, err := r.db.Pool.Query(ctx, `
			SELECT metadata ->> $3, count(*) FROM dataset_version_members
			WHERE tenant_id = $1 AND version_id = $2 AND metadata ? $3 GROUP BY 1 ORDER BY 2 DESC LIMIT 20`, tenantID, versionID, key)
		if err != nil {
			return p, err
		}
		for rows.Next() {
			var v string
			var n int64
			if err := rows.Scan(&v, &n); err != nil {
				rows.Close()
				return p, err
			}
			into[v] = n
		}
		rows.Close()
	}
	return p, nil
}

func (r *EvidenceRepository) PublishSummary(ctx context.Context, tenantID, modelID string, at time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE models SET summary_published_at = $3, summary_reminded_at = NULL WHERE tenant_id = $1 AND id = $2`,
		tenantID, modelID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return model.ErrModelNotFound
	}
	return nil
}

// SummariesDue sends model.summary_due for models whose training summary
// is six months old (or was never published, a month after the model),
// once each six months, and returns how many.
func (r *EvidenceRepository) SummariesDue(ctx context.Context, now time.Time) (int, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		WITH due AS (
			UPDATE models SET summary_reminded_at = $1
			WHERE COALESCE(summary_published_at, created_at + interval '1 month' - interval '6 months') <= $1 - interval '6 months'
			  AND (summary_reminded_at IS NULL OR summary_reminded_at <= $1 - interval '6 months')
			RETURNING tenant_id, id, name, version, summary_published_at)
		SELECT queue_webhook(tenant_id, 'model.summary_due', jsonb_build_object('model_id', id, 'name', name, 'version', version,
		       'summary_published_at', summary_published_at)) FROM due`, now)
	if err != nil {
		return 0, fmt.Errorf("remind of training summaries: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ─── Control evidence ─────────────────────────────────────────────────

type MemberRow struct {
	UserID    string     `json:"user_id"`
	Email     string     `json:"email"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Status    string     `json:"status"`
	SSOOnly   bool       `json:"sso_only"`
	LastLogin *time.Time `json:"last_login_at,omitempty"`
	Since     time.Time  `json:"member_since"`
}

func (r *EvidenceRepository) Members(ctx context.Context, tenantID string) ([]MemberRow, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT u.id, u.email, u.display_name, COALESCE(NULLIF(rl.name, ''), m.role_id), m.status, u.sso_only, u.last_login_at, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		LEFT JOIN org_roles rl ON rl.id = m.role_id AND rl.org_id = m.org_id
		WHERE m.org_id = $1 ORDER BY u.email`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	var out []MemberRow
	for rows.Next() {
		var m MemberRow
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.Role, &m.Status, &m.SSOOnly, &m.LastLogin, &m.Since); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type TokenRow struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	UserID      string     `json:"user_id"`
	Permissions []string   `json:"permissions"`
	Datasets    []string   `json:"datasets"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
}

func (r *EvidenceRepository) Tokens(ctx context.Context, tenantID string, now time.Time) ([]TokenRow, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, name, user_id, permissions, datasets, expires_at, last_used_at FROM api_tokens
		WHERE tenant_id = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $2) ORDER BY created_at`, tenantID, now)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []TokenRow
	for rows.Next() {
		var t TokenRow
		if err := rows.Scan(&t.ID, &t.Name, &t.UserID, &t.Permissions, &t.Datasets, &t.ExpiresAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ControlFacts are the measurable facts control mappings rest on.
type ControlFacts struct {
	Members, SSOOnlyMembers, ActiveTokens, NonExpiringTokens int64
	SSOEnabled                                               bool
	Connections, ConnectionsFailing, ConnectionsCustomerKey  int64
	LockedObjects, HeldObjects, RetentionPolicies            int64
	LastSignedReview, LastAnchor                             *time.Time
	ScansClean, ScansTotal                                   int64
	GovernanceProfile                                        string
	TwoPerson                                                bool
}

func (r *EvidenceRepository) ControlFacts(ctx context.Context, tenantID string, now time.Time) (ControlFacts, error) {
	var f ControlFacts
	err := r.db.Pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memberships WHERE org_id = $1 AND status = 'ACTIVE'),
		  (SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 AND m.status = 'ACTIVE' AND u.sso_only),
		  (SELECT count(*) FROM api_tokens WHERE tenant_id = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $2)),
		  (SELECT count(*) FROM api_tokens WHERE tenant_id = $1 AND revoked_at IS NULL AND expires_at IS NULL),
		  COALESCE((SELECT enabled FROM sso_connections WHERE org_id = $1), false),
		  (SELECT count(*) FROM storage_connections WHERE tenant_id = $1 AND status = 'ACTIVE'),
		  (SELECT count(*) FROM connection_posture WHERE tenant_id = $1 AND worst = 'FAIL'),
		  (SELECT count(*) FROM storage_connections WHERE tenant_id = $1 AND status = 'ACTIVE' AND COALESCE(encryption_key, '') <> ''),
		  (SELECT count(*) FROM object_locks WHERE tenant_id = $1 AND (legal_hold OR retain_until > $2)),
		  (SELECT count(*) FROM legal_holds WHERE tenant_id = $1 AND released_at IS NULL),
		  (SELECT count(*) FROM retention_policies WHERE tenant_id = $1),
		  (SELECT max(signed_at) FROM audit_reviews WHERE tenant_id = $1 AND state = 'SIGNED'),
		  (SELECT max(created_at) FROM audit_anchors WHERE chain = $1),
		  (SELECT count(*) FROM assets WHERE tenant_id = $1 AND status = 'AVAILABLE'),
		  (SELECT count(*) FROM assets WHERE tenant_id = $1 AND status IN ('AVAILABLE', 'REJECTED', 'QUARANTINED')),
		  COALESCE((SELECT profile FROM tenant_governance WHERE tenant_id = $1), 'STANDARD'),
		  COALESCE((SELECT two_person FROM tenant_governance WHERE tenant_id = $1), false)
	`, tenantID, now).Scan(&f.Members, &f.SSOOnlyMembers, &f.ActiveTokens, &f.NonExpiringTokens, &f.SSOEnabled, &f.Connections,
		&f.ConnectionsFailing, &f.ConnectionsCustomerKey, &f.LockedObjects, &f.HeldObjects, &f.RetentionPolicies, &f.LastSignedReview,
		&f.LastAnchor, &f.ScansClean, &f.ScansTotal, &f.GovernanceProfile, &f.TwoPerson)
	if err != nil {
		return f, fmt.Errorf("read control facts: %w", err)
	}
	return f, nil
}

// AsOfRow is an object as it stood at a moment: its version then.
type AsOfRow struct {
	ObjectID string    `json:"object_id"`
	Path     string    `json:"path"`
	AssetID  string    `json:"version_id"`
	Version  int       `json:"version"`
	Size     int64     `json:"size_bytes"`
	SHA256   string    `json:"sha256"`
	Since    time.Time `json:"current_since"`
}

// AsOf lists a namespace's objects as they stood at at: for each, the
// newest version published by then and not deleted by then.
func (r *EvidenceRepository) AsOf(ctx context.Context, tenantID, namespace string, at time.Time, limit int) ([]AsOfRow, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT ON (o.id) o.id, o.path, a.id, COALESCE(a.version, 0), a.size_bytes, COALESCE(a.checksum_sha256, ''),
		       COALESCE(a.published_at, a.created_at)
		FROM catalog_objects o
		JOIN assets a ON a.object_id = o.id AND a.tenant_id = o.tenant_id
		WHERE o.tenant_id = $1 AND o.dataset = $2 AND o.created_at <= $3
		  AND (o.deleted_at IS NULL OR o.deleted_at > $3)
		  AND a.published_at IS NOT NULL AND a.published_at <= $3
		  AND NOT (a.status = 'DELETED' AND a.updated_at <= $3)
		ORDER BY o.id, a.version DESC
		LIMIT $4`, tenantID, namespace, at, limit)
	if err != nil {
		return nil, fmt.Errorf("read catalog as of: %w", err)
	}
	defer rows.Close()
	var out []AsOfRow
	for rows.Next() {
		var x AsOfRow
		if err := rows.Scan(&x.ObjectID, &x.Path, &x.AssetID, &x.Version, &x.Size, &x.SHA256, &x.Since); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
