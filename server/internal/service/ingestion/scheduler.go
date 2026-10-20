package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// Scheduling is pull-based. Clients upload file bytes themselves, straight
// to object storage, so the server schedules uploads by granting upload
// slots: a client claims entries, and each grant carries what it needs to
// upload one file. The limits are enforced in one database transaction per
// claim (see IngestionRepository.Claim): global, per-tenant and per-job
// slots with fair shares under contention, multipart sessions, the storage
// quota and the processing backlog. Nothing is queued in memory: the
// PENDING entries in the database are the queue.

// Grant is one entry a client may upload now.
type Grant struct {
	Entry model.IngestionEntry
	// UploadURL is the presigned PUT of a SINGLE entry. A MULTIPART entry
	// asks for part URLs with its upload ID.
	UploadURL    string
	URLExpiresAt time.Time
}

// ClaimResult is what one claim granted.
type ClaimResult struct {
	Grants []Grant
	// LimitedBy names what stopped the claim short, if anything did, and
	// RetryAfter suggests when to claim again.
	LimitedBy  string
	RetryAfter time.Duration
	// Failed entries were granted but their upload could not be prepared;
	// they are retried later.
	Failed int
}

// Claim grants up to max of the job's pending entries an upload slot.
func (s *Service) Claim(ctx context.Context, p model.Principal, jobID string, max int) (ClaimResult, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.claim", "ingestion_job", jobID); err != nil {
		return ClaimResult{}, err
	}
	if max <= 0 || max > s.cfg.MaxClaim {
		max = s.cfg.MaxClaim
	}
	job, err := s.deps.Repo.GetJob(ctx, p.TenantID, jobID)
	if err != nil {
		return ClaimResult{}, err
	}
	dataset, err := s.deps.Repo.GetDataset(ctx, p.TenantID, job.DatasetID)
	if err != nil {
		return ClaimResult{}, err
	}
	q, err := s.quota(ctx, p.TenantID)
	if err != nil {
		return ClaimResult{}, err
	}
	granted, err := s.deps.Repo.Claim(ctx, interfaces.ClaimRequest{
		TenantID: p.TenantID, JobID: jobID, Max: max, Now: s.now(), Lease: s.cfg.SlotLease,
		GlobalSlots: s.cfg.GlobalUploadSlots, TenantSlots: q.MaxActiveUploads, JobSlots: s.cfg.JobUploadSlots,
		TenantMultipart: s.cfg.TenantMultipartSessions, MaxStorageBytes: q.MaxStorageBytes,
		ProcessingBacklog: s.cfg.TenantProcessingBacklog,
	})
	if err != nil {
		return ClaimResult{}, err
	}
	s.count("ingestion_claims", 1)
	s.count("ingestion_slots_granted", int64(len(granted.Entries)))
	result := ClaimResult{LimitedBy: granted.LimitedBy}
	if granted.LimitedBy != "" {
		s.count("ingestion_claims_limited_"+granted.LimitedBy, 1)
		if len(granted.Entries) == 0 && granted.LimitedBy != interfaces.LimitNothingToDo && granted.LimitedBy != interfaces.LimitJobNotRunning {
			result.RetryAfter = 5 * time.Second
		}
	}

	// Each session is a database write and a presign: prepare a few at a
	// time rather than one after another, since a claim may grant up to
	// MaxClaim entries.
	grants := make([]Grant, len(granted.Entries))
	failures := make([]error, len(granted.Entries))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(len(granted.Entries), prepareWorkers); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				grants[i], failures[i] = s.prepare(ctx, job, dataset, granted.Entries[i])
			}
		}()
	}
	for i := range granted.Entries {
		next <- i
	}
	close(next)
	wg.Wait()

	// The grants are committed: give back the slots of those that could not
	// be prepared even if the client has gone.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.StorageTimeout)
	defer cancel()
	for i, e := range granted.Entries {
		if failures[i] != nil {
			result.Failed++
			s.uploadFailed(recordCtx, e, failures[i])
			continue
		}
		result.Grants = append(result.Grants, grants[i])
	}
	return result, nil
}

// prepareWorkers is how many upload sessions one claim prepares at once.
const prepareWorkers = 8

// uploadKey is the idempotency key of an entry's upload session: the same
// for every claim of one attempt, new for an attempt that needs a new
// session.
func uploadKey(e model.IngestionEntry) string {
	return fmt.Sprintf("ingestion:%s:%d:%d", e.JobID, e.Position, e.UploadGeneration)
}

