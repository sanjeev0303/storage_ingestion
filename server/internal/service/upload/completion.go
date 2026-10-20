package upload

import (
	"context"
	"errors"
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
)

// ErrObjectNotUploaded: the client has not uploaded the object yet, or the
// provider does not show it yet. Completing again later can succeed.
var ErrObjectNotUploaded = errors.New("the object has not been uploaded")

// Plan issues a fresh presigned PUT for a single-request upload that is
// still waiting for its object, for a client whose first URL expired.
func (s *Service) Plan(ctx context.Context, tenantID, uploadID string) (CreateResult, error) {
	session, err := s.repository.GetByID(ctx, tenantID, uploadID)
	if err != nil {
		return CreateResult{}, err
	}
	if session.Status != model.UploadStatusPending || session.StorageUploadID != "" {
		return CreateResult{}, fmt.Errorf("%w: the upload is %s", ErrInvalidRequest, session.Status)
	}
	return s.createResult(ctx, session, true)
}

// Complete finishes a single-request upload after the client has PUT its
// object. The presigned PUT does not limit the size, so the stored object
// must have exactly the declared size. The session then becomes COMPLETED
// with its upload.completed event in the same transaction, and processing
// verifies its checksum and content as it does for any upload. Completing
// again returns the status the first call reached.
func (s *Service) Complete(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error) {
	session, err := s.repository.GetByID(ctx, tenantID, uploadID)
	if err != nil {
		return "", err
	}
	switch session.Status {
	case model.UploadStatusCompleted, model.UploadStatusVerified:
		return session.Status, nil
	case model.UploadStatusFailed:
		if session.FailureReason == model.FailureObjectSizeMismatch {
			return session.Status, ErrObjectSizeMismatch
		}
		return session.Status, fmt.Errorf("%w: the upload failed: %s", ErrInvalidRequest, session.FailureReason)
	case model.UploadStatusPending:
	default:
		return session.Status, fmt.Errorf("%w: the upload is %s", ErrInvalidRequest, session.Status)
	}
	if session.StorageUploadID != "" {
		return session.Status, fmt.Errorf("%w: a multipart upload completes with its parts", ErrInvalidRequest)
	}

	meta, err := s.storage.HeadObject(ctx, s.bucket, session.ObjectKey)
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return session.Status, ErrObjectNotUploaded
	}
	if err != nil {
		return "", fmt.Errorf("check uploaded object: %w", err)
	}

	now := s.now()
	if meta.SizeBytes != session.ExpectedSize {
		err := s.repository.TransitionStatus(ctx, tenantID, uploadID,
			[]model.UploadStatus{model.UploadStatusPending}, model.UploadStatusFailed, model.FailureObjectSizeMismatch, now)
		if err == nil {
			// Bytes that are not what was declared are not left in
			// quarantine. Failing to delete them leaves an orphan for the
			// orphan sweep, never an asset.
			_ = s.storage.DeleteObject(ctx, s.bucket, session.ObjectKey)
			return model.UploadStatusFailed, ErrObjectSizeMismatch
		}
		if !errors.Is(err, model.ErrInvalidUploadState) {
			return "", fmt.Errorf("fail upload: %w", err)
		}
		return s.statusOf(ctx, tenantID, uploadID)
	}

	completed := event.NewUploadCompleted(tenantID, uploadID, now, correlation.From(ctx))
	err = s.repository.TransitionStatus(ctx, tenantID, uploadID,
		[]model.UploadStatus{model.UploadStatusPending}, model.UploadStatusCompleted, "", now, completed)
	if errors.Is(err, model.ErrInvalidUploadState) {
		// A concurrent completion won; report where it got.
		return s.statusOf(ctx, tenantID, uploadID)
	}
	if err != nil {
		return "", fmt.Errorf("complete upload: %w", err)
	}
	return model.UploadStatusCompleted, nil
}

func (s *Service) statusOf(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error) {
	session, err := s.repository.GetByID(ctx, tenantID, uploadID)
	if err != nil {
		return "", err
	}
	return session.Status, nil
}
