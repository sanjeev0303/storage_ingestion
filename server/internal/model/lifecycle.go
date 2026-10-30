package model

import (
	"errors"
	"time"
)

// Lifecycle errors.
var (
	ErrDeletionNotFound = errors.New("deletion operation not found")
	// ErrNotRestorable: the asset has no deletion that can still be undone.
	ErrNotRestorable = errors.New("asset cannot be restored")
	// ErrDeletionBlocked: the asset is in use (an active processing job)
	// and cannot be deleted now.
	ErrDeletionBlocked = errors.New("asset is in use and cannot be deleted now")
	ErrHoldNotFound    = errors.New("legal hold not found")
	ErrInvalidPolicy   = errors.New("invalid retention policy")
)

// ValidDataset reports whether name is a usable dataset name: 1-64
// characters of lowercase letters, digits, '.', '_' and '-', starting with
// a letter or digit.
func ValidDataset(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// Policy and hold scopes, broadest first.
const (
	ScopeTenant  = "TENANT"
	ScopeDataset = "DATASET"
	ScopeAsset   = "ASSET"
)

// RetentionPolicy is one version of a tenant, dataset or asset policy. A
// nil setting inherits from the broader scope.
type RetentionPolicy struct {
	ID       string
	TenantID string
	Scope    string
	// ScopeKey is "" for TENANT, the dataset name or the asset ID.
	ScopeKey string
	Version  int
	// MinRetention: the asset may not be permanently deleted earlier than
	// this long after it was created.
	MinRetention *time.Duration
	// MaxAge: the asset expires (deletion is scheduled) this long after it
	// was created. Zero means it never expires.
	MaxAge *time.Duration
	// DeletionGrace: how long a soft-deleted asset stays restorable.
	DeletionGrace *time.Duration
	// QuarantineRetention: how long a rejected upload is kept for
	// investigation before it is purged.
	QuarantineRetention *time.Duration
	EffectiveFrom       time.Time
	CreatedBy           string
	Reason              string
	CreatedAt           time.Time
	// LockMode is GOVERNANCE (the default) or COMPLIANCE for the Object
	// Lock this policy sets; RetainFromKey a metadata date retention counts
	// from; Template the retention template it came from.
	LockMode      string
	RetainFromKey string
	Template      string
}

// LegalHold blocks permanent deletion of everything in its scope until it
// is released. It overrides every retention policy.
type LegalHold struct {
	ID         string
	TenantID   string
	Scope      string
	ScopeKey   string
	Reason     string
	PlacedBy   string
	PlacedAt   time.Time
	ReleasedBy string
	ReleasedAt *time.Time
	// Query selects the assets of a QUERY hold.
	Query      *HoldQuery
	Matter     string
	Custodians []string
}

// DeletionState is where a deletion operation is.
type DeletionState string

const (
	// DeletionScheduled: the asset is soft-deleted and restorable; permanent
	// deletion waits for PurgeAfter.
	DeletionScheduled DeletionState = "SCHEDULED"
	// DeletionProtected: due, but a legal hold, retention period or active
	// dependency blocks it. Checked again at NextAttemptAt.
	DeletionProtected DeletionState = "PROTECTED"
	// DeletionInProgress: objects are being removed. No longer restorable.
	DeletionInProgress DeletionState = "IN_PROGRESS"
	// DeletionRetryWait: a storage step failed and is retried at
	// NextAttemptAt.
	DeletionRetryWait DeletionState = "RETRY_WAIT"
	// DeletionStorageDeleted: every object is confirmed gone; the metadata
	// cleanup is pending.
	DeletionStorageDeleted DeletionState = "STORAGE_DELETED"
	// DeletionCompleted: objects gone and the asset tombstoned. Terminal.
	DeletionCompleted DeletionState = "COMPLETED"
	// DeletionFailed: retries ran out or the error was permanent. Terminal
	// until an operator requeues it.
	DeletionFailed DeletionState = "FAILED"
	// DeletionCancelled: the asset was restored. Terminal.
	DeletionCancelled DeletionState = "CANCELLED"
)

// IsLive reports whether the operation is still working towards deletion.
func (s DeletionState) IsLive() bool {
	return s != DeletionCompleted && s != DeletionFailed && s != DeletionCancelled
}

// Restorable reports whether undoing the deletion is still possible:
// nothing has been removed from storage yet.
func (s DeletionState) Restorable() bool {
	return s == DeletionScheduled || s == DeletionProtected
}

// Why a deletion was requested.
const (
	DeletionReasonUser     = "USER_REQUEST"
	DeletionReasonExpired  = "POLICY_EXPIRED"
	DeletionReasonOperator = "OPERATOR"
)

// DeletionOperation is the durable record of one asset deletion.
type DeletionOperation struct {
	ID          string
	TenantID    string
	AssetID     string
	State       DeletionState
	Reason      string
	RequestedBy string
	// PreviousStatus is what the asset returns to when restored.
	PreviousStatus AssetStatus
	// PolicyDetail says which policy decided an expiration.
	PolicyDetail     string
	PurgeAfter       time.Time
	ProtectedReason  string
	AttemptCount     int
	MaxAttempts      int
	NextAttemptAt    time.Time
	LastError        string
	LeaseOwner       string
	LeaseExpiresAt   *time.Time
	StorageDeletedAt *time.Time
	CompletedAt      *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// LifecycleEvent is one audited lifecycle decision.
type LifecycleEvent struct {
	ID          string
	TenantID    string
	AssetID     string
	OperationID string
	Event       string
	Outcome     string
	Reason      string
	Actor       string
	CreatedAt   time.Time
}

// Inventory scan statuses.
const (
	ScanRunning   = "RUNNING"
	ScanCompleted = "COMPLETED"
	ScanFailed    = "FAILED"
)

// What a scan lists. Scans of different kinds over one prefix run side by
// side, each with its own cursor.
const (
	// ScanObjects: the cleanup worker's object inventory.
	ScanObjects = "OBJECTS"
	// ScanUploadSessions: reconciliation's listing of provider multipart
	// sessions under a staging prefix.
	ScanUploadSessions = "UPLOAD_SESSIONS"
	// ScanUploadObjects: reconciliation's listing of staging objects,
	// for those of aborted and expired uploads.
	ScanUploadObjects = "UPLOAD_OBJECTS"
)

// InventoryScan is one pass over a bucket prefix. Cursor is the provider's
// continuation token where the scan resumes.
type InventoryScan struct {
	ID string
	// ConnectionID is the storage connection scanned ("" reads as the
	// platform's): the same bucket name on two connections is two buckets.
	ConnectionID     string
	Bucket           string
	Prefix           string
	Kind             string
	Status           string
	Cursor           string
	Pages            int
	ObjectsSeen      int64
	CandidatesMarked int
	Errors           int
	LastError        string
	StartedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
}

// Why a stored object has no owner.
const (
	// OrphanNoUploadSession: a quarantine object whose upload session does
	// not exist. Missing metadata alone never proves ownership.
	OrphanNoUploadSession = "NO_UPLOAD_SESSION"
	// OrphanNoAsset: a delivery object whose asset does not exist.
	OrphanNoAsset = "NO_ASSET"
	// OrphanDeadPromotionCopy: a delivery copy of an asset that was
	// rejected or whose processing failed.
	OrphanDeadPromotionCopy = "DEAD_PROMOTION_COPY"
	// OrphanLeftoverSource: a quarantine source that promotion finished
	// with but did not remove.
	OrphanLeftoverSource = "LEFTOVER_QUARANTINE_SOURCE"
	// OrphanExpiredFailedUpload: the object of a failed upload, kept as
	// evidence until its retention ended.
	OrphanExpiredFailedUpload = "EXPIRED_FAILED_UPLOAD"
	// OrphanDeletedAssetObject: an object of an asset that is tombstoned.
	OrphanDeletedAssetObject = "DELETED_ASSET_OBJECT"
)

// Unrecorded reports whether the category rests only on missing metadata.
// Such objects are never removed unless an operator opts in.
func Unrecorded(category string) bool {
	return category == OrphanNoUploadSession || category == OrphanNoAsset
}

// Orphan candidate statuses.
const (
	CandidateMarked  = "MARKED"
	CandidateDeleted = "DELETED"
	CandidateCleared = "CLEARED"
	CandidateFailed  = "FAILED"
)

// OrphanCandidate is a stored object seen without an owner.
type OrphanCandidate struct {
	ID           string
	ConnectionID string
	Bucket       string
	ObjectKey    string
	ETag         string
	SizeBytes    int64
	Category     string
	TenantID     string
	Reason       string
	Status       string
	Observations int
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	LastScanID   string
	AttemptCount int
	LastError    string
	ResolvedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CompactionRun records one run of one compaction rule.
type CompactionRun struct {
	ID           string
	Rule         string
	Status       string
	RowsAffected int64
	Batches      int
	Cutoff       time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	LastError    string
}

// LockSyncRequest asks the cleanup worker to apply Object Lock to every
// available asset in a scope whose retention policy or holds changed.
type LockSyncRequest struct {
	ID       string
	TenantID string
	Scope    string // ScopeTenant, ScopeDataset or ScopeAsset
	ScopeKey string
	// Position is the last asset applied; the next batch starts after it.
	Position  string
	Reason    string
	CreatedAt time.Time
}

// ObjectLock is what the platform last applied to an asset's stored
// version on a bucket with S3 Object Lock.
type ObjectLock struct {
	AssetID     string
	TenantID    string
	RetainUntil *time.Time
	LegalHold   bool
	AppliedAt   time.Time
	// Mode is the retention mode applied with RetainUntil.
	Mode string
	// VerifiedAt is the last read-back from the provider, and VerifyResult
	// MATCH or what differed.
	VerifiedAt   *time.Time
	VerifyResult string
}
