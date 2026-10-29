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
	COALESCE(failure_reason, ''),
	completed_at,
	aborted_at,
	COALESCE(dataset, ''),
	COALESCE(path, ''),
	COALESCE(object_id, ''),
	create_only,
	metadata,
	storage_connection_id,
	staging_connection_id
`

func scanUpload(row pgx.Row) (*model.UploadSession, error) {
	var upload model.UploadSession
	var metadata []byte

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
		&upload.CompletedAt,
		&upload.AbortedAt,
		&upload.Dataset,
		&upload.Path,
		&upload.ObjectID,
		&upload.CreateOnly,
		&metadata,
		&upload.StorageConnectionID,
		&upload.StagingConnectionID,
	)
	if err != nil {
		return nil, err
	}
	if upload.Metadata, err = decodeMetadata(metadata); err != nil {
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
	return r.insert(ctx, upload, "")
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
	return r.insert(ctx, upload, key)
}

// insert stores a session without a quota reservation; a catalogued one
// claims its object in the same transaction.
func (r *UploadRepository) insert(ctx context.Context, upload *model.UploadSession, key string) error {
	if upload.Path == "" {
		return insertUploadSession(ctx, r.db.Pool, upload, key, false, false)
	}
	return withTx(ctx, r.db, func(tx pgx.Tx) error {
		if err := claimObject(ctx, tx, upload); err != nil {
			return err
		}
		return insertUploadSession(ctx, tx, upload, key, false, false)
	})
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertUploadSession stores a session. With reserved set, its declared
// size is recorded as the tenant quota it holds (see QuotaRepository).
func insertUploadSession(ctx context.Context, q execer, upload *model.UploadSession, idempotencyKey string, reserved, multipart bool) error {
	var quotaState any
	var reservedBytes int64
	if reserved {
		quotaState, reservedBytes = "RESERVED", upload.ExpectedSize
	}
	metadata, err := encodeMetadata(upload.Metadata)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO upload_sessions (
			id, tenant_id, owner_id, object_key,
			file_name, content_type, expected_size,
			checksum_sha256, status, idempotancy_key, expires_at,
			created_at, updated_at, storage_upload_id, dataset,
			quota_state, reserved_bytes, multipart,
			path, object_id, create_only, metadata,
			storage_connection_id, staging_connection_id
		)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			NULLIF($8, ''), $9, NULLIF($10, ''), $11, $12, $13,
			NULLIF($14, ''), NULLIF($15, ''), $16, $17, $18,
			NULLIF($19, ''), NULLIF($20, ''), $21, $22,
			COALESCE(NULLIF($23, ''), 'platform'), COALESCE(NULLIF($24, ''), 'platform')
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
		idempotencyKey,
		upload.ExpiresAt,
		upload.CreatedAt,
		upload.UpdatedAt,
		upload.StorageUploadID,
		upload.Dataset,
		quotaState,
		reservedBytes,
		multipart,
		upload.Path,
		upload.ObjectID,
		upload.CreateOnly,
		metadata,
		upload.StorageConnectionID,
		upload.StagingConnectionID,
	)
	if err != nil {
		return mapDatabaseError(bindingError(err))
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

func (r *UploadRepository) List(ctx context.Context, tenantID string, status model.UploadStatus, after string, limit int) ([]model.UploadSession, string, error) {
	at, id, err := decodeTimeCursor(after)
	if err != nil {
		return nil, "", err
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+uploadColumns+`
		FROM upload_sessions
		WHERE tenant_id = $1
		  AND ($2 = '' OR status = $2)
		  AND ($3 = '' OR (created_at, id) < ($4, $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $5
	`, tenantID, string(status), id, at, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list upload sessions: %w", err)
	}
	defer rows.Close()

	var uploads []model.UploadSession
	for rows.Next() {
		upload, err := scanUpload(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan upload session: %w", err)
		}
		uploads = append(uploads, *upload)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list upload sessions: %w", err)
	}
	if len(uploads) <= limit {
		return uploads, "", nil
	}
	uploads = uploads[:limit]
	return uploads, encodeTimeCursor(uploads[limit-1].CreatedAt, uploads[limit-1].ID), nil
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
	events ...model.OutboxEvent,
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

	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin upload transition: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// The status condition makes this a compare-and-set: of two racing
	// writers (complete vs abort, worker vs request) exactly one wins.
	tag, err := tx.Exec(ctx, `
		UPDATE upload_sessions
		SET status = $3,
		    failure_reason = COALESCE(NULLIF($4, ''), failure_reason),
		    completed_at = CASE WHEN $3 = 'COMPLETED' THEN $5 ELSE completed_at END,
		    aborted_at = CASE WHEN $3 IN ('ABORTED', 'EXPIRED') THEN $5 ELSE aborted_at END,
		    updated_at = $5
		WHERE tenant_id = $1
		  AND id = $2
		  AND status = ANY($6)
	`, tenantID, uploadID, string(to), failureReason, at, allowed)
	if err != nil {
		return mapDatabaseError(err)
	}
	if tag.RowsAffected() == 0 {
		return r.missingOrInvalid(ctx, tenantID, uploadID)
	}

	// An upload that ends without an object gives back its quota in the
	// same transaction.
	switch to {
	case model.UploadStatusFailed, model.UploadStatusAborted, model.UploadStatusExpired:
		if err := releaseReservation(ctx, tx, tenantID, uploadID, at); err != nil {
			return err
		}
	}

	if err := insertOutbox(ctx, tx, events...); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit upload transition: %w", err)
	}
	return nil
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

func (r *UploadRepository) SetDeclaredChecksum(
	ctx context.Context,
	tenantID string,
	uploadID string,
	checksum string,
	at time.Time,
) error {
	if checksum == "" {
		return errors.New("checksum must not be empty")
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE upload_sessions
		SET checksum_sha256 = $3,
		    updated_at = $4
		WHERE tenant_id = $1
		  AND id = $2
		  AND status = 'UPLOADING'
		  AND (checksum_sha256 IS NULL OR checksum_sha256 = $3)
	`, tenantID, uploadID, checksum, at)
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
