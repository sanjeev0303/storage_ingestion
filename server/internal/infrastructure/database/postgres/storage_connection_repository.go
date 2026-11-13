package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// StorageConnectionRepository stores storage connections (Phase 11). A
// tenant sees the platform connection and its own; another tenant's
// connection reads as not found. Credentials are written and read only as
// ciphertext, and only by the methods that name them.
type StorageConnectionRepository struct {
	db *DB
}

func NewStorageConnectionRepository(db *DB) *StorageConnectionRepository {
	return &StorageConnectionRepository{db: db}
}

var _ interfaces.StorageConnectionRepository = (*StorageConnectionRepository)(nil)

const connectionColumns = `id, coalesce(tenant_id, ''), name, provider, status, auth_method, bucket, region, endpoint,
	account_id, force_path_style, role_arn, external_id, staging_prefix, ready_prefix, staging_mode,
	credential_version, coalesce(credential_key_version, 0), credential_rotated_at, capabilities,
	capabilities_checked_at, last_tested_at, last_test_ok_at, coalesce(last_error_category, ''), revision,
	created_by, coalesce(updated_by, ''), created_at, updated_at, disabled_at, deleted_at,
	checksum_mode, coalesce(expected_owner, ''), coalesce(staged_credential_version, 0), staged_at, coalesce(staged_by, ''),
	coalesce(previous_credential_version, 0), previous_until, coalesce(encryption_key, '')`

// visible limits a query to the platform connection and the tenant's own
// connections that are not deleted. $1 is the tenant.
const visible = `(tenant_id = $1 OR id = 'platform') AND status <> 'DELETED'`

func scanConnection(row pgx.Row) (model.StorageConnection, error) {
	var c model.StorageConnection
	var capabilities []byte
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.Provider, &c.Status, &c.AuthMethod, &c.Bucket, &c.Region, &c.Endpoint,
		&c.AccountID, &c.ForcePathStyle, &c.RoleARN, &c.ExternalID, &c.StagingPrefix, &c.ReadyPrefix, &c.StagingMode,
		&c.CredentialVersion, &c.CredentialKeyVersion, &c.CredentialRotatedAt, &capabilities,
		&c.CapabilitiesCheckedAt, &c.LastTestedAt, &c.LastTestOKAt, &c.LastErrorCategory, &c.Revision,
		&c.CreatedBy, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt, &c.DisabledAt, &c.DeletedAt,
		&c.ChecksumMode, &c.ExpectedOwner, &c.StagedCredentialVersion, &c.StagedAt, &c.StagedBy,
		&c.PreviousCredentialVersion, &c.PreviousUntil, &c.EncryptionKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.StorageConnection{}, model.ErrConnectionNotFound
	}
	if err != nil {
		return model.StorageConnection{}, err
	}
	if capabilities != nil {
		c.Capabilities = &model.ProviderCapabilities{}
		if err := json.Unmarshal(capabilities, c.Capabilities); err != nil {
			return model.StorageConnection{}, fmt.Errorf("read connection capabilities: %w", err)
		}
	}
	return c, nil
}

// bindingError maps the storage-binding trigger's refusals.
func bindingError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.ConstraintName {
	case "storage_connection_tenant":
		return model.ErrConnectionNotFound
	case "storage_connection_active":
		return model.ErrConnectionNotActive
	}
	return err
}

