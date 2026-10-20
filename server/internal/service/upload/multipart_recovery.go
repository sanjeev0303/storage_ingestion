package upload

import (
	"context"
	"errors"
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	checksum "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/checksum/sha256"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// The methods below finish operations whose provider outcome was unknown.
// Each one reads the session, inspects the provider, and moves the session
// with a compare-and-set, so running it again, or on two workers at once,
// is safe. Each returns the session's resulting status. A nil error means
// nothing is left to do; any error leaves the session for another attempt
// (retry.IsPermanent marks the ones retrying cannot fix).

// ResolveInitiation settles a PENDING session that never recorded a
// provider upload ID. It lists the provider's in-progress uploads for the
// session's object key (unique per session) rather than initiating again.
func (s *MultipartService) ResolveInitiation(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error) {
	session, err := s.getSession(ctx, tenantID, uploadID)
	if err != nil {
		return "", err
	}
	if session.Status != model.UploadStatusPending || session.StorageUploadID != "" {
		return session.Status, nil
	}

	at, err := s.staging(ctx, session)
	if err != nil {
		return "", err
	}
	ids, err := at.storage.ListMultipartUploads(ctx, at.bucket, session.ObjectKey)
	if err != nil {
		return "", fmt.Errorf("list provider uploads: %w", err)
	}

	switch len(ids) {
	case 0:
		// The provider holds no upload for this key: initiation never took
		// effect. The client starts again with a new idempotency key.
		return s.finish(ctx, session, model.UploadStatusFailed, model.FailureInitiationFailed)

	case 1:
		err := s.uploads.SetStorageUploadID(ctx, tenantID, uploadID, ids[0], s.now())
		if errors.Is(err, model.ErrInvalidUploadState) {
			return s.currentStatus(ctx, tenantID, uploadID)
		}
		if err != nil {
			return "", fmt.Errorf("adopt provider upload: %w", err)
		}
		return model.UploadStatusPending, nil

	default:
		// Never expected, since nothing initiates twice. Keeping either one
		// would be a guess, so remove them all and fail the session.
		for _, id := range ids {
			err := at.storage.AbortMultipart(ctx, at.bucket, session.ObjectKey, id)
			if err != nil && !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
				return "", fmt.Errorf("abort duplicate provider upload: %w", err)
			}
		}
		return s.finish(ctx, session, model.UploadStatusFailed, model.FailureDuplicateInitiation)
	}
}