// prepare creates (or finds again) the upload session of a granted entry
// and attaches it. Calling it again for the same attempt returns the same
// session: its idempotency key does not change.
func (s *Service) prepare(ctx context.Context, job model.IngestionJob, dataset model.Dataset, e model.IngestionEntry) (Grant, error) {
	grant := Grant{Entry: e}
	err := s.onStorage(ctx, func(ctx context.Context) error {
		switch e.Strategy {
		case model.StrategySingle:
			res, err := s.deps.Uploads.Create(ctx, upload.CreateInput{
				TenantID: e.TenantID, OwnerID: job.CreatedBy, Dataset: dataset.Name,
				Request: request.CreateUploadRequest{
					FileName: model.BaseName(e.Path), ContentType: e.ContentType, SizeBytes: e.SizeBytes,
					ChecksumSHA256: e.ChecksumSHA256, IdempotencyKey: uploadKey(e),
					Path: e.Path, Metadata: entryMetadata(e),
				},
			})
			if err != nil {
				return err
			}
			grant.Entry.UploadID = res.Upload.ID
			grant.UploadURL, grant.URLExpiresAt = res.Plan.UploadURL, res.Plan.ExpiresAt
		case model.StrategyMultipart:
			res, err := s.deps.Multipart.Start(ctx, upload.StartMultipartInput{
				Dataset: dataset.Name, TenantID: e.TenantID, OwnerID: job.CreatedBy, FileName: model.BaseName(e.Path),
				ContentType: e.ContentType, SizeBytes: e.SizeBytes, ChecksumSHA256: e.ChecksumSHA256,
				IdempotencyKey: uploadKey(e), Path: e.Path, Metadata: entryMetadata(e),
			})
			if err != nil {
				return err
			}
			grant.Entry.UploadID = res.Session.ID
		default:
			return fmt.Errorf("unknown strategy %q", e.Strategy)
		}
		return nil
	})
	if err != nil {
		return Grant{}, err
	}
	if e.UploadID == "" {
		if err := s.deps.Repo.AttachUpload(ctx, e.JobID, e.Position, e.UploadGeneration, grant.Entry.UploadID, s.now()); err != nil {
			return Grant{}, err
		}
	}
	if e.Strategy == model.StrategySingle {
		s.count("ingestion_uploads_single", 1)
	} else {
		s.count("ingestion_uploads_multipart", 1)
	}
	return grant, nil
}

// uploadFailed records an entry whose upload could not be started. A
// request the upload service refuses cannot succeed by retrying; anything
// else (storage, database) is retried with a new session.
func (s *Service) uploadFailed(ctx context.Context, e model.IngestionEntry, cause error) {
	s.count("ingestion_prepare_failures", 1)
	code, permanent := model.EntryErrStorage, false
	switch {
	case errors.Is(cause, upload.ErrUploadTooLarge):
		code, permanent = model.EntryErrTooLarge, true
	case errors.Is(cause, upload.ErrInvalidRequest), errors.Is(cause, upload.ErrIdempotencyConflict):
		code, permanent = model.EntryErrUploadFailed, true
	case errors.Is(cause, model.ErrQuotaExceeded):
		code = model.EntryErrQuotaExceeded
	}
	s.fail(ctx, e, []model.EntryState{model.EntryScheduled, model.EntryUploading}, code, retry.Sanitize(cause), permanent)
}

// fail moves an entry to FAILED_RETRYABLE with a backoff, or to FAILED
// when the failure is permanent or the attempts are spent. Either way the
// entry's slot and reservation are released, and the next attempt gets a
// new upload session.
func (s *Service) fail(ctx context.Context, e model.IngestionEntry, from []model.EntryState, code, detail string, permanent bool) {
	now := s.now()
	change := interfaces.EntryChange{
		JobID: e.JobID, Position: e.Position, From: from, Now: now, UploadID: e.UploadID,
		ErrorCode: code, ErrorDetail: detail, CountAttempt: true, NewGeneration: true,
	}
	switch {
	case permanent:
		change.To = model.EntryFailed
	case e.Attempts+1 >= s.cfg.MaxAttempts:
		change.To, change.ErrorCode = model.EntryFailed, model.EntryErrAttemptsExhausted
		change.ErrorDetail = fmt.Sprintf("%d attempts; last: %s", e.Attempts+1, detail)
	default:
		change.To = model.EntryRetryable
		next := now.Add(s.backoff(e.Attempts + 1))
		change.NextAttemptAt = &next
	}
	if _, err := s.deps.Repo.Transition(ctx, change); err != nil && !errors.Is(err, model.ErrInvalidEntryState) {
		s.deps.Log.Warn("record failed entry", "job_id", e.JobID, "position", e.Position, "error", retry.Sanitize(err))
		return
	}
	s.count("ingestion_entries_failed", 1)
}

func (s *Service) backoff(attempt int) time.Duration {
	return retry.Policy{BaseDelay: s.cfg.RetryBackoffBase, MaxDelay: s.cfg.RetryBackoffMax, MaxAttempts: s.cfg.MaxAttempts}.Delay(attempt)
}

