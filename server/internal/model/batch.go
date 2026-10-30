package model

import (
	"encoding/json"
	"errors"
	"time"
)

// Bulk ingestion: a tenant's dataset is filled by ingestion jobs, each
// driven by a manifest whose lines become entries. An entry is one file:
// it gets an upload session when a client claims it, and is published
// when its asset becomes AVAILABLE. File bytes never pass through the
// database or the API: clients upload them straight to object storage.

var (
	ErrDatasetNotFound    = errors.New("dataset not found")
	ErrDatasetConflict    = errors.New("a dataset with this name exists with different settings")
	ErrIngestionNotFound  = errors.New("ingestion job not found")
	ErrIngestionConflict  = errors.New("idempotency key reused for a different ingestion job")
	ErrEntryNotFound      = errors.New("ingestion entry not found")
	ErrInvalidJobState    = errors.New("ingestion job is not in a state that allows this")
	ErrInvalidEntryState  = errors.New("ingestion entry is not in a state that allows this")
	ErrQuotaExceeded      = errors.New("tenant ingestion quota exceeded")
	ErrManifestConflict   = errors.New("a different manifest was already submitted for this job")
	ErrManifestIncomplete = errors.New("the manifest has not been fully read yet")
)

// CompletionPolicy decides when a dataset counts as ready.
type CompletionPolicy string

const (
	// CompleteAll: every entry of every job must be published, with no
	// invalid, failed or cancelled entry.
	CompleteAll CompletionPolicy = "ALL"
	// CompleteRequired: every entry marked required must be published;
	// optional entries may fail.
	CompleteRequired CompletionPolicy = "REQUIRED"
	// CompletePartial: partial completion is accepted and reported as
	// PARTIALLY_READY.
	CompletePartial CompletionPolicy = "PARTIAL"
)

func (p CompletionPolicy) Valid() bool {
	return p == CompleteAll || p == CompleteRequired || p == CompletePartial
}

// DatasetStatus is derived from the dataset's jobs and its policy.
type DatasetStatus string

const (
	DatasetOpen           DatasetStatus = "OPEN"
	DatasetIngesting      DatasetStatus = "INGESTING"
	DatasetReady          DatasetStatus = "READY"
	DatasetPartiallyReady DatasetStatus = "PARTIALLY_READY"
	DatasetIncomplete     DatasetStatus = "INCOMPLETE"
)

// Dataset groups a tenant's ingested files. Its name is the dataset label
// uploads and assets already carry, so retention policies apply to it.
type Dataset struct {
	ID               string
	TenantID         string
	Name             string
	CompletionPolicy CompletionPolicy
	Status           DatasetStatus
	// MaxBytes caps the bytes the dataset may hold; 0 means no cap of its
	// own (the tenant quota still applies).
	MaxBytes      int64
	ReservedBytes int64
	StoredBytes   int64
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ReadyAt       *time.Time

	// The managed dataset (Phase 10): its lifecycle beside the ingestion
	// Status above, a draft membership and immutable versions.
	Description      string
	Tags             []string
	Metadata         map[string]string
	Lifecycle        DatasetLifecycle
	Revision         int64
	MembersRevision  int64
	MemberCount      int64
	NextVersion      int
	CurrentVersionID string
	ProfileID        string
	ProfileVersion   int
	LatestReportID   string
	Derivation       *Derivation
	UpdatedBy        string
	ArchivedAt       *time.Time
	// StorageConnectionID is the storage connection new uploads into the
	// dataset go to ("" for the organization's default).
	StorageConnectionID string
	// DataClass is PHI, PCI or PII when the dataset holds regulated data;
	// it limits which connections may store it (docs/storage-posture.md).
	DataClass string
	// Residency lists where the dataset's data may be stored; with
	// RequireCustomerKey only connections encrypting every write with a
	// customer-managed key take it (docs/residency.md).
	Residency          []string
	RequireCustomerKey bool
	// Protection: a replica at a second connection, escrow before
	// deletion, a locked vault, scrubs, and the clean point
	// (docs/copy-engine.md).
	ReplicaConnectionID string
	Escrow              bool
	VaultRetentionDays  int
	ScrubDays           int
	CleanVersionID      string

	// A branch (docs/datasets.md, "Branches"): its parent dataset, its
	// name there, the version it was branched from, and the common
	// ancestor of its next merge. All empty on a dataset that is not a
	// branch.
	ParentID           string
	BranchName         string
	ForkVersionID      string
	MergeBaseVersionID string
}