func (r *StorageConnectionRepository) CreateConnection(ctx context.Context, c model.StorageConnection, sealed interfaces.SealedCredential) (model.StorageConnection, error) {
	var keyVersion *int
	var nonce, ciphertext, wrappedKey []byte
	if sealed.Version > 0 {
		keyVersion, nonce, ciphertext, wrappedKey = &sealed.KeyVersion, sealed.Nonce, sealed.Ciphertext, sealed.WrappedKey
	}
	stored, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		INSERT INTO storage_connections (id, tenant_id, name, provider, status, auth_method, bucket, region, endpoint,
		    account_id, force_path_style, role_arn, external_id, staging_prefix, ready_prefix, staging_mode,
		    credential_version, credential_key_version, credential_nonce, credential_ciphertext, credential_rotated_at,
		    created_by, updated_by, created_at, updated_at, credential_wrapped_key)
		VALUES ($1, $2, $3, $4, 'PENDING', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		    $16, $17, $18, $19, CASE WHEN $16 > 0 THEN $21::timestamptz END, $20, $20, $21, $21, $22)
		RETURNING `+connectionColumns,
		c.ID, c.TenantID, c.Name, c.Provider, c.AuthMethod, c.Bucket, c.Region, c.Endpoint,
		c.AccountID, c.ForcePathStyle, c.RoleARN, c.ExternalID, c.StagingPrefix, c.ReadyPrefix, c.StagingMode,
		sealed.Version, keyVersion, nonce, ciphertext, c.CreatedBy, c.CreatedAt, wrappedKey))
	if isUniqueViolation(err) {
		return model.StorageConnection{}, model.ErrConnectionExists
	}
	if err != nil {
		return model.StorageConnection{}, fmt.Errorf("create storage connection: %w", err)
	}
	return stored, nil
}

func (r *StorageConnectionRepository) GetConnection(ctx context.Context, tenantID, connectionID string) (model.StorageConnection, error) {
	c, err := scanConnection(r.db.Pool.QueryRow(ctx,
		`SELECT `+connectionColumns+` FROM storage_connections WHERE `+visible+` AND id = $2`, tenantID, connectionID))
	if err != nil && !errors.Is(err, model.ErrConnectionNotFound) {
		return c, fmt.Errorf("get storage connection: %w", err)
	}
	return c, err
}

func (r *StorageConnectionRepository) ListConnections(ctx context.Context, tenantID string) ([]model.StorageConnection, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+connectionColumns+` FROM storage_connections WHERE `+visible+`
		ORDER BY tenant_id NULLS FIRST, created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list storage connections: %w", err)
	}
	defer rows.Close()
	var out []model.StorageConnection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// missed explains why a change to the tenant's connection matched no row:
// there is no such connection, or its revision moved on.
func (r *StorageConnectionRepository) missed(ctx context.Context, tenantID, connectionID string) error {
	var exists bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT true FROM storage_connections WHERE tenant_id = $1 AND id = $2 AND status <> 'DELETED'`,
		tenantID, connectionID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrConnectionNotFound
	}
	if err != nil {
		return err
	}
	return model.ErrRevisionConflict
}

// UpdateConnection changes non-secret settings at the given revision.
func (r *StorageConnectionRepository) UpdateConnection(ctx context.Context, tenantID, connectionID string, revision int64, change interfaces.ConnectionChange) (model.StorageConnection, error) {
	c, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_connections SET
		    name = coalesce($4, name),
		    region = coalesce($5, region),
		    endpoint = coalesce($6, endpoint),
		    force_path_style = coalesce($7, force_path_style),
		    staging_mode = coalesce($8, staging_mode),
		    checksum_mode = coalesce($11, checksum_mode),
		    expected_owner = CASE WHEN $12::text IS NULL THEN expected_owner ELSE NULLIF($12::text, '') END,
		    encryption_key = CASE WHEN $13::text IS NULL THEN encryption_key ELSE NULLIF($13::text, '') END,
		    revision = revision + 1, updated_by = $9, updated_at = $10
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND status <> 'DELETED'
		RETURNING `+connectionColumns,
		tenantID, connectionID, revision, change.Name, change.Region, change.Endpoint, change.ForcePathStyle,
		change.StagingMode, change.Actor, change.At, change.ChecksumMode, change.ExpectedOwner, change.EncryptionKey))
	if errors.Is(err, model.ErrConnectionNotFound) {
		return c, r.missed(ctx, tenantID, connectionID)
	}
	if isUniqueViolation(err) {
		return c, model.ErrConnectionExists
	}
	if err != nil {
		return c, fmt.Errorf("update storage connection: %w", err)
	}
	return c, nil
}