// entryInFlight loads the caller's entry and checks that it still holds an
// upload slot in a job that is not being cancelled.
func (s *Service) entryInFlight(ctx context.Context, p model.Principal, jobID string, position int64) (model.IngestionJob, model.IngestionEntry, error) {
	job, err := s.deps.Repo.GetJob(ctx, p.TenantID, jobID)
	if err != nil {
		return job, model.IngestionEntry{}, err
	}
	e, err := s.deps.Repo.GetEntry(ctx, p.TenantID, jobID, position)
	if err != nil {
		return job, e, err
	}
	if job.State == model.JobCancelling || job.State == model.JobCancelled {
		return job, e, fmt.Errorf("%w: the job is cancelled", model.ErrInvalidJobState)
	}
	if !e.State.HoldsSlot() {
		return job, e, fmt.Errorf("%w: the entry is %s", model.ErrInvalidEntryState, e.State)
	}
	return job, e, nil
}

// RefreshUpload gives a client a fresh grant for an entry it holds: a new
// presigned PUT when the first one expired, and the upload session itself
// when the claim's response was lost before the session existed.
func (s *Service) RefreshUpload(ctx context.Context, p model.Principal, jobID string, position int64) (Grant, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.upload", "ingestion_job", jobID); err != nil {
		return Grant{}, err
	}
	job, e, err := s.entryInFlight(ctx, p, jobID, position)
	if err != nil {
		return Grant{}, err
	}
	if e.UploadID == "" {
		dataset, err := s.deps.Repo.GetDataset(ctx, p.TenantID, job.DatasetID)
		if err != nil {
			return Grant{}, err
		}
		return s.prepare(ctx, job, dataset, e)
	}
	grant := Grant{Entry: e}
	if e.Strategy == model.StrategySingle {
		err := s.onStorage(ctx, func(ctx context.Context) error {
			res, err := s.deps.Uploads.Plan(ctx, e.TenantID, e.UploadID)
			grant.UploadURL, grant.URLExpiresAt = res.Plan.UploadURL, res.Plan.ExpiresAt
			return err
		})
		if err != nil {
			return Grant{}, err
		}
	}
	return grant, nil
}

// PartURLs presigns parts of a MULTIPART entry's upload, at most
// MaxPartURLs per request.
func (s *Service) PartURLs(ctx context.Context, p model.Principal, jobID string, position int64, partNumbers []int32) ([]upload.PartURL, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.upload", "ingestion_job", jobID); err != nil {
		return nil, err
	}
	if len(partNumbers) == 0 || len(partNumbers) > s.cfg.MaxPartURLs {
		return nil, fmt.Errorf("%w: ask for 1 to %d parts at a time", ErrInvalidInput, s.cfg.MaxPartURLs)
	}
	job, e, err := s.multipartEntry(ctx, p, jobID, position)
	if err != nil {
		return nil, err
	}
	var urls []upload.PartURL
	err = s.onStorage(ctx, func(ctx context.Context) error {
		var err error
		urls, err = s.deps.Multipart.PresignParts(ctx, e.TenantID, job.CreatedBy, e.UploadID, partNumbers)
		return err
	})
	return urls, err
}

// ConfirmParts records parts the client uploaded, checked against the
// provider by the multipart service.
func (s *Service) ConfirmParts(ctx context.Context, p model.Principal, jobID string, position int64, parts []upload.RecordPartInput) error {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.upload", "ingestion_job", jobID); err != nil {
		return err
	}
	if len(parts) == 0 || len(parts) > s.cfg.MaxPartURLs {
		return fmt.Errorf("%w: confirm 1 to %d parts at a time", ErrInvalidInput, s.cfg.MaxPartURLs)
	}
	job, e, err := s.multipartEntry(ctx, p, jobID, position)
	if err != nil {
		return err
	}
	return s.onStorage(ctx, func(ctx context.Context) error {
		return s.deps.Multipart.RecordParts(ctx, e.TenantID, job.CreatedBy, e.UploadID, parts)
	})
}

func (s *Service) multipartEntry(ctx context.Context, p model.Principal, jobID string, position int64) (model.IngestionJob, model.IngestionEntry, error) {
	job, e, err := s.entryInFlight(ctx, p, jobID, position)
	if err != nil {
		return job, e, err
	}
	if e.Strategy != model.StrategyMultipart {
		return job, e, ErrStrategyMismatch
	}
	if e.UploadID == "" {
		return job, e, ErrUploadNotAttached
	}
	return job, e, nil
}