// DatasetSummary adds up the counters of a dataset's jobs.
type DatasetSummary struct {
	Jobs           int64
	ActiveJobs     int64
	Valid          int64
	Published      int64
	Invalid        int64
	Failed         int64
	Cancelled      int64
	RequiredFailed int64
}

// DatasetStatusOf applies a completion policy to a dataset's summary. A
// dataset is never ready while a job is still running: no entry may be
// pending, in flight or unverified when it is judged.
func DatasetStatusOf(policy CompletionPolicy, s DatasetSummary) DatasetStatus {
	switch {
	case s.Jobs == 0:
		return DatasetOpen
	case s.ActiveJobs > 0:
		return DatasetIngesting
	}
	everything := s.Valid > 0 && s.Published == s.Valid && s.Invalid == 0 && s.Failed == 0 && s.Cancelled == 0
	switch policy {
	case CompleteRequired:
		// Every required entry ended published; optional ones may not have.
		if s.RequiredFailed == 0 && s.Published > 0 {
			return DatasetReady
		}
	case CompletePartial:
		if everything {
			return DatasetReady
		}
		if s.Published > 0 {
			return DatasetPartiallyReady
		}
	default:
		if everything {
			return DatasetReady
		}
	}
	return DatasetIncomplete
}

// JobState is the lifecycle of an ingestion job.
type JobState string

const (
	JobPendingValidation  JobState = "PENDING_VALIDATION"
	JobValidating         JobState = "VALIDATING"
	JobReady              JobState = "READY"
	JobRunning            JobState = "RUNNING"
	JobPartiallyCompleted JobState = "PARTIALLY_COMPLETED"
	JobCompleted          JobState = "COMPLETED"
	JobFailed             JobState = "FAILED"
	JobCancelling         JobState = "CANCELLING"
	JobCancelled          JobState = "CANCELLED"
)

// Terminal reports whether the job has finished. A terminal job other than
// CANCELLED becomes RUNNING again when its failures are retried.
func (s JobState) Terminal() bool {
	switch s {
	case JobPartiallyCompleted, JobCompleted, JobFailed, JobCancelled:
		return true
	}
	return false
}

// Manifest sources.
const (
	ManifestInline = "INLINE"
	ManifestStaged = "STAGED"
)

// JobCounters count a job's entries by state, kept in the same transaction
// as every entry change. Reconciliation recounts them from the entries.
type JobCounters struct {
	Total      int64
	Invalid    int64
	Pending    int64
	Scheduled  int64
	Uploading  int64
	Uploaded   int64
	Processing int64
	Published  int64
	Retryable  int64
	Failed     int64
	Cancelled  int64
	// RequiredFailed counts required entries that ended invalid, failed
	// or cancelled: the REQUIRED completion policy needs it to be zero.
	RequiredFailed int64
	ExpectedBytes  int64
	StoredBytes    int64
}

// Valid is the number of entries that passed validation.
func (c JobCounters) Valid() int64 { return c.Total - c.Invalid }

// Open is the number of entries that have not reached a final state.
func (c JobCounters) Open() int64 {
	return c.Pending + c.Scheduled + c.Uploading + c.Uploaded + c.Processing + c.Retryable
}

// IngestionJob is one manifest's ingestion into a dataset.
type IngestionJob struct {
	ID             string
	TenantID       string
	DatasetID      string
	CreatedBy      string
	IdempotencyKey string
	State          JobState
	// MaxConcurrency caps this job's upload slots below the configured
	// per-job limit; 0 means the configured limit.
	MaxConcurrency int

	ManifestSource    string
	ManifestObjectKey string
	ManifestETag      string
	ManifestBytes     int64
	ManifestSHA256    string
	ManifestComplete  bool
	// ParsedLines and ParsedBytes checkpoint manifest reading: the last
	// line written and the byte offset after it.
	ParsedLines int64
	ParsedBytes int64

	Counters JobCounters
	Error    string

	CreatedAt   time.Time
	UpdatedAt   time.Time
	FinalizedAt *time.Time
	CompletedAt *time.Time
	CancelledAt *time.Time
}

// EntryState is the lifecycle of one manifest entry.
type EntryState string

