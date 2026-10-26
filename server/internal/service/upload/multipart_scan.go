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

// ScanPlace is one connection's staging area, where its uploads land:
// keys Prefix/{tenant}/{upload}.
type ScanPlace struct {
	ConnectionID string
	Storage      interfaces.MultipartStorage
	Bucket       string
	Prefix       string
	// TenantID limits a tenant connection's scan to its own tenant's keys;
	// "" on the platform, which holds every tenant's.
	TenantID string
	// Customer marks a tenant's own bucket. What no upload session claims
	// there may be the customer's own: it is reported and never removed,
	// whatever DeleteUnrecorded says.
	Customer bool
}

// ListPrefix is what a scan of the place lists.
func (p ScanPlace) ListPrefix() string {
	if p.TenantID == "" {
		return p.Prefix + "/"
	}
	return p.Prefix + "/" + p.TenantID + "/"
}

// PlatformPlace is the platform's staging area: quarantine/ in its bucket.
func (s *MultipartService) PlatformPlace() ScanPlace {
	return ScanPlace{ConnectionID: model.PlatformConnectionID, Storage: s.storage, Bucket: s.bucket, Prefix: "quarantine"}
}

// ScanOrphanSessions scans the platform's staging area (ScanSessionsAt).
func (s *MultipartService) ScanOrphanSessions(ctx context.Context, cursor string, opts ScanOptions) ([]model.ReconciliationFinding, string, error) {
	return s.ScanSessionsAt(ctx, s.PlatformPlace(), cursor, opts)
}

// ScanOrphanObjects scans the platform's staging area (ScanObjectsAt).
func (s *MultipartService) ScanOrphanObjects(ctx context.Context, cursor string, opts ScanOptions) ([]model.ReconciliationFinding, string, error) {
	return s.ScanObjectsAt(ctx, s.PlatformPlace(), cursor, opts)
}

// ScanSessionsAt checks one page of the provider multipart sessions in a
// place and aborts those no live upload session owns. It returns the
// findings and the cursor for the next page ("" when the listing is done).
// Ownership is established from the key (prefix/{tenant}/{upload}), the
// tenant-scoped session lookup and the session's staging binding; anything
// else is left alone.
func (s *MultipartService) ScanSessionsAt(ctx context.Context, place ScanPlace, cursor string, opts ScanOptions) ([]model.ReconciliationFinding, string, error) {
	page, err := place.Storage.ListMultipartUploadsPage(ctx, place.Bucket, place.ListPrefix(), cursor, opts.PageSize)
	if err != nil {
		return nil, cursor, fmt.Errorf("list provider sessions: %w", err)
	}

	var findings []model.ReconciliationFinding
	for _, upload := range page.Uploads {
		// A young session may belong to an initiation not recorded yet.
		if s.now().Sub(upload.Initiated) < opts.MinAge {
			continue
		}
		ref, ok := parseStagingKey(upload.ObjectKey, place)
		if !ok {
			if place.Customer {
				findings = append(findings, s.unverifiable(place, upload.ObjectKey, "a multipart session"))
			}
			continue
		}
		reason, err := s.orphanSession(ctx, place, ref, upload)
		if err != nil {
			// Retry this page next time; aborting is idempotent.
			return findings, cursor, err
		}
		if reason == "" {
			continue
		}

		finding := s.newFinding(model.FindingOrphanSession, model.ActionAbortSession, ref, upload.ObjectKey, reason)
		finding.ConnectionID, finding.StorageUploadID = place.ConnectionID, upload.StorageUploadID
		if unprovable(reason, place, opts) {
			finding.Outcome = model.FindingReported
		} else if !opts.DryRun {
			err := place.Storage.AbortMultipart(ctx, place.Bucket, upload.ObjectKey, upload.StorageUploadID)
			finding.Outcome, finding.Reason = outcome(err, interfaces.ErrStorageUploadNotFound, reason)
		}
		findings = append(findings, finding)
	}
	return findings, page.Cursor, nil
}

// orphanSession returns why a provider session is an orphan, or "" when it
// is another connection's, or still owned by a live session (whose own
// reconciliation task handles it).
func (s *MultipartService) orphanSession(ctx context.Context, place ScanPlace, ref model.UploadRef, upload interfaces.StoredMultipartUpload) (string, error) {
	session, err := s.uploads.GetByID(ctx, ref.TenantID, ref.UploadID)
	if errors.Is(err, model.ErrUploadNotFound) {
		return reasonUnrecorded + "key", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up session: %w", err)
	}

	switch {
	case session.StagingConnectionID != place.ConnectionID:
		return "", nil // staged on another connection; never touched here
	case session.Status == model.UploadStatusPending && session.StorageUploadID == "":
		return "", nil // ResolveInitiation adopts or cleans it up
	case session.Status == model.UploadStatusAborting:
		return "", nil // the ABORT task aborts it
	case session.StorageUploadID != upload.StorageUploadID:
		return "a second provider session for one upload", nil
	case session.Status.IsTerminal():
		return "upload session is " + string(session.Status), nil
	}
	return "", nil // live or completed: owned
}

