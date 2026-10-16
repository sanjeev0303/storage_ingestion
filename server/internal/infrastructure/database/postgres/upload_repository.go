package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

type UploadRepository struct {
	db *DB
}

var _ interfaces.UploadRepository = (*UploadRepository)(nil)

func NewUploadRepository(db *DB) *UploadRepository {
	return &UploadRepository{db: db}
}

const uploadColumns = `
	id,
	tenant_id,
	owner_id,
	object_key,
	file_name,
	content_type,
	expected_size,
	COALESCE(checksum_sha256, ''),
	status,
	expires_at,
	created_at,
	updated_at,
	COALESCE(storage_upload_id, ''),
	COALESCE(failure_reason, '')
`

func scanUpload(row pgx.Row) (*model.UploadSession, error) {
	var upload model.UploadSession

	err := row.Scan(
		&upload.ID,
		&upload.TenantID,
		&upload.OwnerID,
		&upload.ObjectKey,
		&upload.FileName,
		&upload.ContentType,
		&upload.ExpectedSize,
		&upload.ChecksumSHA256,
		&upload.Status,
		&upload.ExpiresAt,
		&upload.CreatedAt,
		&upload.UpdatedAt,
		&upload.StorageUploadID,
		&upload.FailureReason,
	)
	if err != nil {
		return nil, err
	}

	return &upload, nil
}

func (r *UploadRepository) Create(
	ctx context.Context,
	upload *model.UploadSession,
) error {
	if upload == nil {
		return errors.New("upload session must not be nil")
	}

	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO upload_sessions (
			id, tenant_id, owner_id, object_key,
			file_name, content_type, expected_size,
			checksum_sha256, status, expires_at,
			created_at, updated_at, storage_upload_id
		)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			NULLIF($8, ''), $9, $10, $11, $12,
			NULLIF($13, '')
		)
	`,
		upload.ID,
		upload.TenantID,
		upload.OwnerID,
		upload.ObjectKey,
		upload.FileName,
		upload.ContentType,
		upload.ExpectedSize,
		upload.ChecksumSHA256,
		upload.Status,
		upload.ExpiresAt,
		upload.CreatedAt,
		upload.UpdatedAt,
		upload.StorageUploadID,
	)
	if err != nil {
		return mapDatabaseError(err)
	}

	return nil
}

func (r *UploadRepository) CreateWithIdempotency(
	ctx context.Context,
	upload *model.UploadSession,
	key string,
) error {
	if upload == nil {
		return errors.New("upload session must not be nil")
	}
	if key == "" {
		return errors.New("idempotency key must not be empty")
	}

	// The upload row and its idempotency key are committed together.
	// PostgreSQL's unique index arbitrates concurrent requests.
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO upload_sessions (
			id, tenant_id, owner_id, object_key,
			file_name, content_type, expected_size,
			checksum_sha256, status, idempotancy_key,
			expires_at, created_at, updated_at
		)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			NULLIF($8, ''), $9, $10, $11, $12, $13
		)
	`,
		upload.ID,
		upload.TenantID,
		upload.OwnerID,
		upload.ObjectKey,
		upload.FileName,
		upload.ContentType,
		upload.ExpectedSize,
		upload.ChecksumSHA256,
		upload.Status,
		key,
		upload.ExpiresAt,
		upload.CreatedAt,
		upload.UpdatedAt,
	)
	if err != nil {
		return mapDatabaseError(err)
	}

	return nil
}

func (r *UploadRepository) GetByID(
	ctx context.Context,
	tenantID string,
	uploadID string,
) (*model.UploadSession, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT `+uploadColumns+`
		FROM upload_sessions
		WHERE tenant_id = $1 AND id = $2
	`, tenantID, uploadID)

	upload, err := scanUpload(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, model.ErrUploadNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query upload session: %w", err)
	}

	return upload, nil
}

func (r *UploadRepository) GetByIdempotencyKey(
	ctx context.Context,
	tenantID string,
	ownerID string,
	key string,
) (*model.UploadSession, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT `+uploadColumns+`
		FROM upload_sessions
		WHERE tenant_id = $1
		  AND owner_id = $2
		  AND idempotancy_key = $3
	`, tenantID, ownerID, key)

	upload, err := scanUpload(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, model.ErrUploadNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query idempotent upload: %w", err)
	}

	return upload, nil
}