const (
	// EntryInvalid: rejected by validation; never uploaded.
	EntryInvalid EntryState = "INVALID"
	// EntryPending: valid, waiting for an upload slot.
	EntryPending EntryState = "PENDING"
	// EntryScheduled: a client claimed it and holds an upload slot.
	EntryScheduled EntryState = "SCHEDULED"
	// EntryUploading: the client reported progress on its upload.
	EntryUploading EntryState = "UPLOADING"
	// EntryUploaded: the object is stored and its size confirmed;
	// processing (verification, scanning, promotion) follows.
	EntryUploaded EntryState = "UPLOADED"
	// EntryProcessing: its asset exists and is being processed.
	EntryProcessing EntryState = "PROCESSING"
	// EntryPublished: its asset is AVAILABLE.
	EntryPublished EntryState = "PUBLISHED"
	// EntryRetryable: failed in a way a later attempt can fix.
	EntryRetryable EntryState = "FAILED_RETRYABLE"
	// EntryFailed: failed for good, or out of attempts.
	EntryFailed    EntryState = "FAILED"
	EntryCancelled EntryState = "CANCELLED"
)

// HoldsSlot reports whether the entry occupies an upload slot.
func (s EntryState) HoldsSlot() bool { return s == EntryScheduled || s == EntryUploading }

// HoldsReservation reports whether the entry has bytes reserved against
// its tenant's and dataset's quota.
func (s EntryState) HoldsReservation() bool {
	return s == EntryScheduled || s == EntryUploading || s == EntryUploaded || s == EntryProcessing
}

// AwaitingProcessing reports whether the entry is stored and not yet
// published or failed: the backlog processing works through.
func (s EntryState) AwaitingProcessing() bool { return s == EntryUploaded || s == EntryProcessing }

// Open reports whether the entry still has work ahead of it.
func (s EntryState) Open() bool {
	switch s {
	case EntryPending, EntryScheduled, EntryUploading, EntryUploaded, EntryProcessing, EntryRetryable:
		return true
	}
	return false
}

// Upload strategies.
const (
	StrategySingle    = "SINGLE"
	StrategyMultipart = "MULTIPART"
)

// Entry error codes: sanitized, stable identifiers. Detail strings never
// carry URLs, credentials or file content.
const (
	EntryErrInvalidJSON        = "invalid_json"
	EntryErrLineTooLong        = "line_too_long"
	EntryErrInvalidPath        = "invalid_path"
	EntryErrDuplicatePath      = "duplicate_path"
	EntryErrInvalidSize        = "invalid_size"
	EntryErrTooLarge           = "too_large"
	EntryErrInvalidChecksum    = "invalid_checksum"
	EntryErrChecksumRequired   = "checksum_required"
	EntryErrInvalidContentType = "invalid_content_type"
	EntryErrInvalidMetadata    = "invalid_metadata"
	EntryErrLeaseExpired       = "upload_abandoned"
	EntryErrObjectMissing      = "object_missing"
	EntryErrSizeMismatch       = "size_mismatch"
	EntryErrUploadFailed       = "upload_failed"
	EntryErrStorage            = "storage_unavailable"
	EntryErrRejected           = "content_rejected"
	EntryErrProcessingHeld     = "processing_exhausted"
	EntryErrAttemptsExhausted  = "attempts_exhausted"
	EntryErrCancelled          = "cancelled"
	// EntryErrQuotaExceeded: the tenant's quota refused the upload session;
	// it is retried once space frees up.
	EntryErrQuotaExceeded = "quota_exceeded"
)

// RetryableEntryErrors are the failures a later attempt can fix.
// Validation failures and rejected content are not among them.
var RetryableEntryErrors = []string{
	EntryErrLeaseExpired, EntryErrObjectMissing, EntryErrSizeMismatch, EntryErrUploadFailed,
	EntryErrStorage, EntryErrProcessingHeld, EntryErrAttemptsExhausted, EntryErrQuotaExceeded,
}

// RetryableEntryError reports whether code is one of RetryableEntryErrors.
func RetryableEntryError(code string) bool {
	for _, c := range RetryableEntryErrors {
		if c == code {
			return true
		}
	}
	return false
}

