package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// multipartUploadRepository implements multipart metadata persistence
// using Neon PostgreSQL.
type multipartUploadRepository struct {
	db *DB
}

// NewMultipartUploadRepository constructs the PostgreSQL adapter.
func NewMultipartUploadRepository(
	db *DB,
) interfaces.MultipartUploadRepository {
	return &multipartUploadRepository{db: db}
}

// Ensure the adapter satisfies the application-owned interface.
var _ interfaces.MultipartUploadRepository = (*multipartUploadRepository)(nil)

// UpsertPart inserts a new part or updates an existing part when
// the client retries the same part number.
//
// The INSERT ... SELECT ensures that the upload session belongs
// to the tenant before any part metadata is written.
func (r *multipartUploadRepository) UpsertPart(
	ctx context.Context,
	tenantID string,
	part model.UploadPart,
) error {
	if err := part.Validate(); err != nil {
		return err
	}

	if tenantID == "" || tenantID != part.TenantID {
		return model.ErrInvalidUploadPart
	}

	const query = `
		INSERT INTO upload_parts (
			tenant_id,
			upload_id,
			part_number,
			etag,
			size_bytes,
			checksum_sha256,
			status,
			created_at,
			updated_at
		)
		SELECT
			$1,
			$2,
			$3,
			$4,
			$5,
			NULLIF($6, ''),
			$7,
			NOW(),
			NOW()
		FROM upload_sessions AS s
		WHERE s.id = $2
		  AND s.tenant_id = $1
		ON CONFLICT (upload_id, part_number)
		DO UPDATE SET
			etag = EXCLUDED.etag,
			size_bytes = EXCLUDED.size_bytes,
			checksum_sha256 = EXCLUDED.checksum_sha256,
			status = EXCLUDED.status,
			updated_at = NOW()
		WHERE upload_parts.tenant_id = EXCLUDED.tenant_id
		RETURNING upload_id
	`

	var savedUploadID string

	err := r.db.Pool.QueryRow(
		ctx,
		query,
		tenantID,
		part.UploadID,
		part.PartNumber,
		part.ETag,
		part.SizeBytes,
		part.ChecksumSHA256,
		string(part.Status),
	).Scan(&savedUploadID)

	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrUploadNotFound
	}
	if err != nil {
		return fmt.Errorf("upsert multipart part: %w", err)
	}

	return nil
}

// ListParts returns the recorded parts in ascending part-number order.
// It returns an empty slice when a valid session has no recorded parts.
func (r *multipartUploadRepository) ListParts(
	ctx context.Context,
	tenantID string,
	uploadID string,
) ([]model.UploadPart, error) {
	const sessionQuery = `
		SELECT EXISTS (
			SELECT 1
			FROM upload_sessions
			WHERE id = $1 AND tenant_id = $2
		)
	`

	var sessionExists bool

	err := r.db.Pool.QueryRow(
		ctx,
		sessionQuery,
		uploadID,
		tenantID,
	).Scan(&sessionExists)
	if err != nil {
		return nil, fmt.Errorf("check multipart upload session: %w", err)
	}

	if !sessionExists {
		return nil, model.ErrUploadNotFound
	}

	const query = `
		SELECT
			tenant_id,
			upload_id,
			part_number,
			COALESCE(etag, ''),
			size_bytes,
			COALESCE(checksum_sha256, ''),
			status,
			created_at,
			updated_at
		FROM upload_parts
		WHERE tenant_id = $1
		  AND upload_id = $2
		ORDER BY part_number ASC
	`

	rows, err := r.db.Pool.Query(ctx, query, tenantID, uploadID)
	if err != nil {
		return nil, fmt.Errorf("list multipart parts: %w", err)
	}
	defer rows.Close()

	parts := make([]model.UploadPart, 0)

	for rows.Next() {
		var part model.UploadPart
		var status string

		err := rows.Scan(
			&part.TenantID,
			&part.UploadID,
			&part.PartNumber,
			&part.ETag,
			&part.SizeBytes,
			&part.ChecksumSHA256,
			&status,
			&part.CreatedAt,
			&part.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan multipart part: %w", err)
		}

		part.Status = model.UploadPartStatus(status)
		parts = append(parts, part)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate multipart parts: %w", err)
	}

	return parts, nil
}

// DeleteParts removes the metadata for one tenant-owned upload.
// The session itself is not deleted by this method.
func (r *multipartUploadRepository) DeleteParts(
	ctx context.Context,
	tenantID string,
	uploadID string,
) error {
	const query = `
		DELETE FROM upload_parts AS p
		USING upload_sessions AS s
		WHERE p.upload_id = s.id
		  AND p.tenant_id = s.tenant_id
		  AND s.tenant_id = $1
		  AND p.upload_id = $2
	`

	_, err := r.db.Pool.Exec(ctx, query, tenantID, uploadID)
	if err != nil {
		return fmt.Errorf("delete multipart parts: %w", err)
	}

	return nil
}
