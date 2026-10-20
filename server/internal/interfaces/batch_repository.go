package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// IngestionRepository stores datasets, ingestion jobs, their entries and
// the tenant usage they count against.
//
// Every entry change also updates the job's counters, the tenant's usage
// and the dataset's bytes, in one transaction. Rows are locked in one order
// to rule out deadlocks: an entry first, then the tenant usage, the dataset
// and the job. Claims first take the claim lock all tenants share (the
// global slots are counted under it), then the counter rows, and pick
// entries with SKIP LOCKED, so they never wait for an entry; batch
// operations skip locked entries too.
type IngestionRepository interface {
	// CreateDataset stores d, or returns the tenant's dataset of that name
	// (created false). model.ErrDatasetConflict when its settings differ.
	CreateDataset(ctx context.Context, d model.Dataset) (dataset model.Dataset, created bool, err error)
	GetDataset(ctx context.Context, tenantID, datasetID string) (model.Dataset, error)

	// CreateJob stores job, or returns the tenant's job with the same
	// idempotency key (created false). It counts the job against the
	// tenant's active-job limit: model.ErrQuotaExceeded when that is full.
	CreateJob(ctx context.Context, job model.IngestionJob, maxActiveJobs int) (stored model.IngestionJob, created bool, err error)
	GetJob(ctx context.Context, tenantID, jobID string) (model.IngestionJob, error)
	ListJobs(ctx context.Context, tenantID, datasetID, after string, limit int) ([]model.IngestionJob, error)

	// StartManifest moves a PENDING_VALIDATION job to VALIDATING with its
	// manifest's source, writing events in the same transaction. Starting
	// again with the same source is a no-op that writes nothing.
	StartManifest(ctx context.Context, tenantID, jobID string, manifest ManifestSource, now time.Time, events ...model.OutboxEvent) (model.IngestionJob, error)
	// AppendEntries stores one batch of manifest lines and moves the
	// reading checkpoint, in one transaction: a crash leaves either the
	// whole batch and its checkpoint or neither. Lines already stored are
	// skipped; a valid line whose path is already live in the dataset is
	// stored INVALID as a duplicate. model.ErrQuotaExceeded when the
	// tenant's pending entries would pass maxPending.
	AppendEntries(ctx context.Context, job model.IngestionJob, entries []model.IngestionEntry, checkpoint Checkpoint, maxPending int64, now time.Time) (AppendResult, error)
	// CompleteManifest records that the whole manifest is read.
	CompleteManifest(ctx context.Context, jobID, sha256 string, size int64, now time.Time) error
	// FailManifest ends a job whose manifest could not be read as FAILED.
	FailManifest(ctx context.Context, jobID, reason string, now time.Time) error
	// Finalize moves a fully read job to READY, or to FAILED when no entry
	// is valid. model.ErrQuotaExceeded when its expected bytes do not fit
	// the tenant's or dataset's remaining quota.
	Finalize(ctx context.Context, tenantID, jobID string, maxStorageBytes int64, now time.Time) (model.IngestionJob, error)

	// ClaimManifests leases staged jobs whose manifest is still being
	// read and whose reader stopped, or never started; only jobID when it
	// is not empty.
	ClaimManifests(ctx context.Context, owner, jobID string, now time.Time, lease time.Duration, limit int) ([]model.IngestionJob, error)
	// ExtendManifest renews a manifest lease; ReleaseManifest gives it up.
	ExtendManifest(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) error
	ReleaseManifest(ctx context.Context, jobID, owner string) error

	// Claim grants up to req.Max of the job's PENDING entries an upload
	// slot, within every limit in req, and reserves their bytes.
	Claim(ctx context.Context, req ClaimRequest) (ClaimResult, error)
	// AttachUpload records the upload session created for an entry's
	// current generation.
	AttachUpload(ctx context.Context, jobID string, position int64, generation int, uploadID string, now time.Time) error
	// Transition moves one entry, with every counter it affects.
	// model.ErrInvalidEntryState when the entry is not in one of
	// change.From (or not on change.UploadID); the entry is returned
	// as it is.
	Transition(ctx context.Context, change EntryChange) (model.IngestionEntry, error)

	GetEntry(ctx context.Context, tenantID, jobID string, position int64) (model.IngestionEntry, error)
	EntryByUpload(ctx context.Context, uploadID string) (model.IngestionEntry, error)
	// ListEntries pages a job's entries in position order, after the
	// given position, optionally limited to some states.
	ListEntries(ctx context.Context, tenantID, jobID string, states []model.EntryState, after int64, limit int) ([]model.IngestionEntry, error)

	// RequestCancel moves a job to CANCELLING; claims grant nothing from
	// then on. A job with no open entries is CANCELLED at once.
	RequestCancel(ctx context.Context, tenantID, jobID string, now time.Time) (model.IngestionJob, error)
	// CancelWaiting cancels up to limit PENDING and FAILED_RETRYABLE
	// entries of a CANCELLING job, and returns how many.
	CancelWaiting(ctx context.Context, jobID string, limit int, now time.Time) (int, error)
	// Retry makes failed entries PENDING again: FAILED_RETRYABLE ones, and
	// FAILED ones with a retryable error and manual retries left. It
	// returns how many it moved. A finished job becomes RUNNING again.
	Retry(ctx context.Context, req RetryRequest) (int, error)

	// Worker sweeps.
	ExpiredLeases(ctx context.Context, now time.Time, limit int) ([]model.IngestionEntry, error)
	DueRetries(ctx context.Context, now time.Time, limit int) ([]model.IngestionEntry, error)
	AwaitingOutcome(ctx context.Context, before time.Time, limit int) ([]model.IngestionEntry, error)
	InFlight(ctx context.Context, jobID string, limit int) ([]model.IngestionEntry, error)
	JobsInState(ctx context.Context, state model.JobState, limit int) ([]model.IngestionJob, error)

	Quota(ctx context.Context, tenantID string) (model.TenantQuota, error)
	SetQuota(ctx context.Context, tenantID string, quota model.TenantQuota, now time.Time) error
	Usage(ctx context.Context, tenantID string) (model.TenantUsage, error)
	SlotsInUse(ctx context.Context) (int, error)
	// Recount sets a job's counters from its entries and returns them as
	// they were and as they are.
	Recount(ctx context.Context, jobID string, now time.Time) (before, after model.JobCounters, err error)
	// ReconcileUsage sets a tenant's usage from its jobs and entries, and
	// returns the usage before and after.
	ReconcileUsage(ctx context.Context, tenantID string, now time.Time) (before, after model.TenantUsage, err error)
	// TenantsWithWork lists tenants that have jobs not yet finished.
	TenantsWithWork(ctx context.Context, limit int) ([]string, error)
}