// ScanObjectsAt checks one page of the staging objects in a place and
// deletes those whose upload session is gone, aborted or expired. FAILED
// sessions keep their object as evidence (retention); live and completed
// ones are owned.
func (s *MultipartService) ScanObjectsAt(ctx context.Context, place ScanPlace, cursor string, opts ScanOptions) ([]model.ReconciliationFinding, string, error) {
	page, err := place.Storage.ListObjectsPage(ctx, place.Bucket, place.ListPrefix(), cursor, opts.PageSize)
	if err != nil {
		return nil, cursor, fmt.Errorf("list staging objects: %w", err)
	}

	var findings []model.ReconciliationFinding
	for _, object := range page.Objects {
		if s.now().Sub(object.LastModified) < opts.MinAge {
			continue
		}
		ref, ok := parseStagingKey(object.ObjectKey, place)
		if !ok {
			if place.Customer {
				findings = append(findings, s.unverifiable(place, object.ObjectKey, "an object"))
			}
			continue
		}

		reason := ""
		session, err := s.uploads.GetByID(ctx, ref.TenantID, ref.UploadID)
		switch {
		case errors.Is(err, model.ErrUploadNotFound):
			reason = reasonUnrecorded + "object"
		case err != nil:
			// Never delete on a lookup that failed: retry the page.
			return findings, cursor, fmt.Errorf("look up session: %w", err)
		case session.StagingConnectionID != place.ConnectionID:
			// The session stages elsewhere: this object is not its.
		case session.Status == model.UploadStatusAborted || session.Status == model.UploadStatusExpired:
			reason = "upload session is " + string(session.Status)
		}
		if reason == "" {
			continue
		}

		finding := s.newFinding(model.FindingOrphanObject, model.ActionDeleteObject, ref, object.ObjectKey, reason)
		finding.ConnectionID = place.ConnectionID
		if unprovable(reason, place, opts) {
			finding.Outcome = model.FindingReported
		} else if !opts.DryRun {
			// On a versioned bucket this removes every version of the key.
			err := place.Storage.DeleteObject(ctx, place.Bucket, object.ObjectKey)
			finding.Outcome, finding.Reason = outcome(err, interfaces.ErrObjectNotFound, reason)
		}
		findings = append(findings, finding)
	}
	return findings, page.Cursor, nil
}

// unverifiable reports an item under a tenant's staging prefix whose key
// names no upload: nothing can vouch for it, so it is left alone.
func (s *MultipartService) unverifiable(place ScanPlace, key, what string) model.ReconciliationFinding {
	f := s.newFinding(model.FindingUnverifiable, "", model.UploadRef{TenantID: place.TenantID}, key,
		what+" whose key names no upload; no record can vouch for it")
	f.ConnectionID, f.Outcome = place.ConnectionID, model.FindingReported
	return f
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

// parseStagingKey reads the place's prefix/{tenant}/{upload}, of the
// place's own tenant on a tenant connection; anything else is not an
// upload and is never touched.
func parseStagingKey(key string, place ScanPlace) (model.UploadRef, bool) {
	rest, ok := strings.CutPrefix(key, place.Prefix+"/")
	if !ok {
		return model.UploadRef{}, false
	}
	tenant, upload, ok := strings.Cut(rest, "/")
	if !ok || tenant == "" || upload == "" || strings.Contains(upload, "/") {
		return model.UploadRef{}, false
	}
	if place.TenantID != "" && tenant != place.TenantID {
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
		at, err := s.staging(ctx, session)
		if err != nil {
			// The connection is unreachable now; the next scan asks again.
			continue
		}
		ids, err := at.storage.ListMultipartUploads(ctx, at.bucket, session.ObjectKey)
		if err != nil {
			return findings, fmt.Errorf("list provider sessions for a finished upload: %w", err)
		}

		ref := model.UploadRef{TenantID: session.TenantID, UploadID: session.ID}
		for _, id := range ids {
			reason := "upload session is " + string(session.Status) + " but the provider still holds a multipart session"
			finding := s.newFinding(model.FindingOrphanSession, model.ActionAbortSession, ref, session.ObjectKey, reason)
			finding.StorageUploadID = id
			if !dryRun {
				err := at.storage.AbortMultipart(ctx, at.bucket, session.ObjectKey, id)
				finding.Outcome, finding.Reason = outcome(err, interfaces.ErrStorageUploadNotFound, reason)
			}
			findings = append(findings, finding)
		}
	}
	return findings, nil
}

// unprovable reports whether a finding is about something with no upload
// session at all: removed only from the platform's own bucket, and only
// when DeleteUnrecorded allows it.
func unprovable(reason string, place ScanPlace, opts ScanOptions) bool {
	return strings.HasPrefix(reason, reasonUnrecorded) && (place.Customer || !opts.DeleteUnrecorded)
}
