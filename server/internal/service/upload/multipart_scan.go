package upload

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// quarantinePrefix is where every upload session's object lives.
const quarantinePrefix = "quarantine/"

// ScanOptions bound one scan step.
type ScanOptions struct {
	PageSize int
	// MinAge protects young provider sessions and objects: an initiation or
	// upload may still be in flight and not yet recorded in the database.
	MinAge time.Duration
	// DryRun reports what would be done and changes nothing.
	DryRun bool
	// DeleteUnrecorded allows aborting or deleting provider sessions and
	// objects that have no upload session at all. Off by default: a missing
	// row proves ownership only if this database is the bucket's sole
	// writer (another environment may share the bucket).
	DeleteUnrecorded bool
}

// reasonUnrecorded marks findings whose upload session does not exist.
const reasonUnrecorded = "no upload session exists for this "

// ScanOrphanSessions checks one page of provider multipart sessions under
// quarantine/ and aborts those no live upload session owns. It returns the
// findings and the cursor for the next page ("" when the listing is done).
// Ownership is established from the key (quarantine/{tenant}/{upload}) and
// the tenant-scoped session lookup; anything else is left alone.
func (s *MultipartService) ScanOrphanSessions(ctx context.Context, cursor string, opts ScanOptions) ([]model.ReconciliationFinding, string, error) {
	page, err := s.storage.ListMultipartUploadsPage(ctx, s.bucket, quarantinePrefix, cursor, opts.PageSize)
	if err != nil {
		return nil, cursor, fmt.Errorf("list provider sessions: %w", err)
	}

	var findings []model.ReconciliationFinding
	for _, upload := range page.Uploads {
		ref, reason, err := s.orphanSession(ctx, upload, opts.MinAge)
		if err != nil {
			// Retry this page next time; aborting is idempotent.
			return findings, cursor, err
		}
		if reason == "" {
			continue
		}

		finding := s.newFinding(model.FindingOrphanSession, model.ActionAbortSession, ref, upload.ObjectKey, reason)
		finding.StorageUploadID = upload.StorageUploadID
		if unprovable(reason, opts) {
			finding.Outcome = model.FindingReported
		} else if !opts.DryRun {
			err := s.storage.AbortMultipart(ctx, s.bucket, upload.ObjectKey, upload.StorageUploadID)
			finding.Outcome, finding.Reason = outcome(err, interfaces.ErrStorageUploadNotFound, reason)
		}
		findings = append(findings, finding)
	}
	return findings, page.Cursor, nil
}

// orphanSession returns why a provider session is an orphan, or "" when it
// is young, not ours, or still owned by a live session (whose own
// reconciliation task handles it).
func (s *MultipartService) orphanSession(ctx context.Context, upload interfaces.StoredMultipartUpload, minAge time.Duration) (model.UploadRef, string, error) {
	if s.now().Sub(upload.Initiated) < minAge {
		return model.UploadRef{}, "", nil
	}
	ref, ok := parseQuarantineKey(upload.ObjectKey)
	if !ok {
		return model.UploadRef{}, "", nil
	}

	session, err := s.uploads.GetByID(ctx, ref.TenantID, ref.UploadID)
	if errors.Is(err, model.ErrUploadNotFound) {
		return ref, reasonUnrecorded + "key", nil
	}
	if err != nil {
		return ref, "", fmt.Errorf("look up session: %w", err)
	}

	switch {
	case session.Status == model.UploadStatusPending && session.StorageUploadID == "":
		return ref, "", nil // ResolveInitiation adopts or cleans it up
	case session.Status == model.UploadStatusAborting:
		return ref, "", nil // the ABORT task aborts it
	case session.StorageUploadID != upload.StorageUploadID:
		return ref, "a second provider session for one upload", nil
	case session.Status.IsTerminal():
		return ref, "upload session is " + string(session.Status), nil
	}
	return ref, "", nil // live or completed: owned
}

// ScanOrphanObjects checks one page of quarantine objects and deletes those
// whose upload session is gone, aborted or expired. FAILED sessions keep
// their object as evidence (retention); live and completed ones are owned.
func (s *MultipartService) ScanOrphanObjects(ctx context.Context, cursor string, opts ScanOptions) ([]model.ReconciliationFinding, string, error) {
	page, err := s.storage.ListObjectsPage(ctx, s.bucket, quarantinePrefix, cursor, opts.PageSize)
	if err != nil {
		return nil, cursor, fmt.Errorf("list quarantine objects: %w", err)
	}

	var findings []model.ReconciliationFinding
	for _, object := range page.Objects {
		if s.now().Sub(object.LastModified) < opts.MinAge {
			continue
		}
		ref, ok := parseQuarantineKey(object.ObjectKey)
		if !ok {
			continue
		}

		reason := ""
		session, err := s.uploads.GetByID(ctx, ref.TenantID, ref.UploadID)
		switch {
		case errors.Is(err, model.ErrUploadNotFound):
			reason = reasonUnrecorded + "object"
		case err != nil:
			return findings, cursor, fmt.Errorf("look up session: %w", err)
		case session.Status == model.UploadStatusAborted || session.Status == model.UploadStatusExpired:
			reason = "upload session is " + string(session.Status)
		}
		if reason == "" {
			continue
		}

		finding := s.newFinding(model.FindingOrphanObject, model.ActionDeleteObject, ref, object.ObjectKey, reason)
		if unprovable(reason, opts) {
			finding.Outcome = model.FindingReported
		} else if !opts.DryRun {
			err := s.storage.DeleteObject(ctx, s.bucket, object.ObjectKey)
			finding.Outcome, finding.Reason = outcome(err, interfaces.ErrObjectNotFound, reason)
		}
		findings = append(findings, finding)
	}
	return findings, page.Cursor, nil
}