// RotateCredential replaces the current credential outright. A staged
// credential and the rollback slot predate it, so both are erased: a later
// promotion or rollback would otherwise bring back what this replaced.
func (r *StorageConnectionRepository) RotateCredential(ctx context.Context, tenantID, connectionID string, revision int64, sealed interfaces.SealedCredential, actor string, at time.Time) (model.StorageConnection, error) {
	c, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_connections SET
		    credential_version = $4, credential_key_version = $5, credential_nonce = $6, credential_ciphertext = $7,
		    credential_wrapped_key = $10, credential_rotated_at = $9,
		    staged_credential_version = NULL, staged_key_version = NULL, staged_nonce = NULL, staged_ciphertext = NULL,
		    staged_wrapped_key = NULL, staged_at = NULL, staged_by = NULL,
		    previous_credential_version = NULL, previous_key_version = NULL, previous_nonce = NULL,
		    previous_ciphertext = NULL, previous_wrapped_key = NULL, previous_until = NULL,
		    revision = revision + 1, updated_by = $8, updated_at = $9
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND status <> 'DELETED' AND credential_version < $4
		RETURNING `+connectionColumns,
		tenantID, connectionID, revision, sealed.Version, sealed.KeyVersion, sealed.Nonce, sealed.Ciphertext, actor, at, sealed.WrappedKey))
	if errors.Is(err, model.ErrConnectionNotFound) {
		return c, r.missed(ctx, tenantID, connectionID)
	}
	if err != nil {
		return c, fmt.Errorf("rotate storage credential: %w", err)
	}
	return c, nil
}

func (r *StorageConnectionRepository) Credential(ctx context.Context, tenantID, connectionID string, version int) (interfaces.SealedCredential, error) {
	sealed := interfaces.SealedCredential{Version: version}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT credential_key_version, credential_nonce, credential_ciphertext, credential_wrapped_key FROM storage_connections
		WHERE tenant_id = $1 AND id = $2 AND credential_version = $3 AND credential_ciphertext IS NOT NULL
		  AND status <> 'DELETED'`,
		tenantID, connectionID, version).Scan(&sealed.KeyVersion, &sealed.Nonce, &sealed.Ciphertext, &sealed.WrappedKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return sealed, model.ErrCredentialMissing
	}
	if err != nil {
		return sealed, fmt.Errorf("read storage credential: %w", err)
	}
	return sealed, nil
}