// IngestionEntry is one manifest line. Its identity is (JobID, Position):
// the line's 1-based position in the manifest.
type IngestionEntry struct {
	JobID     string
	Position  int64
	TenantID  string
	DatasetID string
	// Path is the normalized logical path inside the dataset; PathKey is
	// what duplicates are detected on. Neither is ever a storage key.
	Path           string
	PathKey        string
	SizeBytes      int64
	ChecksumSHA256 string
	ContentType    string
	// Metadata is the entry's flat application metadata, as JSON.
	Metadata json.RawMessage
	Required bool
	Strategy string
	State    EntryState
	// UploadID is the current attempt's upload session. Generation counts
	// attempts that needed a new session; the session's idempotency key
	// includes it, so a repeated claim reuses the same session.
	UploadID         string
	UploadGeneration int
	AssetID          string
	Attempts         int
	ManualRetries    int
	NextAttemptAt    *time.Time
	LeaseExpiresAt   *time.Time
	ReservedBytes    int64
	StoredBytes      int64
	ErrorCode        string
	ErrorDetail      string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// TenantQuota is what one tenant may use. Zero fields fall back to the
// configured defaults.
type TenantQuota struct {
	MaxStorageBytes   int64
	MaxActiveJobs     int
	MaxActiveUploads  int
	MaxPendingEntries int64

	MaxAssets            int64
	MaxObjectBytes       int64
	MaxActiveMultipart   int
	MaxPendingProcessing int
	MaxManifestEntries   int64
	MaxManifestBytes     int64
	MaxRequestsPerMinute int
}

// Merge fills q's zero fields from defaults.
func (q TenantQuota) Merge(defaults TenantQuota) TenantQuota {
	pick64 := func(v, d int64) int64 {
		if v > 0 {
			return v
		}
		return d
	}
	pick := func(v, d int) int {
		if v > 0 {
			return v
		}
		return d
	}
	return TenantQuota{
		MaxStorageBytes:      pick64(q.MaxStorageBytes, defaults.MaxStorageBytes),
		MaxActiveJobs:        pick(q.MaxActiveJobs, defaults.MaxActiveJobs),
		MaxActiveUploads:     pick(q.MaxActiveUploads, defaults.MaxActiveUploads),
		MaxPendingEntries:    pick64(q.MaxPendingEntries, defaults.MaxPendingEntries),
		MaxAssets:            pick64(q.MaxAssets, defaults.MaxAssets),
		MaxObjectBytes:       pick64(q.MaxObjectBytes, defaults.MaxObjectBytes),
		MaxActiveMultipart:   pick(q.MaxActiveMultipart, defaults.MaxActiveMultipart),
		MaxPendingProcessing: pick(q.MaxPendingProcessing, defaults.MaxPendingProcessing),
		MaxManifestEntries:   pick64(q.MaxManifestEntries, defaults.MaxManifestEntries),
		MaxManifestBytes:     pick64(q.MaxManifestBytes, defaults.MaxManifestBytes),
		MaxRequestsPerMinute: pick(q.MaxRequestsPerMinute, defaults.MaxRequestsPerMinute),
	}
}

// StorageUsage is a tenant's storage usage, by category. ReservedBytes
// counts declared sizes of uploads not yet published or released;
// StoredBytes the verified sizes of AVAILABLE assets. The rest is computed
// when asked for.
type StorageUsage struct {
	TenantID             string
	ReservedBytes        int64
	ReservedUploads      int64
	StoredBytes          int64
	AssetCount           int64
	PendingDeletionBytes int64
	// Computed from current state.
	ActiveUploads            int64
	ActiveMultipart          int64
	PendingProcessing        int64
	QuarantinedBytes         int64
	IncompleteMultipartBytes int64
}

// QuotaError is a refused reservation: which limit, its value, what is in
// use, and what was asked for. It matches ErrQuotaExceeded.
type QuotaError struct {
	Limit     string
	Max       int64
	Used      int64
	Requested int64
}

func (e *QuotaError) Error() string {
	return "quota exceeded: " + e.Limit
}

func (e *QuotaError) Is(target error) bool { return target == ErrQuotaExceeded }

// Remaining is what is left under the limit.
func (e *QuotaError) Remaining() int64 { return max(e.Max-e.Used, 0) }

// TenantUsage is a tenant's ingestion usage. ReservedBytes are claimed for
// uploads in flight, judged by declared sizes; StoredBytes are published
// bytes, from the verified object sizes.
type TenantUsage struct {
	TenantID           string
	ReservedBytes      int64
	StoredBytes        int64
	ActiveJobs         int
	ActiveUploads      int
	ActiveMultipart    int
	PendingEntries     int64
	AwaitingProcessing int64
}
