package model

import (
	"encoding/json"
	"errors"
	"time"
)

// Copy job kinds (docs/copy-engine.md).
const (
	CopyMigrate = "MIGRATE"
	CopyRestore = "RESTORE"
	CopyExport  = "EXPORT"
	CopyDrill   = "DRILL"
)

// Copy job and item states.
const (
	CopyQueued    = "QUEUED"
	CopyRunning   = "RUNNING"
	CopyCompleted = "COMPLETED"
	CopyFailed    = "FAILED"
	CopyCancelled = "CANCELLED"

	ItemPending = "PENDING"
	ItemDone    = "DONE"
	ItemFailed  = "FAILED"
)

var (
	ErrCopyJobNotFound = errors.New("copy job not found")
	ErrInvalidCopyJob  = errors.New("invalid copy job")
	// ErrEscrowRequired: the asset belongs to a dataset that keeps a
	// verified escrow copy, and it has none yet.
	ErrEscrowRequired = errors.New("the dataset requires a verified escrow copy at its replica connection before deletion")
	// ErrVaultRequirements: a locked vault's replica connection is not
	// locked, or shares an account with the primary.
	ErrVaultRequirements = errors.New("a locked vault needs a replica connection with Object Lock in a different account")
	// ErrChecksumMismatch: bytes read did not hash to the recorded SHA-256.
	ErrChecksumMismatch = errors.New("the bytes do not match the recorded SHA-256")
)

// CopyJob moves or copies a dataset's objects between connections:
// MIGRATE switches them to the target once every copy is verified;
// RESTORE copies a snapshot to a target prefix; EXPORT writes the
// organization's export-everything bundle; DRILL re-reads a sample and
// measures how long a restore would take.
type CopyJob struct {
	ID                 string          `json:"id"`
	TenantID           string          `json:"-"`
	Kind               string          `json:"kind"`
	DatasetID          string          `json:"dataset_id,omitempty"`
	VersionID          string          `json:"version_id,omitempty"`
	SourceConnectionID string          `json:"source_connection_id,omitempty"`
	TargetConnectionID string          `json:"target_connection_id,omitempty"`
	TargetPrefix       string          `json:"target_prefix,omitempty"`
	Options            json.RawMessage `json:"options"`
	State              string          `json:"state"`
	Planned            bool            `json:"planned"`
	TotalItems         int64           `json:"total_items"`
	DoneItems          int64           `json:"done_items"`
	FailedItems        int64           `json:"failed_items"`
	TotalBytes         int64           `json:"total_bytes"`
	DoneBytes          int64           `json:"done_bytes"`
	Report             json.RawMessage `json:"report"`
	Error              string          `json:"error,omitempty"`
	IdempotencyKey     string          `json:"-"`
	CreatedBy          string          `json:"created_by"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	StartedAt          *time.Time      `json:"started_at,omitempty"`
	FinishedAt         *time.Time      `json:"finished_at,omitempty"`
}

// CopyItem is one object of a copy job.
type CopyItem struct {
	JobID           string     `json:"-"`
	Ordinal         int        `json:"ordinal"`
	TenantID        string     `json:"-"`
	AssetID         string     `json:"asset_id,omitempty"`
	Path            string     `json:"path,omitempty"`
	TargetKey       string     `json:"target_key"`
	SizeBytes       int64      `json:"size_bytes"`
	ChecksumSHA256  string     `json:"checksum_sha256,omitempty"`
	State           string     `json:"state"`
	TargetVersionID string     `json:"target_version_id,omitempty"`
	TargetETag      string     `json:"-"`
	Error           string     `json:"error,omitempty"`
	DurationMS      int64      `json:"duration_ms"`
	DoneAt          *time.Time `json:"done_at,omitempty"`
}

// AssetReplica is a verified copy of an asset's bytes at a second
// connection.
type AssetReplica struct {
	TenantID       string     `json:"-"`
	AssetID        string     `json:"asset_id"`
	ConnectionID   string     `json:"connection_id"`
	ObjectKey      string     `json:"-"`
	VersionID      string     `json:"-"`
	ETag           string     `json:"-"`
	SizeBytes      int64      `json:"size_bytes"`
	ChecksumSHA256 string     `json:"checksum_sha256"`
	LockMode       string     `json:"lock_mode,omitempty"`
	LockedUntil    *time.Time `json:"locked_until,omitempty"`
	ReplicatedAt   time.Time  `json:"replicated_at"`
	VerifiedAt     time.Time  `json:"verified_at"`
}

// ScrubResult is the last re-read of an asset's bytes.
type ScrubResult struct {
	AssetID   string    `json:"asset_id"`
	Path      string    `json:"path,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail,omitempty"`
	Repaired  bool      `json:"repaired"`
}

// Protection is what a dataset asks for beyond its own connection.
type Protection struct {
	ReplicaConnectionID string
	Escrow              bool
	VaultRetentionDays  int
	ScrubDays           int
	CleanVersionID      string
}

// Security alert kinds from the copy engine.
const (
	AlertMassDeletion  = "MASS_DELETION"
	AlertScrubMismatch = "SCRUB_MISMATCH"
)