// RecordTest stores a test's sanitized outcome. A passing test activates
// a pending connection; a failing one leaves the status alone. A test of a
// credential replaced since changes nothing: model.ErrCredentialChanged.
func (r *StorageConnectionRepository) RecordTest(ctx context.Context, tenantID, connectionID string, result interfaces.ConnectionTestRecord) (model.StorageConnection, error) {
	var capabilities []byte
	if result.Capabilities != nil {
		var err error
		if capabilities, err = json.Marshal(result.Capabilities); err != nil {
			return model.StorageConnection{}, err
		}
	}
	c, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_connections SET
		    last_tested_at = $4,
		    last_test_ok_at = CASE WHEN $3 THEN $4 ELSE last_test_ok_at END,
		    last_error_category = CASE WHEN $3 THEN NULL ELSE $5 END,
		    capabilities = coalesce($6, capabilities),
		    capabilities_checked_at = CASE WHEN $6::jsonb IS NULL THEN capabilities_checked_at ELSE $4 END,
		    status = CASE WHEN $3 AND status = 'PENDING' THEN 'ACTIVE' ELSE status END
		WHERE tenant_id = $1 AND id = $2 AND status <> 'DELETED' AND credential_version = $7
		RETURNING `+connectionColumns,
		tenantID, connectionID, result.OK, result.At, result.ErrorCategory, capabilities, result.CredentialVersion))
	if errors.Is(err, model.ErrConnectionNotFound) {
		if missing := r.missed(ctx, tenantID, connectionID); !errors.Is(missing, model.ErrConnectionNotFound) {
			return c, model.ErrCredentialChanged
		}
	}
	if err != nil && !errors.Is(err, model.ErrConnectionNotFound) {
		return c, fmt.Errorf("record connection test: %w", err)
	}
	return c, err
}

func (r *StorageConnectionRepository) SetStatus(ctx context.Context, tenantID, connectionID string, from []model.ConnectionStatus, to model.ConnectionStatus, actor string, at time.Time) (model.StorageConnection, error) {
	states := make([]string, len(from))
	for i, s := range from {
		states[i] = string(s)
	}
	c, err := scanConnection(r.db.Pool.QueryRow(ctx, `
		UPDATE storage_connections SET status = $4,
		    disabled_at = CASE WHEN $4 = 'DISABLED' THEN $6::timestamptz END,
		    revision = revision + 1, updated_by = $5, updated_at = $6
		WHERE tenant_id = $1 AND id = $2 AND status = ANY($3)
		RETURNING `+connectionColumns,
		tenantID, connectionID, states, to, actor, at))
	if errors.Is(err, model.ErrConnectionNotFound) {
		if err := r.missed(ctx, tenantID, connectionID); errors.Is(err, model.ErrConnectionNotFound) {
			return c, err
		}
		return c, model.ErrConnectionNotActive
	}
	if err != nil {
		return c, fmt.Errorf("set storage connection status: %w", err)
	}
	return c, nil
}

// A completed upload is still work until its asset exists: deleting its
// connection first would leave nothing to process it with.
const dependentsQuery = `
	SELECT
	    (SELECT count(*) FROM upload_sessions AS s WHERE s.tenant_id = $1
	        AND (s.storage_connection_id = $2 OR s.staging_connection_id = $2)
	        AND (s.status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING')
	             OR (s.status IN ('COMPLETED', 'VERIFIED')
	                 AND NOT EXISTS (SELECT 1 FROM assets AS a WHERE a.upload_id = s.id)))),
	    (SELECT count(*) FROM assets WHERE tenant_id = $1
	        AND (storage_connection_id = $2 OR staging_connection_id = $2) AND status <> 'DELETED'),
	    (SELECT count(*) FROM datasets WHERE tenant_id = $1 AND storage_connection_id = $2),
	    coalesce((SELECT default_storage_connection_id = $2 FROM organizations WHERE id = $1), false)`

func (r *StorageConnectionRepository) Dependents(ctx context.Context, tenantID, connectionID string) (interfaces.ConnectionDependents, error) {
	return dependents(ctx, r.db.Pool, tenantID, connectionID)
}

// HeldObject finds one object the connection holds. A packed asset's own
// object is retired once its pack is published, so it is skipped; when
// every asset is packed, a pack is the object held.
func (r *StorageConnectionRepository) HeldObject(ctx context.Context, tenantID, connectionID string) (string, error) {
	var key string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT a.object_key FROM assets AS a
		WHERE a.tenant_id = $1 AND a.storage_connection_id = $2 AND a.status = 'AVAILABLE'
		  AND NOT EXISTS (SELECT 1 FROM pack_entries AS e JOIN packs AS p ON p.id = e.pack_id
		                  WHERE e.tenant_id = a.tenant_id AND e.asset_id = a.id AND e.state = 'PUBLISHED' AND p.state = 'PUBLISHED')
		LIMIT 1`, tenantID, connectionID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		err = r.db.Pool.QueryRow(ctx, `
			SELECT object_key FROM packs WHERE tenant_id = $1 AND storage_connection_id = $2 AND state = 'PUBLISHED' LIMIT 1`,
			tenantID, connectionID).Scan(&key)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find an object the connection holds: %w", err)
	}
	return key, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func dependents(ctx context.Context, q queryRower, tenantID, connectionID string) (interfaces.ConnectionDependents, error) {
	var d interfaces.ConnectionDependents
	err := q.QueryRow(ctx, dependentsQuery, tenantID, connectionID).Scan(&d.ActiveUploads, &d.LiveAssets, &d.Datasets, &d.IsDefault)
	if err != nil {
		return d, fmt.Errorf("count connection dependents: %w", err)
	}
	return d, nil
}