// CleanStaleParts removes part rows of finished sessions. Each session is
// re-read first, so one that changed since it was listed is left alone.
func (s *MultipartService) CleanStaleParts(ctx context.Context, refs []model.UploadRef, dryRun bool) ([]model.ReconciliationFinding, error) {
	var findings []model.ReconciliationFinding
	for _, ref := range refs {
		session, err := s.getSession(ctx, ref.TenantID, ref.UploadID)
		if errors.Is(err, ErrMultipartUploadNotFound) {
			continue
		}
		if err != nil {
			return findings, err
		}
		if !session.Status.IsTerminal() {
			continue
		}

		reason := "part rows left on a " + string(session.Status) + " session"
		finding := s.newFinding(model.FindingStaleParts, model.ActionDeleteParts, ref, session.ObjectKey, reason)
		if !dryRun {
			err := s.parts.DeleteParts(ctx, ref.TenantID, ref.UploadID)
			finding.Outcome, finding.Reason = outcome(err, nil, reason)
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

func (s *MultipartService) newFinding(kind, action string, ref model.UploadRef, objectKey, reason string) model.ReconciliationFinding {
	id, err := generateMultipartUploadID()
	if err != nil {
		id = fmt.Sprintf("finding-%d", s.now().UnixNano())
	}
	return model.ReconciliationFinding{
		ID: id, Kind: kind, TenantID: ref.TenantID, UploadID: ref.UploadID, ObjectKey: objectKey,
		Action: action, Outcome: model.FindingPlanned, Reason: reason, CreatedAt: s.now(),
	}
}

// outcome maps an action's error: none, or the "already gone" answer,
// means done; anything else is a recorded failure for the next cycle.
func outcome(err error, alreadyGone error, reason string) (string, string) {
	if err == nil || (alreadyGone != nil && errors.Is(err, alreadyGone)) {
		return model.FindingDone, reason
	}
	return model.FindingFailed, reason + ": " + retry.Sanitize(err)
}

// parseQuarantineKey reads quarantine/{tenant}/{upload}; anything else is
// not an upload object and is never touched.
func parseQuarantineKey(key string) (model.UploadRef, bool) {
	rest, ok := strings.CutPrefix(key, quarantinePrefix)
	if !ok {
		return model.UploadRef{}, false
	}
	tenant, upload, ok := strings.Cut(rest, "/")
	if !ok || tenant == "" || upload == "" || strings.Contains(upload, "/") {
		return model.UploadRef{}, false
	}
	return model.UploadRef{TenantID: tenant, UploadID: upload}, true
}

// AbortLeftoverSessions asks the provider, key by key, whether finished
// sessions (ABORTED, FAILED, EXPIRED) still hold multipart sessions, and
// aborts any it finds. It is the scan for providers such as MinIO that
// list multipart sessions only for an exact object key, not by prefix.
func (s *MultipartService) AbortLeftoverSessions(ctx context.Context, sessions []model.UploadSession, dryRun bool) ([]model.ReconciliationFinding, error) {
	var findings []model.ReconciliationFinding
	for _, session := range sessions {
		if !session.Status.IsTerminal() {
			continue
		}
		ids, err := s.storage.ListMultipartUploads(ctx, s.bucket, session.ObjectKey)
		if err != nil {
			return findings, fmt.Errorf("list provider sessions for a finished upload: %w", err)
		}

		ref := model.UploadRef{TenantID: session.TenantID, UploadID: session.ID}
		for _, id := range ids {
			reason := "upload session is " + string(session.Status) + " but the provider still holds a multipart session"
			finding := s.newFinding(model.FindingOrphanSession, model.ActionAbortSession, ref, session.ObjectKey, reason)
			finding.StorageUploadID = id
			if !dryRun {
				err := s.storage.AbortMultipart(ctx, s.bucket, session.ObjectKey, id)
				finding.Outcome, finding.Reason = outcome(err, interfaces.ErrStorageUploadNotFound, reason)
			}
			findings = append(findings, finding)
		}
	}
	return findings, nil
}

// unprovable reports whether a finding is about something with no upload
// session at all while deleting such things is not enabled.
func unprovable(reason string, opts ScanOptions) bool {
	return !opts.DeleteUnrecorded && strings.HasPrefix(reason, reasonUnrecorded)
}
