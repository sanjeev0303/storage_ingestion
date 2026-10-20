package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// UploadStatus describes the current state of an upload session.
type UploadStatus string

const (
	UploadStatusPending   UploadStatus = "PENDING"
	UploadStatusUploading UploadStatus = "UPLOADING"
	// COMPLETING: completion was requested and its provider outcome may be
	// unknown. Only the completing request or reconciliation moves it on.
	UploadStatusCompleting UploadStatus = "COMPLETING"
	// COMPLETED: the object is assembled and its size verified. It stays
	// quarantined until its checksum is verified.
	UploadStatusCompleted UploadStatus = "COMPLETED"
	// VERIFIED: the SHA-256 of the stored bytes matched. The only status
	// that may be promoted out of quarantine.
	UploadStatusVerified UploadStatus = "VERIFIED"
	// ABORTING: an abort or expiry cleanup was requested and the provider
	// has not yet confirmed that the multipart upload is gone.
	UploadStatusAborting UploadStatus = "ABORTING"
	// ABORTED: the client's abort is confirmed: no provider upload and no
	// assembled object remain.
	UploadStatusAborted UploadStatus = "ABORTED"
	// FAILED: a terminal failure recovery cannot safely continue past.
	UploadStatusFailed UploadStatus = "FAILED"
	// EXPIRED: the session outlived its lifetime and was cleaned up.
	UploadStatusExpired UploadStatus = "EXPIRED"
)

// IsTerminal reports whether no further transition is possible for the
// upload itself (processing continues separately from VERIFIED).
func (s UploadStatus) IsTerminal() bool {
	return s == UploadStatusAborted || s == UploadStatusFailed || s == UploadStatusExpired
}

// Failure reasons recorded with FAILED, EXPIRED and ABORTING sessions.
const (
	FailureInitiationFailed    = "initiation_failed"
	FailureDuplicateInitiation = "duplicate_initiation"
	FailureUploadMissing       = "provider_upload_missing"
	FailureObjectSizeMismatch  = "object_size_mismatch"
	FailureObjectKeyMismatch   = "object_key_mismatch"
	FailureChecksumMissing     = "checksum_missing"
	FailureChecksumMismatch    = "checksum_mismatch"
	FailureAbortedByClient     = "aborted_by_client"
	FailureExpired             = "expired"
)

// UploadSession holds the metadata needed to track an upload
// independently of the HTTP request that created it.
type UploadSession struct {
	ID             string
	TenantID       string
	OwnerID        string
	ObjectKey      string
	FileName       string
	ContentType    string
	ExpectedSize   int64
	ChecksumSHA256 string
	// Dataset optionally groups the resulting asset for retention.
	Dataset string
	// StorageUploadID identifies the multipart upload created
	// by the object-storage provider.
	// It is empty for sessions that have not started multipart upload.
	StorageUploadID string
	Status          UploadStatus
	// FailureReason explains a FAILED, EXPIRED, ABORTED or ABORTING session.
	FailureReason string
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// CompletedAt and AbortedAt record when completion or abort/expiry was
	// confirmed. Nil until then.
	CompletedAt *time.Time
	AbortedAt   *time.Time

	// The catalog: the normalized logical path the upload publishes to
	// (in Dataset's namespace), the object claimed for it, and the user
	// metadata of the version. CreateOnly refuses an existing path.
	// Sessions with no path are not catalogued.
	Path       string
	ObjectID   string
	CreateOnly bool
	Metadata   map[string]string
}

// Validate checks the basic invariants of an upload session.
// Authentication and configurable upload limits are enforced
// by the service layer in later phases.
func (u UploadSession) Validate() error {
	if strings.TrimSpace(u.ID) == "" {
		return errors.New("upload ID is required")
	}
	if strings.TrimSpace(u.TenantID) == "" {
		return errors.New("tenant ID is required")
	}
	if strings.TrimSpace(u.OwnerID) == "" {
		return errors.New("owner ID is required")
	}
	if strings.TrimSpace(u.ObjectKey) == "" {
		return errors.New("object key is required")
	}
	if strings.TrimSpace(u.FileName) == "" {
		return errors.New("file name is required")
	}
	if strings.TrimSpace(u.ContentType) == "" {
		return errors.New("content type is required")
	}
	if u.ExpectedSize <= 0 {
		return errors.New("expected size must be greater than zero")
	}
	if u.Status == "" {
		return errors.New("upload status is required")
	}
	if !u.ExpiresAt.After(u.CreatedAt) {
		return errors.New("upload expiration must be after creation")
	}

	return nil
}

// TransitionTo changes the upload status only when the transition
// is allowed by the upload lifecycle.
func (u *UploadSession) TransitionTo(next UploadStatus) error {
	if u == nil {
		return errors.New("upload session is nil")
	}

	if !CanTransition(u.Status, next) {
		return fmt.Errorf(
			"cannot transition upload from %s to %s",
			u.Status,
			next,
		)
	}

	u.Status = next
	u.UpdatedAt = time.Now().UTC()

	return nil
}

// CanTransition reports whether the upload lifecycle allows from -> next.
// Repositories apply the same rule as a conditional database update.
func CanTransition(from, next UploadStatus) bool {
	switch from {
	case UploadStatusPending:
		return next == UploadStatusUploading ||
			next == UploadStatusCompleted ||
			next == UploadStatusAborting ||
			next == UploadStatusFailed ||
			next == UploadStatusExpired

	case UploadStatusUploading:
		return next == UploadStatusCompleting ||
			next == UploadStatusCompleted ||
			next == UploadStatusAborting ||
			next == UploadStatusFailed ||
			next == UploadStatusExpired

	case UploadStatusCompleting:
		return next == UploadStatusCompleted ||
			next == UploadStatusFailed

	case UploadStatusCompleted:
		return next == UploadStatusVerified ||
			next == UploadStatusFailed

	case UploadStatusAborting:
		return next == UploadStatusAborted ||
			next == UploadStatusExpired ||
			next == UploadStatusFailed
	}

	return false
}

// CanPromote reports whether the object may leave quarantine.
// Only a session whose stored bytes passed checksum verification may.
func (u UploadSession) CanPromote() bool {
	return u.Status == UploadStatusVerified
}