// Delete tombstones a disabled or pending connection and erases its
// credentials, if nothing depends on it. The row lock, held to commit,
// keeps the check and the delete together; an active connection must be
// disabled first, so no new upload can bind to it meanwhile.
func (r *StorageConnectionRepository) Delete(ctx context.Context, tenantID, connectionID, actor string, at time.Time) (model.StorageConnection, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.StorageConnection{}, fmt.Errorf("begin delete connection: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var status model.ConnectionStatus
	err = tx.QueryRow(ctx, `
		SELECT status FROM storage_connections WHERE tenant_id = $1 AND id = $2 AND status <> 'DELETED' FOR UPDATE`,
		tenantID, connectionID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.StorageConnection{}, model.ErrConnectionNotFound
	}
	if err != nil {
		return model.StorageConnection{}, fmt.Errorf("lock connection: %w", err)
	}
	if status == model.ConnectionActive {
		return model.StorageConnection{}, fmt.Errorf("%w: disable it before deleting it", model.ErrConnectionInUse)
	}
	d, err := dependents(ctx, tx, tenantID, connectionID)
	if err != nil {
		return model.StorageConnection{}, err
	}
	if d.Any() {
		return model.StorageConnection{}, model.ErrConnectionInUse
	}
	c, err := scanConnection(tx.QueryRow(ctx, `
		UPDATE storage_connections SET status = 'DELETED', deleted_at = $4, updated_by = $3, updated_at = $4,
		    credential_version = 0, credential_key_version = NULL, credential_nonce = NULL, credential_ciphertext = NULL,
		    credential_wrapped_key = NULL,
		    staged_credential_version = NULL, staged_key_version = NULL, staged_nonce = NULL, staged_ciphertext = NULL,
		    staged_wrapped_key = NULL, staged_at = NULL, staged_by = NULL,
		    previous_credential_version = NULL, previous_key_version = NULL, previous_nonce = NULL,
		    previous_ciphertext = NULL, previous_wrapped_key = NULL, previous_until = NULL,
		    revision = revision + 1
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+connectionColumns, tenantID, connectionID, actor, at))
	if err != nil {
		return c, fmt.Errorf("delete storage connection: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return c, fmt.Errorf("commit delete connection: %w", err)
	}
	return c, nil
}

func (r *StorageConnectionRepository) DefaultConnection(ctx context.Context, tenantID string) (string, error) {
	var id string
	err := r.db.Pool.QueryRow(ctx, `SELECT default_storage_connection_id FROM organizations WHERE id = $1`, tenantID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.PlatformConnectionID, nil
	}
	if err != nil {
		return "", fmt.Errorf("read default connection: %w", err)
	}
	return id, nil
}

// Disable locks the organization's row first, the row SetDefault updates:
// a SetDefault that commits first is seen here as the default, and one
// that waits fails the binding trigger's ACTIVE check once this commits.
func (r *StorageConnectionRepository) Disable(ctx context.Context, tenantID, connectionID, actor string, at time.Time) (model.StorageConnection, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.StorageConnection{}, fmt.Errorf("begin disable connection: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var defaultID string
	err = tx.QueryRow(ctx, `SELECT default_storage_connection_id FROM organizations WHERE id = $1 FOR UPDATE`, tenantID).Scan(&defaultID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return model.StorageConnection{}, fmt.Errorf("lock organization: %w", err)
	}
	if defaultID == connectionID {
		return model.StorageConnection{}, fmt.Errorf("%w: it is the organization's default; set another default first", model.ErrConnectionInUse)
	}
	c, err := scanConnection(tx.QueryRow(ctx, `
		UPDATE storage_connections SET status = 'DISABLED', disabled_at = $3,
		    revision = revision + 1, updated_by = $4, updated_at = $3
		WHERE tenant_id = $1 AND id = $2 AND status IN ('ACTIVE', 'PENDING')
		RETURNING `+connectionColumns, tenantID, connectionID, at, actor))
	if errors.Is(err, model.ErrConnectionNotFound) {
		if err := r.missed(ctx, tenantID, connectionID); errors.Is(err, model.ErrConnectionNotFound) {
			return c, err
		}
		return c, model.ErrConnectionNotActive
	}
	if err != nil {
		return c, fmt.Errorf("disable storage connection: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return c, fmt.Errorf("commit disable connection: %w", err)
	}
	return c, nil
}

// SetDefault makes the connection the organization's default. The binding
// trigger refuses another tenant's connection and one that is not active.
func (r *StorageConnectionRepository) SetDefault(ctx context.Context, tenantID, connectionID string) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE organizations SET default_storage_connection_id = $2 WHERE id = $1`, tenantID, connectionID)
	if err != nil {
		return bindingError(err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrTenantRequired
	}
	return nil
}

func (r *StorageConnectionRepository) DatasetConnection(ctx context.Context, tenantID, dataset string) (string, error) {
	var id string
	err := r.db.Pool.QueryRow(ctx, `
		SELECT coalesce(storage_connection_id, '') FROM datasets WHERE tenant_id = $1 AND name = $2`, tenantID, dataset).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read dataset connection: %w", err)
	}
	return id, nil
}

var _ interfaces.ConnectionLister = (*StorageConnectionRepository)(nil)

func (r *StorageConnectionRepository) ConnectionsHoldingWork(ctx context.Context, tenantID string) ([]model.StorageConnection, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+connectionColumns+` FROM storage_connections
		WHERE tenant_id IS NOT NULL AND status IN ('ACTIVE', 'DISABLED') AND ($1 = '' OR tenant_id = $1)
		ORDER BY tenant_id, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list connections holding work: %w", err)
	}
	defer rows.Close()
	var out []model.StorageConnection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SealedCredentials are one connection's stored credentials, for a re-seal
// after a master-key rotation: the current one and any staged or previous
// one (Version 0 when there is none).
type SealedCredentials struct {
	Connection model.StorageConnection // ID, TenantID and Provider only: what the associated data needs
	Current    interfaces.SealedCredential
	Staged     interfaces.SealedCredential
	Previous   interfaces.SealedCredential
}

// ResealBatch locks up to limit live connections after the ID after whose
// credentials are not all in an envelope under keyVersion, passes each to
// reseal, and writes back what reseal leaves in it, in one transaction. A
// connection reseal fails for is left as it was.
// Without write the transaction is rolled back: a dry run. It returns the
// last ID it looked at, to continue after, and "" when none was left.
//
// The revision is not bumped: the secrets themselves do not change, so
// clients built from them stay valid.
func (r *StorageConnectionRepository) ResealBatch(ctx context.Context, keyVersion int, after string, limit int, write bool,
	reseal func(*SealedCredentials) error) (last string, resealed int, err error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("begin reseal: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, provider,
		    credential_version, coalesce(credential_key_version, 0), credential_nonce, credential_ciphertext, credential_wrapped_key,
		    coalesce(staged_credential_version, 0), coalesce(staged_key_version, 0), staged_nonce, staged_ciphertext, staged_wrapped_key,
		    coalesce(previous_credential_version, 0), coalesce(previous_key_version, 0), previous_nonce, previous_ciphertext, previous_wrapped_key
		FROM storage_connections
		WHERE status <> 'DELETED' AND id > $2 AND (
		    (credential_ciphertext IS NOT NULL AND (credential_wrapped_key IS NULL OR credential_key_version <> $1)) OR
		    (staged_ciphertext IS NOT NULL AND (staged_wrapped_key IS NULL OR staged_key_version <> $1)) OR
		    (previous_ciphertext IS NOT NULL AND (previous_wrapped_key IS NULL OR previous_key_version <> $1)))
		ORDER BY id
		LIMIT $3
		FOR UPDATE`, keyVersion, after, limit)
	if err != nil {
		return "", 0, fmt.Errorf("select credentials to reseal: %w", err)
	}
	var batch []SealedCredentials
	for rows.Next() {
		var s SealedCredentials
		c, cur, st, prev := &s.Connection, &s.Current, &s.Staged, &s.Previous
		if err := rows.Scan(&c.ID, &c.TenantID, &c.Provider,
			&cur.Version, &cur.KeyVersion, &cur.Nonce, &cur.Ciphertext, &cur.WrappedKey,
			&st.Version, &st.KeyVersion, &st.Nonce, &st.Ciphertext, &st.WrappedKey,
			&prev.Version, &prev.KeyVersion, &prev.Nonce, &prev.Ciphertext, &prev.WrappedKey); err != nil {
			rows.Close()
			return "", 0, fmt.Errorf("read credentials to reseal: %w", err)
		}
		batch = append(batch, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("read credentials to reseal: %w", err)
	}

	for i := range batch {
		s := &batch[i]
		last = s.Connection.ID
		if err := reseal(s); err != nil {
			continue
		}
		_, err := tx.Exec(ctx, `
			UPDATE storage_connections SET
			    credential_key_version = CASE WHEN credential_ciphertext IS NULL THEN NULL ELSE $2::int END,
			    credential_nonce = $3, credential_ciphertext = $4, credential_wrapped_key = $5,
			    staged_key_version = CASE WHEN staged_ciphertext IS NULL THEN NULL ELSE $6::int END,
			    staged_nonce = $7, staged_ciphertext = $8, staged_wrapped_key = $9,
			    previous_key_version = CASE WHEN previous_ciphertext IS NULL THEN NULL ELSE $10::int END,
			    previous_nonce = $11, previous_ciphertext = $12, previous_wrapped_key = $13
			WHERE id = $1`,
			s.Connection.ID, s.Current.KeyVersion, s.Current.Nonce, s.Current.Ciphertext, s.Current.WrappedKey,
			s.Staged.KeyVersion, s.Staged.Nonce, s.Staged.Ciphertext, s.Staged.WrappedKey,
			s.Previous.KeyVersion, s.Previous.Nonce, s.Previous.Ciphertext, s.Previous.WrappedKey)
		if err != nil {
			return "", 0, fmt.Errorf("write resealed credentials: %w", err)
		}
		resealed++
	}
	if !write {
		return last, resealed, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("commit reseal: %w", err)
	}
	return last, resealed, nil
}

// EraseDeletedCredentials erases staged and previous credentials that
// deleted connections kept from before Delete erased them too.
func (r *StorageConnectionRepository) EraseDeletedCredentials(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE storage_connections SET
		    staged_credential_version = NULL, staged_key_version = NULL, staged_nonce = NULL, staged_ciphertext = NULL,
		    staged_wrapped_key = NULL, staged_at = NULL, staged_by = NULL,
		    previous_credential_version = NULL, previous_key_version = NULL, previous_nonce = NULL,
		    previous_ciphertext = NULL, previous_wrapped_key = NULL, previous_until = NULL
		WHERE status = 'DELETED' AND (staged_ciphertext IS NOT NULL OR previous_ciphertext IS NOT NULL)`)
	if err != nil {
		return 0, fmt.Errorf("erase deleted connections' credentials: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CredentialKeyUse counts stored credentials (current, staged and previous,
// deleted connections included) by the master key version they are under
// and whether they are in an envelope. A key version may be removed from
// STORAGE_CREDENTIAL_KEYS once nothing is counted under it.
type CredentialKeyUse struct {
	KeyVersion int  `json:"key_version"`
	Enveloped  bool `json:"enveloped"`
	Count      int  `json:"count"`
}

func (r *StorageConnectionRepository) CredentialKeyUse(ctx context.Context) ([]CredentialKeyUse, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT key_version, enveloped, count(*) FROM (
		    SELECT credential_key_version AS key_version, credential_wrapped_key IS NOT NULL AS enveloped
		    FROM storage_connections WHERE credential_ciphertext IS NOT NULL
		    UNION ALL
		    SELECT staged_key_version, staged_wrapped_key IS NOT NULL FROM storage_connections WHERE staged_ciphertext IS NOT NULL
		    UNION ALL
		    SELECT previous_key_version, previous_wrapped_key IS NOT NULL FROM storage_connections WHERE previous_ciphertext IS NOT NULL
		) AS sealed
		GROUP BY key_version, enveloped
		ORDER BY key_version, enveloped`)
	if err != nil {
		return nil, fmt.Errorf("count credentials by key: %w", err)
	}
	defer rows.Close()
	var out []CredentialKeyUse
	for rows.Next() {
		var u CredentialKeyUse
		if err := rows.Scan(&u.KeyVersion, &u.Enveloped, &u.Count); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
