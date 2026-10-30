package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ContentCheckRepository keeps what content checks found (migration
// 000029), the claims of re-scans, and answers blast-radius questions.
type ContentCheckRepository struct {
	db *DB
}

func NewContentCheckRepository(db *DB) *ContentCheckRepository {
	return &ContentCheckRepository{db: db}
}

// ReplaceFindings records a job's findings, replacing what an earlier
// attempt of the same job recorded. Re-scan findings are kept.
func (r *ContentCheckRepository) ReplaceFindings(ctx context.Context, tenantID, assetID, jobID string, findings []model.ContentFinding, at time.Time) error {
	return pgx.BeginFunc(ctx, r.db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM content_findings WHERE tenant_id = $1 AND asset_id = $2 AND job_id = $3 AND source <> 'rescan'`,
			tenantID, assetID, jobID); err != nil {
			return fmt.Errorf("clear content findings: %w", err)
		}
		for _, f := range findings {
			if err := insertFinding(ctx, tx, tenantID, assetID, jobID, f, at); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *ContentCheckRepository) AddFinding(ctx context.Context, tenantID, assetID, jobID string, f model.ContentFinding, at time.Time) error {
	return insertFinding(ctx, r.db.Pool, tenantID, assetID, jobID, f, at)
}

func insertFinding(ctx context.Context, db execer, tenantID, assetID, jobID string, f model.ContentFinding, at time.Time) error {
	_, err := db.Exec(ctx, `
		INSERT INTO content_findings (id, tenant_id, asset_id, job_id, source, code, action, detail, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		event.NewID(), tenantID, assetID, jobID, f.Source, f.Code, f.Action, f.Detail, at)
	if err != nil {
		return fmt.Errorf("record content finding: %w", err)
	}
	return nil
}

func (r *ContentCheckRepository) ListFindings(ctx context.Context, tenantID, assetID string) ([]model.ContentFinding, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, asset_id, job_id, source, code, action, detail, created_at
		FROM content_findings WHERE tenant_id = $1 AND asset_id = $2
		ORDER BY created_at, id`, tenantID, assetID)
	if err != nil {
		return nil, fmt.Errorf("list content findings: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.ContentFinding, error) {
		var f model.ContentFinding
		err := row.Scan(&f.ID, &f.AssetID, &f.JobID, &f.Source, &f.Code, &f.Action, &f.Detail, &f.CreatedAt)
		return f, err
	})
}

func (r *ContentCheckRepository) RecordCoverage(ctx context.Context, jobID, coverage string) error {
	if _, err := r.db.Pool.Exec(ctx, `UPDATE processing_jobs SET content_checks = $2 WHERE id = $1`, jobID, coverage); err != nil {
		return fmt.Errorf("record content check coverage: %w", err)
	}
	return nil
}

// ClaimStaleScans claims published assets whose CLEAN verdict came from
// other signatures than version, for one re-scan each. A claim lasts an
// hour: a re-scan that fails is tried again after it.
func (r *ContentCheckRepository) ClaimStaleScans(ctx context.Context, scanner, version string, now time.Time, limit int) ([]model.StaleScan, error) {
	rows, err := r.db.Pool.Query(ctx, `
		UPDATE processing_jobs AS pj SET rescanned_at = $3::timestamptz
		FROM (
			SELECT j.id FROM processing_jobs AS j
			JOIN assets AS a ON a.id = j.asset_id AND a.tenant_id = j.tenant_id
			WHERE j.scan_result = 'CLEAN' AND j.scanner = $1::text AND j.scanner_version IS DISTINCT FROM $2::text
			  AND a.status = 'AVAILABLE'
			  AND (j.rescanned_at IS NULL OR j.rescanned_at < $3::timestamptz - interval '1 hour')
			ORDER BY j.id
			LIMIT $4
			FOR UPDATE OF j SKIP LOCKED
		) AS c
		WHERE pj.id = c.id
		RETURNING pj.tenant_id, pj.asset_id, pj.id, COALESCE(pj.scanner_version, '')`,
		scanner, version, now, limit)
	if err != nil {
		return nil, fmt.Errorf("claim stale scans: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.StaleScan, error) {
		var s model.StaleScan
		err := row.Scan(&s.TenantID, &s.AssetID, &s.JobID, &s.OldVersion)
		return s, err
	})
}

// MarkRescanned records a re-scan's CLEAN verdict by version; later
// uploads of the same bytes may then reuse it.
func (r *ContentCheckRepository) MarkRescanned(ctx context.Context, jobID, version string, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE processing_jobs SET scanner_version = $2, scanned_at = $3, rescanned_at = $3, updated_at = $3
		WHERE id = $1 AND scan_result = 'CLEAN'`, jobID, version, at)
	if err != nil {
		return fmt.Errorf("record re-scan: %w", err)
	}
	return nil
}

// RaiseClassification makes an asset at least as restricted as class.
func (r *ContentCheckRepository) RaiseClassification(ctx context.Context, tenantID, assetID, class string) error {
	return NewAccessRepository(r.db).SetClassification(ctx, tenantID, assetID, class, true)
}

// BlockAsset stops every read of a published asset, for reason.
func (r *ContentCheckRepository) BlockAsset(ctx context.Context, tenantID, assetID, reason string, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE assets SET blocked_reason = $3, blocked_at = $4, updated_at = $4
		WHERE tenant_id = $1 AND id = $2 AND blocked_reason IS NULL`, tenantID, assetID, reason, at)
	if err != nil {
		return fmt.Errorf("block asset: %w", err)
	}
	return nil
}

// MarkRescanInfected records that a re-scan found malware. The signature
// stays in the job, as at upload; findings name only the finding.
func (r *ContentCheckRepository) MarkRescanInfected(ctx context.Context, jobID, signature string, at time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE processing_jobs SET scan_result = 'INFECTED', scan_signature = $2, scanner_version = NULL,
		       scanned_at = $3, rescanned_at = $3, updated_at = $3
		WHERE id = $1`, jobID, signature, at)
	if err != nil {
		return fmt.Errorf("record re-scan infection: %w", err)
	}
	return nil
}