// ManifestSource describes where a job's manifest comes from.
type ManifestSource struct {
	Source    string // model.ManifestInline or model.ManifestStaged
	ObjectKey string
	ETag      string
	SizeBytes int64
	// SHA256 is the checksum the client declared, if it did; the manifest
	// read must match it.
	SHA256 string
}

// Checkpoint is how far a manifest has been read: the last line and the
// byte offset after it.
type Checkpoint struct {
	Lines int64
	Bytes int64
}

// AppendResult counts what one batch of manifest lines did.
type AppendResult struct {
	Accepted   int
	Invalid    int
	Duplicates int
	// Replayed lines were stored by an earlier submission.
	Replayed int
}

// ClaimRequest carries one claim and the limits it must respect.
type ClaimRequest struct {
	TenantID string
	JobID    string
	Max      int
	Now      time.Time
	Lease    time.Duration

	GlobalSlots     int
	TenantSlots     int
	JobSlots        int
	TenantMultipart int
	MaxStorageBytes int64
	// ProcessingBacklog: no slot is granted while this many of the
	// tenant's entries wait for processing.
	ProcessingBacklog int64
}

// What stopped a claim from granting more.
const (
	LimitGlobalSlots   = "global_slots"
	LimitTenantSlots   = "tenant_slots"
	LimitJobSlots      = "job_slots"
	LimitMultipart     = "multipart_sessions"
	LimitQuota         = "storage_quota"
	LimitBacklog       = "processing_backlog"
	LimitNothingToDo   = "no_pending_entries"
	LimitJobNotRunning = "job_not_running"
)

// ClaimResult is what a claim granted, and what held it back.
type ClaimResult struct {
	Entries []model.IngestionEntry
	// LimitedBy names the limit that stopped the claim short of Max, if
	// one did.
	LimitedBy string
}

// EntryChange is one entry transition and what it changes besides the
// state.
type EntryChange struct {
	JobID    string
	Position int64
	From     []model.EntryState
	To       model.EntryState
	Now      time.Time

	// UploadID, when set, must be the entry's current upload: an outcome
	// for an older attempt changes nothing.
	UploadID string
	AssetID  string
	// StoredBytes is the verified size of a published entry's object.
	StoredBytes int64
	ErrorCode   string
	ErrorDetail string
	// CountAttempt adds one to the entry's attempts.
	CountAttempt   bool
	NextAttemptAt  *time.Time
	LeaseExpiresAt *time.Time
	// NewGeneration starts a new upload session for the next attempt.
	NewGeneration bool
}

// RetryRequest selects failed entries to try again.
type RetryRequest struct {
	TenantID  string
	JobID     string
	Positions []int64
	// All retries every eligible failed entry, in pages of Limit.
	All              bool
	Limit            int
	MaxManualRetries int
	MaxActiveJobs    int
	Now              time.Time
}
