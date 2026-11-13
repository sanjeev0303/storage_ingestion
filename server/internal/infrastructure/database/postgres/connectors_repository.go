package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ConnectorsRepository keeps chat integrations and shares, Git LFS locks,
// imported collections and the DICOM index (docs/connectors.md).
type ConnectorsRepository struct {
	db *DB
}

func NewConnectorsRepository(db *DB) *ConnectorsRepository {
	return &ConnectorsRepository{db: db}
}

// ─── Chat ─────────────────────────────────────────────────────────────

const chatIntegrationColumns = `id, tenant_id, kind, name, url_host, url_key_version, url_nonce, url_cipher, created_by, created_at`

func scanChatIntegration(row pgx.Row) (model.ChatIntegration, error) {
	var c model.ChatIntegration
	err := row.Scan(&c.ID, &c.TenantID, &c.Kind, &c.Name, &c.URLHost, &c.KeyVersion, &c.Nonce, &c.Cipher, &c.CreatedBy, &c.CreatedAt)
	return c, err
}

func (r *ConnectorsRepository) AddIntegration(ctx context.Context, c model.ChatIntegration) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO chat_integrations (`+chatIntegrationColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		c.ID, c.TenantID, c.Kind, c.Name, c.URLHost, c.KeyVersion, c.Nonce, c.Cipher, c.CreatedBy, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("add chat integration: %w", err)
	}
	return nil
}

func (r *ConnectorsRepository) Integrations(ctx context.Context, tenantID string) ([]model.ChatIntegration, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+chatIntegrationColumns+` FROM chat_integrations WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list chat integrations: %w", err)
	}
	defer rows.Close()
	out := []model.ChatIntegration{}
	for rows.Next() {
		c, err := scanChatIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *ConnectorsRepository) Integration(ctx context.Context, tenantID, id string) (model.ChatIntegration, error) {
	c, err := scanChatIntegration(r.db.Pool.QueryRow(ctx, `SELECT `+chatIntegrationColumns+` FROM chat_integrations WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, model.ErrChatNotFound
	}
	return c, err
}

func (r *ConnectorsRepository) DeleteIntegration(ctx context.Context, tenantID, id string) error {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM chat_integrations WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete chat integration: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrChatNotFound
	}
	return nil
}

const chatShareColumns = `s.id, s.tenant_id, s.asset_id, s.integration_id, s.note, s.expires_at, s.revoked_at, s.opens, s.created_by, s.created_at,
	COALESCE(o.path, a.file_name), a.size_bytes`

const chatShareFrom = ` FROM chat_shares s JOIN assets a ON a.tenant_id = s.tenant_id AND a.id = s.asset_id
	LEFT JOIN catalog_objects o ON o.tenant_id = a.tenant_id AND o.id = a.object_id`

func scanChatShare(row pgx.Row) (model.ChatShare, error) {
	var c model.ChatShare
	err := row.Scan(&c.ID, &c.TenantID, &c.AssetID, &c.IntegrationID, &c.Note, &c.ExpiresAt, &c.RevokedAt, &c.Opens, &c.CreatedBy, &c.CreatedAt, &c.Path, &c.SizeBytes)
	return c, err
}

func (r *ConnectorsRepository) AddShare(ctx context.Context, c model.ChatShare) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO chat_shares (id, tenant_id, asset_id, integration_id, note, expires_at, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, c.ID, c.TenantID, c.AssetID, c.IntegrationID, c.Note, c.ExpiresAt, c.CreatedBy, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("add chat share: %w", err)
	}
	return nil
}

func (r *ConnectorsRepository) RemoveShare(ctx context.Context, tenantID, id string) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM chat_shares WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return err
}

func (r *ConnectorsRepository) Share(ctx context.Context, tenantID, id string) (model.ChatShare, error) {
	c, err := scanChatShare(r.db.Pool.QueryRow(ctx, `SELECT `+chatShareColumns+chatShareFrom+` WHERE s.tenant_id = $1 AND s.id = $2`, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, model.ErrChatNotFound
	}
	return c, err
}

func (r *ConnectorsRepository) Shares(ctx context.Context, tenantID string, limit int) ([]model.ChatShare, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+chatShareColumns+chatShareFrom+` WHERE s.tenant_id = $1 ORDER BY s.created_at DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list chat shares: %w", err)
	}
	defer rows.Close()
	out := []model.ChatShare{}
	for rows.Next() {
		c, err := scanChatShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *ConnectorsRepository) CountOpen(ctx context.Context, tenantID, id string) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE chat_shares SET opens = opens + 1 WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	return err
}

func (r *ConnectorsRepository) RevokeShare(ctx context.Context, c model.ChatShare) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE chat_shares SET revoked_at = $3 WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`, c.TenantID, c.ID, c.RevokedAt)
	if err != nil {
		return fmt.Errorf("revoke chat share: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrChatNotFound
	}
	return nil
}

// AssetFacts reads what a share or a Git LFS answer needs of an asset.
func (r *ConnectorsRepository) AssetFacts(ctx context.Context, tenantID, assetID string) (dataset, path, status string, err error) {
	err = r.db.Pool.QueryRow(ctx, `SELECT COALESCE(a.dataset, ''), COALESCE(o.path, a.file_name), `+assetUsableStatus+` FROM assets a
		LEFT JOIN catalog_objects o ON o.tenant_id = a.tenant_id AND o.id = a.object_id
		WHERE a.tenant_id = $1 AND a.id = $2`, tenantID, assetID).Scan(&dataset, &path, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", model.ErrChatNotFound
	}
	return dataset, path, status, err
}

// ─── Git LFS ──────────────────────────────────────────────────────────

// AssetByHash finds the newest live asset of a namespace with these bytes.
func (r *ConnectorsRepository) AssetByHash(ctx context.Context, tenantID, dataset, sha256 string) (id, status string, size int64, found bool, err error) {
	err = r.db.Pool.QueryRow(ctx, `SELECT a.id, `+assetUsableStatus+`, a.size_bytes FROM assets a
		WHERE a.tenant_id = $1 AND a.dataset = $2 AND a.checksum_sha256 = $3 AND a.status <> 'DELETED'
		ORDER BY (a.status = 'AVAILABLE' AND a.blocked_reason IS NULL) DESC, a.created_at DESC LIMIT 1`, tenantID, dataset, sha256).Scan(&id, &status, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", 0, false, nil
	}
	if err != nil {
		return "", "", 0, false, fmt.Errorf("find object by hash: %w", err)
	}
	return id, status, size, true, nil
}

const lfsLockColumns = `id, tenant_id, dataset, path, owner_id, locked_at`

func scanLock(row pgx.Row) (model.LFSLock, error) {
	var l model.LFSLock
	err := row.Scan(&l.ID, &l.TenantID, &l.Dataset, &l.Path, &l.OwnerID, &l.LockedAt)
	return l, err
}

// AddLock takes a path's lock; a path already locked answers its lock and
// taken false.
func (r *ConnectorsRepository) AddLock(ctx context.Context, l model.LFSLock) (model.LFSLock, bool, error) {
	got, err := scanLock(r.db.Pool.QueryRow(ctx, `INSERT INTO lfs_locks (`+lfsLockColumns+`) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, dataset, path) DO NOTHING RETURNING `+lfsLockColumns, l.ID, l.TenantID, l.Dataset, l.Path, l.OwnerID, l.LockedAt))
	if err == nil {
		return got, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return got, false, fmt.Errorf("take lock: %w", err)
	}
	got, err = scanLock(r.db.Pool.QueryRow(ctx, `SELECT `+lfsLockColumns+` FROM lfs_locks WHERE tenant_id = $1 AND dataset = $2 AND path = $3`, l.TenantID, l.Dataset, l.Path))
	return got, false, err
}

func (r *ConnectorsRepository) Locks(ctx context.Context, tenantID, dataset, path, id string, limit int) ([]model.LFSLock, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+lfsLockColumns+` FROM lfs_locks WHERE tenant_id = $1 AND dataset = $2
		AND ($3 = '' OR path = $3) AND ($4 = '' OR id = $4) ORDER BY path LIMIT $5`, tenantID, dataset, path, id, limit)
	if err != nil {
		return nil, fmt.Errorf("list locks: %w", err)
	}
	defer rows.Close()
	out := []model.LFSLock{}
	for rows.Next() {
		l, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *ConnectorsRepository) RemoveLock(ctx context.Context, tenantID, dataset, id string) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM lfs_locks WHERE tenant_id = $1 AND dataset = $2 AND id = $3`, tenantID, dataset, id)
	return err
}