// BlastRadius answers "what else did these bytes reach": every asset of
// the tenant with the same SHA-256, the dataset snapshots that pin any of
// them, and who was given a download link for any of them.
func (r *ContentCheckRepository) BlastRadius(ctx context.Context, tenantID, assetID string, limit int) (model.BlastRadius, error) {
	var out model.BlastRadius
	err := r.db.Pool.QueryRow(ctx, `SELECT COALESCE(checksum_sha256, ''), size_bytes FROM assets WHERE tenant_id = $1 AND id = $2`,
		tenantID, assetID).Scan(&out.ChecksumSHA256, &out.SizeBytes)
	if err == pgx.ErrNoRows {
		return out, model.ErrAssetNotFound
	}
	if err != nil {
		return out, fmt.Errorf("read asset: %w", err)
	}

	// The same bytes: matched by SHA-256 and size, or the asset itself when
	// it has no checksum.
	const same = `
		SELECT id FROM assets
		WHERE tenant_id = $1 AND (id = $2 OR ($3 <> '' AND checksum_sha256 = $3 AND size_bytes = $4))`
	rows, err := r.db.Pool.Query(ctx, `
		SELECT a.id, COALESCE(a.object_id, ''), COALESCE(a.version, 0), a.file_name, a.status, COALESCE(a.blocked_reason, ''), a.created_at
		FROM assets AS a WHERE a.id IN (`+same+`) AND a.tenant_id = $1
		ORDER BY a.created_at LIMIT $5`, tenantID, assetID, out.ChecksumSHA256, out.SizeBytes, limit)
	if err != nil {
		return out, fmt.Errorf("find assets with the same content: %w", err)
	}
	out.Assets, err = collect(rows, func(row pgx.Row) (model.BlastAsset, error) {
		var a model.BlastAsset
		err := row.Scan(&a.AssetID, &a.ObjectID, &a.Version, &a.FileName, &a.Status, &a.BlockedReason, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		return out, err
	}

	rows, err = r.db.Pool.Query(ctx, `
		SELECT d.id, d.name, v.version, count(*)
		FROM dataset_version_members AS vm
		JOIN dataset_versions AS v ON v.id = vm.version_id AND v.tenant_id = vm.tenant_id
		JOIN datasets AS d ON d.id = v.dataset_id AND d.tenant_id = v.tenant_id
		WHERE vm.tenant_id = $1 AND vm.asset_id IN (`+same+`)
		GROUP BY d.id, d.name, v.version
		ORDER BY d.name, v.version LIMIT $5`, tenantID, assetID, out.ChecksumSHA256, out.SizeBytes, limit)
	if err != nil {
		return out, fmt.Errorf("find snapshots with the content: %w", err)
	}
	out.Snapshots, err = collect(rows, func(row pgx.Row) (model.BlastSnapshot, error) {
		var s model.BlastSnapshot
		err := row.Scan(&s.DatasetID, &s.DatasetName, &s.Version, &s.Members)
		return s, err
	})
	if err != nil {
		return out, err
	}

	rows, err = r.db.Pool.Query(ctx, `
		SELECT actor_type, actor_id, count(*), min(created_at), max(created_at)
		FROM audit_events
		WHERE tenant_id = $1 AND resource_type = 'asset' AND action = 'asset.download_url' AND outcome = 'SUCCESS'
		  AND resource_id IN (`+same+`)
		GROUP BY actor_type, actor_id
		ORDER BY max(created_at) DESC LIMIT $5`, tenantID, assetID, out.ChecksumSHA256, out.SizeBytes, limit)
	if err != nil {
		return out, fmt.Errorf("find downloads of the content: %w", err)
	}
	out.Downloads, err = collect(rows, func(row pgx.Row) (model.BlastDownload, error) {
		var d model.BlastDownload
		err := row.Scan(&d.ActorType, &d.ActorID, &d.Links, &d.First, &d.Last)
		return d, err
	})
	return out, err
}

// GetPolicy returns a tenant's own content policy; false when it has none.
func (r *ContentCheckRepository) GetPolicy(ctx context.Context, tenantID string) (model.ContentPolicy, bool, error) {
	return NewContentPolicyRepository(r.db).Get(ctx, tenantID)
}

// PutPolicy replaces a tenant's own content policy.
func (r *ContentCheckRepository) PutPolicy(ctx context.Context, tenantID string, p model.ContentPolicy, at time.Time) error {
	actions, err := json.Marshal(p.FindingActions)
	if err != nil {
		return err
	}
	var maxSize *int64
	if p.MaxSizeBytes > 0 {
		maxSize = &p.MaxSizeBytes
	}
	var unknown *string
	if p.UnknownTypeAction != "" {
		unknown = &p.UnknownTypeAction
	}
	_, err = r.db.Pool.Exec(ctx, `
		INSERT INTO tenant_content_policies (tenant_id, allowed_types, denied_types, max_size_bytes, unknown_type_action, finding_actions, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id) DO UPDATE SET
			allowed_types = EXCLUDED.allowed_types, denied_types = EXCLUDED.denied_types,
			max_size_bytes = EXCLUDED.max_size_bytes, unknown_type_action = EXCLUDED.unknown_type_action,
			finding_actions = EXCLUDED.finding_actions, updated_at = EXCLUDED.updated_at`,
		tenantID, nonNil(p.AllowedTypes), nonNil(p.DeniedTypes), maxSize, unknown, actions, at)
	if err != nil {
		return fmt.Errorf("save content policy: %w", err)
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