func (r *UploadRepository) Update(
	ctx context.Context,
	upload *model.UploadSession,
) error {
	if upload == nil {
		return errors.New("upload session must not be nil")
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE upload_sessions
		SET object_key = $3,
		    file_name = $4,
		    content_type = $5,
		    expected_size = $6,
		    checksum_sha256 = NULLIF($7, ''),
		    status = $8,
		    expires_at = $9,
		    updated_at = $10,
		    storage_upload_id = NULLIF($11, ''),
		    failure_reason = NULLIF($12, '')
		WHERE tenant_id = $1 AND id = $2
	`,
		upload.TenantID,
		upload.ID,
		upload.ObjectKey,
		upload.FileName,
		upload.ContentType,
		upload.ExpectedSize,
		upload.ChecksumSHA256,
		upload.Status,
		upload.ExpiresAt,
		upload.UpdatedAt,
		upload.StorageUploadID,
		upload.FailureReason,
	)
	if err != nil {
		return mapDatabaseError(err)
	}

	if tag.RowsAffected() == 0 {
		return model.ErrUploadNotFound
	}

	return nil
}

func (r *UploadRepository) TransitionStatus(
	ctx context.Context,
	tenantID string,
	uploadID string,
	from []model.UploadStatus,
	to model.UploadStatus,
	failureReason string,
	at time.Time,
) error {
	allowed := make([]string, 0, len(from))
	for _, status := range from {
		if model.CanTransition(status, to) {
			allowed = append(allowed, string(status))
		}
	}
	if len(allowed) == 0 {
		return fmt.Errorf("%w: no allowed transition to %s", model.ErrInvalidUploadState, to)
	}

	// The status condition makes this a compare-and-set: of two racing
	// writers (complete vs abort, worker vs request) exactly one wins.
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE upload_sessions
		SET status = $3,
		    failure_reason = COALESCE(NULLIF($4, ''), failure_reason),
		    updated_at = $5
		WHERE tenant_id = $1
		  AND id = $2
		  AND status = ANY($6)
	`, tenantID, uploadID, string(to), failureReason, at, allowed)
	if err != nil {
		return mapDatabaseError(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	return r.missingOrInvalid(ctx, tenantID, uploadID)
}

func (r *UploadRepository) SetStorageUploadID(
	ctx context.Context,
	tenantID string,
	uploadID string,
	storageUploadID string,
	at time.Time,
) error {
	if storageUploadID == "" {
		return errors.New("storage upload ID must not be empty")
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE upload_sessions
		SET storage_upload_id = $3,
		    updated_at = $4
		WHERE tenant_id = $1
		  AND id = $2
		  AND status = 'PENDING'
		  AND (storage_upload_id IS NULL OR storage_upload_id = $3)
	`, tenantID, uploadID, storageUploadID, at)
	if err != nil {
		return mapDatabaseError(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	return r.missingOrInvalid(ctx, tenantID, uploadID)
}

// missingOrInvalid tells a conditional update that matched nothing apart:
// the session does not exist, or it is in a state the update refused.
func (r *UploadRepository) missingOrInvalid(
	ctx context.Context,
	tenantID string,
	uploadID string,
) error {
	if _, err := r.GetByID(ctx, tenantID, uploadID); err != nil {
		return err
	}
	return model.ErrInvalidUploadState
}

func mapDatabaseError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" &&
			pgErr.ConstraintName == "ux_upload_sessions_idempotency":
			return fmt.Errorf(
				"%w: %s",
				model.ErrIdempotencyConflict,
				pgErr.ConstraintName,
			)

		case pgErr.Code == "23505" &&
			pgErr.ConstraintName == "upload_sessions_pkey":
			return fmt.Errorf(
				"%w: %s",
				model.ErrUploadAlreadyExists,
				pgErr.ConstraintName,
			)
		}
	}

	return fmt.Errorf("database operation failed: %w", err)
}