// ─── Collections ──────────────────────────────────────────────────────

func (r *ConnectorsRepository) AddCollection(ctx context.Context, c model.CollectionImport) error {
	mismatches, err := json.Marshal(c.Mismatches)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `INSERT INTO collection_imports (id, tenant_id, dataset_id, format, source, export_sha256, items, verified,
		mismatches, retention, created_by, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		c.ID, c.TenantID, c.DatasetID, c.Format, c.Source, c.ExportSHA256, c.Items, c.Verified, mismatches, c.Retention, c.CreatedBy, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("record collection: %w", err)
	}
	return nil
}

func (r *ConnectorsRepository) Collections(ctx context.Context, tenantID, datasetID string, limit int) ([]model.CollectionImport, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT id, tenant_id, dataset_id, format, source, export_sha256, items, verified, mismatches, retention,
		created_by, created_at FROM collection_imports WHERE tenant_id = $1 AND dataset_id = $2 ORDER BY created_at DESC LIMIT $3`, tenantID, datasetID, limit)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()
	out := []model.CollectionImport{}
	for rows.Next() {
		var c model.CollectionImport
		var mismatches []byte
		if err := rows.Scan(&c.ID, &c.TenantID, &c.DatasetID, &c.Format, &c.Source, &c.ExportSHA256, &c.Items, &c.Verified, &mismatches,
			&c.Retention, &c.CreatedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(mismatches, &c.Mismatches); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ─── DICOM ────────────────────────────────────────────────────────────

// PutDicom records an instance of a version from its header.
func (r *ConnectorsRepository) PutDicom(ctx context.Context, tenantID, versionID, assetID, path string, in model.DicomInstance) error {
	tags, err := json.Marshal(in.Tags)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `INSERT INTO dicom_instances (version_id, tenant_id, sop_uid, study_uid, series_uid, modality, patient_id,
		study_date, asset_id, path, tags) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (version_id, sop_uid) DO UPDATE SET asset_id = EXCLUDED.asset_id, path = EXCLUDED.path, tags = EXCLUDED.tags`,
		versionID, tenantID, in.SOPUID, in.StudyUID, in.SeriesUID, in.Modality, in.PatientID, in.StudyDate, assetID, path, tags)
	if err != nil {
		return fmt.Errorf("index DICOM instance: %w", err)
	}
	return nil
}

// DicomInstances lists a version's instances matching what is set, in
// study, series, instance order, skipping those whose bytes are gone.
func (r *ConnectorsRepository) DicomInstances(ctx context.Context, tenantID, versionID string, f model.DicomFilter) ([]model.DicomInstance, error) {
	where := []string{"d.tenant_id = $1", "d.version_id = $2", "a.status = 'AVAILABLE'", "a.blocked_reason IS NULL"}
	args := []any{tenantID, versionID}
	add := func(column, value string) {
		if value != "" {
			args = append(args, value)
			where = append(where, fmt.Sprintf("d.%s = $%d", column, len(args)))
		}
	}
	add("study_uid", f.Study)
	add("series_uid", f.Series)
	add("sop_uid", f.Instance)
	add("patient_id", f.PatientID)
	add("modality", f.Modality)
	add("study_date", f.StudyDate)
	rows, err := r.db.Pool.Query(ctx, `SELECT d.sop_uid, d.study_uid, d.series_uid, d.modality, d.patient_id, d.study_date, d.asset_id, d.path, d.tags
		FROM dicom_instances d JOIN assets a ON a.tenant_id = d.tenant_id AND a.id = d.asset_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY d.study_uid, d.series_uid, d.sop_uid LIMIT 100000`, args...)
	if err != nil {
		return nil, fmt.Errorf("read DICOM index: %w", err)
	}
	defer rows.Close()
	out := []model.DicomInstance{}
	for rows.Next() {
		var in model.DicomInstance
		var tags []byte
		if err := rows.Scan(&in.SOPUID, &in.StudyUID, &in.SeriesUID, &in.Modality, &in.PatientID, &in.StudyDate, &in.AssetID, &in.Path, &tags); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(tags, &in.Tags); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