// Heartbeat tells the server the client is still uploading an entry: its
// lease is renewed, and it shows as UPLOADING.
func (s *Service) Heartbeat(ctx context.Context, p model.Principal, jobID string, position int64) (model.IngestionEntry, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.upload", "ingestion_job", jobID); err != nil {
		return model.IngestionEntry{}, err
	}
	_, e, err := s.entryInFlight(ctx, p, jobID, position)
	if err != nil {
		return e, err
	}
	lease := s.now().Add(s.cfg.SlotLease)
	return s.deps.Repo.Transition(ctx, interfaces.EntryChange{
		JobID: jobID, Position: position, From: []model.EntryState{model.EntryScheduled, model.EntryUploading},
		To: model.EntryUploading, Now: s.now(), UploadID: e.UploadID, LeaseExpiresAt: &lease,
	})
}

// CompleteEntry confirms an entry's upload. A SINGLE entry's object must
// exist with its declared size; a MULTIPART entry is completed from the
// parts listed. The entry then becomes UPLOADED, releases its slot, and
// processing takes over. Completing again is harmless.
func (s *Service) CompleteEntry(ctx context.Context, p model.Principal, jobID string, position int64, parts []upload.CompletedPartInput) (model.IngestionEntry, error) {
	if err := s.authorize(ctx, p, model.PermIngestionWrite, "ingestion.upload", "ingestion_job", jobID); err != nil {
		return model.IngestionEntry{}, err
	}
	e, err := s.deps.Repo.GetEntry(ctx, p.TenantID, jobID, position)
	if err != nil {
		return e, err
	}
	switch e.State {
	case model.EntryUploaded, model.EntryProcessing, model.EntryPublished:
		return e, nil
	}
	job, e, err := s.entryInFlight(ctx, p, jobID, position)
	if err != nil {
		return e, err
	}
	if e.UploadID == "" {
		return e, ErrUploadNotAttached
	}

	var status model.UploadStatus
	err = s.onStorage(ctx, func(ctx context.Context) error {
		var err error
		if e.Strategy == model.StrategySingle {
			status, err = s.deps.Uploads.Complete(ctx, e.TenantID, e.UploadID)
			return err
		}
		var session model.UploadSession
		session, err = s.deps.Multipart.Complete(ctx, e.TenantID, job.CreatedBy, e.UploadID, upload.CompleteMultipartInput{Parts: parts})
		status = session.Status
		return err
	})
	// From here the upload's outcome is settled at the provider and in its
	// session: record it even if the client has gone, or the entry would
	// hold its slot until its lease runs out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.StorageTimeout)
	defer cancel()
	switch {
	case errors.Is(err, upload.ErrObjectSizeMismatch), errors.Is(err, upload.ErrPartSizeMismatch):
		// The client uploaded other bytes than it declared: not retried by
		// itself, but a manual retry may re-upload.
		s.fail(ctx, e, []model.EntryState{model.EntryScheduled, model.EntryUploading}, model.EntryErrSizeMismatch,
			"the stored object's size differs from the manifest", true)
		return s.deps.Repo.GetEntry(ctx, p.TenantID, jobID, position)
	case errors.Is(err, upload.ErrOperationPending):
		return e, ErrCompletionPending
	case err != nil:
		return e, err
	}

	switch status {
	case model.UploadStatusCompleted, model.UploadStatusVerified:
		return s.uploaded(ctx, e)
	case model.UploadStatusCompleting:
		return e, ErrCompletionPending
	}
	s.fail(ctx, e, []model.EntryState{model.EntryScheduled, model.EntryUploading}, model.EntryErrUploadFailed,
		"the upload ended "+string(status), false)
	return s.deps.Repo.GetEntry(ctx, p.TenantID, jobID, position)
}

// uploaded records that an entry's object is stored and confirmed.
func (s *Service) uploaded(ctx context.Context, e model.IngestionEntry) (model.IngestionEntry, error) {
	moved, err := s.deps.Repo.Transition(ctx, interfaces.EntryChange{
		JobID: e.JobID, Position: e.Position, From: []model.EntryState{model.EntryScheduled, model.EntryUploading},
		To: model.EntryUploaded, Now: s.now(), UploadID: e.UploadID,
	})
	if errors.Is(err, model.ErrInvalidEntryState) && (moved.State.AwaitingProcessing() || moved.State == model.EntryPublished) {
		return moved, nil // a concurrent completion got there first
	}
	if err != nil {
		return moved, err
	}
	s.count("ingestion_entries_uploaded", 1)
	s.count("ingestion_bytes_uploaded", e.SizeBytes)
	return moved, nil
}

// entryMetadata is an entry's manifest metadata, which the manifest reader
// has already checked to be a flat object of strings; it becomes the user
// metadata of the published version.
func entryMetadata(e model.IngestionEntry) map[string]string {
	if len(e.Metadata) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(e.Metadata, &m); err != nil {
		return nil
	}
	return m
}