// ResolveCompletion settles a COMPLETING session. It completes again only
// when the provider shows no object and the upload is still active with a
// valid, complete set of parts; an assembled object is verified instead.
func (s *MultipartService) ResolveCompletion(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error) {
	session, err := s.getSession(ctx, tenantID, uploadID)
	if err != nil {
		return "", err
	}
	if session.Status != model.UploadStatusCompleting {
		return session.Status, nil
	}

	status, err := s.confirmCompletion(ctx, session)
	if !errors.Is(err, interfaces.ErrObjectNotFound) {
		return status, err
	}

	at, err := s.staging(ctx, session)
	if err != nil {
		return "", err
	}
	stored, err := at.storage.ListParts(ctx, at.bucket, session.ObjectKey, session.StorageUploadID)
	if errors.Is(err, interfaces.ErrStorageUploadNotFound) {
		// Neither an object nor an upload. The upload may have completed
		// between our two calls, so look for the object once more.
		status, headErr := s.confirmCompletion(ctx, session)
		if errors.Is(headErr, interfaces.ErrObjectNotFound) {
			return s.finish(ctx, session, model.UploadStatusFailed, model.FailureUploadMissing)
		}
		return status, headErr
	}
	if err != nil {
		return "", fmt.Errorf("inspect provider parts: %w", err)
	}

	recorded, err := s.syncProviderParts(ctx, session, stored)
	if err != nil {
		return "", err
	}

	// The provider's own parts are the completion list; they must still add
	// up to the declared size, or an operator has to look at the upload.
	asInput := CompleteMultipartInput{Parts: make([]CompletedPartInput, 0, len(recorded))}
	for _, part := range recorded {
		asInput.Parts = append(asInput.Parts, CompletedPartInput{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	parts, err := validateCompletion(session, recorded, asInput, at.rules)
	if err != nil {
		return "", retry.Permanent(fmt.Errorf("provider parts cannot complete the upload: %w", err))
	}

	if err := at.storage.CompleteMultipart(ctx, at.bucket, session.ObjectKey, session.StorageUploadID, parts); err != nil {
		return "", err
	}

	return s.confirmCompletion(ctx, session)
}

// confirmCompletion checks the assembled object and moves a COMPLETING
// session to COMPLETED, or to FAILED when its size is wrong. It returns
// interfaces.ErrObjectNotFound while no object exists.
func (s *MultipartService) confirmCompletion(ctx context.Context, session model.UploadSession) (model.UploadStatus, error) {
	at, err := s.staging(ctx, session)
	if err != nil {
		return "", err
	}
	metadata, err := at.storage.HeadObject(ctx, at.bucket, session.ObjectKey)
	if err != nil {
		return "", fmt.Errorf("inspect completed object: %w", err)
	}

	if metadata.SizeBytes != session.ExpectedSize {
		return s.finish(ctx, session, model.UploadStatusFailed, model.FailureObjectSizeMismatch)
	}
	return s.finish(ctx, session, model.UploadStatusCompleted, "")
}

// ResolveAbort settles an ABORTING session: every provider upload for the
// key is aborted, and no assembled object may remain.
func (s *MultipartService) ResolveAbort(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error) {
	session, err := s.getSession(ctx, tenantID, uploadID)
	if err != nil {
		return "", err
	}
	if session.Status != model.UploadStatusAborting {
		return session.Status, nil
	}

	at, err := s.staging(ctx, session)
	if err != nil {
		return "", err
	}
	ids := []string{session.StorageUploadID}
	if session.StorageUploadID == "" {
		// Initiation may have taken effect without its ID being saved.
		ids, err = at.storage.ListMultipartUploads(ctx, at.bucket, session.ObjectKey)
		if err != nil {
			return "", fmt.Errorf("list provider uploads: %w", err)
		}
	}

	for _, id := range ids {
		err := at.storage.AbortMultipart(ctx, at.bucket, session.ObjectKey, id)
		if err != nil && !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
			return "", fmt.Errorf("abort provider upload: %w", err)
		}
	}

	// "Upload not found" is also what a completed upload answers, so it
	// alone does not prove the abort's goal. No object may remain either.
	_, err = at.storage.HeadObject(ctx, at.bucket, session.ObjectKey)
	if err == nil {
		if err := at.storage.DeleteObject(ctx, at.bucket, session.ObjectKey); err != nil {
			return "", fmt.Errorf("delete object of aborted upload: %w", err)
		}
	} else if !errors.Is(err, interfaces.ErrObjectNotFound) {
		return "", fmt.Errorf("inspect object of aborted upload: %w", err)
	}

	if err := s.parts.DeleteParts(ctx, tenantID, uploadID); err != nil {
		return "", fmt.Errorf("delete aborted part metadata: %w", err)
	}

	// The reason recorded when the abort was claimed says how it ends.
	final := model.UploadStatusFailed
	switch session.FailureReason {
	case model.FailureExpired:
		final = model.UploadStatusExpired
	case model.FailureAbortedByClient:
		final = model.UploadStatusAborted
	}
	return s.finish(ctx, session, final, "")
}

// Expire moves an active session that passed its expiry time into
// cleanup. Expired sessions already refuse new part URLs and completion.
func (s *MultipartService) Expire(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error) {
	session, err := s.getSession(ctx, tenantID, uploadID)
	if err != nil {
		return "", err
	}

	switch session.Status {
	case model.UploadStatusPending, model.UploadStatusUploading:
		if s.now().Before(session.ExpiresAt) {
			return session.Status, nil
		}
		err := s.uploads.TransitionStatus(ctx, tenantID, uploadID,
			[]model.UploadStatus{model.UploadStatusPending, model.UploadStatusUploading},
			model.UploadStatusAborting, model.FailureExpired, s.now())
		if err != nil && !errors.Is(err, model.ErrInvalidUploadState) {
			return "", fmt.Errorf("claim expired upload: %w", err)
		}
	case model.UploadStatusAborting:
	default:
		return session.Status, nil
	}

	return s.ResolveAbort(ctx, tenantID, uploadID)
}

// VerifyIntegrity streams a COMPLETED object through SHA-256 and compares
// it with the checksum the client declared. A multipart ETag is not a
// content hash, so it is never used as one. Only a match makes the session
// VERIFIED, the one status that may be promoted; a mismatch is recorded as
// FAILED and the object stays in quarantine.
func (s *MultipartService) VerifyIntegrity(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error) {
	session, err := s.getSession(ctx, tenantID, uploadID)
	if err != nil {
		return "", err
	}
	if session.Status != model.UploadStatusCompleted {
		return session.Status, nil
	}

	at, err := s.staging(ctx, session)
	if err != nil {
		return "", err
	}
	if session.ObjectKey != at.key {
		return s.finish(ctx, session, model.UploadStatusFailed, model.FailureObjectKeyMismatch)
	}
	if session.ChecksumSHA256 == "" {
		return s.finish(ctx, session, model.UploadStatusFailed, model.FailureChecksumMissing)
	}

	metadata, err := at.storage.HeadObject(ctx, at.bucket, session.ObjectKey)
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return s.finish(ctx, session, model.UploadStatusFailed, model.FailureUploadMissing)
	}
	if err != nil {
		return "", fmt.Errorf("inspect object for verification: %w", err)
	}
	if metadata.SizeBytes != session.ExpectedSize {
		return s.finish(ctx, session, model.UploadStatusFailed, model.FailureObjectSizeMismatch)
	}

	body, err := at.storage.OpenObject(ctx, at.bucket, session.ObjectKey)
	if err != nil {
		return "", fmt.Errorf("open object for verification: %w", err)
	}
	defer body.Close()

	sum, err := checksum.Sum(ctx, body)
	if err != nil {
		return "", fmt.Errorf("hash object: %w", err)
	}

	if sum != session.ChecksumSHA256 {
		return s.finish(ctx, session, model.UploadStatusFailed, model.FailureChecksumMismatch)
	}
	return s.finish(ctx, session, model.UploadStatusVerified, "")
}

// finish moves the session from its current status to `to`. If another
// actor moved it first, that newer state wins and is returned unchanged.
// Completion writes upload.completed in the same transaction, so a
// completed upload always has the event that starts its processing.
func (s *MultipartService) finish(
	ctx context.Context,
	session model.UploadSession,
	to model.UploadStatus,
	reason string,
) (model.UploadStatus, error) {
	now := s.now()
	var events []model.OutboxEvent
	if to == model.UploadStatusCompleted {
		events = append(events, event.NewUploadCompleted(session.TenantID, session.ID, now, correlation.From(ctx)))
	}

	err := s.uploads.TransitionStatus(ctx, session.TenantID, session.ID,
		[]model.UploadStatus{session.Status}, to, reason, now, events...)
	if errors.Is(err, model.ErrInvalidUploadState) {
		return s.currentStatus(ctx, session.TenantID, session.ID)
	}
	if err != nil {
		return "", fmt.Errorf("move upload to %s: %w", to, err)
	}
	return to, nil
}

func (s *MultipartService) currentStatus(ctx context.Context, tenantID, uploadID string) (model.UploadStatus, error) {
	session, err := s.getSession(ctx, tenantID, uploadID)
	if err != nil {
		return "", err
	}
	return session.Status, nil
}
