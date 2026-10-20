package model

import (
	"errors"
	"time"
)

// ReconciliationOperation names what a reconciliation task must finish.
// It is derived from the upload session's durable state.
type ReconciliationOperation string

const (
	// ReconcileInitiate: the session has no provider upload ID, so the
	// outcome of BeginMultipart is unknown.
	ReconcileInitiate ReconciliationOperation = "INITIATE"
	// ReconcileComplete: the session is COMPLETING.
	ReconcileComplete ReconciliationOperation = "COMPLETE"
	// ReconcileAbort: the session is ABORTING.
	ReconcileAbort ReconciliationOperation = "ABORT"
	// ReconcileExpire: an active session passed its expiry time.
	ReconcileExpire ReconciliationOperation = "EXPIRE"
	// ReconcileVerify: the session is COMPLETED and its checksum unverified.
	// No longer created since Phase 5.4 (the processing worker's VERIFY
	// stage owns it); still handled so tasks created earlier finish.
	ReconcileVerify ReconciliationOperation = "VERIFY"
)

// ReconciliationState is the task's own lifecycle.
type ReconciliationState string

const (
	// ReconcilePending: due at NextAttemptAt, possibly leased by a worker.
	ReconcilePending ReconciliationState = "PENDING"
	// ReconcileSucceeded: the session reached the state the task needed.
	ReconcileSucceeded ReconciliationState = "SUCCEEDED"
	// ReconcileExhausted: retries ran out or the error was permanent.
	// It stays visible for an operator and is never picked up again.
	ReconcileExhausted ReconciliationState = "EXHAUSTED"
)

// ReconciliationTask is one durable unit of recovery work.
// It never stores presigned URLs, credentials or request bodies.
type ReconciliationTask struct {
	ID              string
	TenantID        string
	UploadID        string
	Operation       ReconciliationOperation
	State           ReconciliationState
	StorageProvider string
	ObjectKey       string
	StorageUploadID string
	AttemptCount    int
	MaxAttempts     int
	LastAttemptAt   *time.Time
	NextAttemptAt   time.Time
	LastError       string
	// Outcome is the upload status the task left behind.
	Outcome        string
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ErrLeaseLost means the worker's claim expired or another worker took
// the task over, so the worker must not write its outcome.
var ErrLeaseLost = errors.New("reconciliation lease lost")

// Discrepancies the reconciliation scans look for beyond live tasks.
const (
	// FindingOrphanSession: a provider multipart session no live session owns.
	FindingOrphanSession = "ORPHAN_MULTIPART_SESSION"
	// FindingOrphanObject: a quarantine object no live session owns.
	FindingOrphanObject = "ORPHAN_OBJECT"
	// FindingStaleParts: part rows left on a finished (aborted, failed or
	// expired) session.
	FindingStaleParts = "STALE_PARTS"
)

// Actions a finding calls for.
const (
	ActionAbortSession = "ABORT_SESSION"
	ActionDeleteObject = "DELETE_OBJECT"
	ActionDeleteParts  = "DELETE_PARTS"
)

// Finding outcomes. PLANNED (a dry run) and REPORTED (left alone because
// ownership cannot be proven) are logged and counted but never stored, so
// a finding repeated every cycle does not fill the table.
const (
	FindingDone     = "DONE"
	FindingFailed   = "FAILED"
	FindingPlanned  = "PLANNED"
	FindingReported = "REPORTED"
)

// ReconciliationFinding records one discrepancy and what was done about it.
type ReconciliationFinding struct {
	ID              string
	Kind            string
	TenantID        string
	UploadID        string
	ObjectKey       string
	StorageUploadID string
	Action          string
	Outcome         string
	Reason          string
	CreatedAt       time.Time
}

// UploadRef names one tenant's upload session.
type UploadRef struct {
	TenantID string
	UploadID string
}

// DueReconciliation is a session that needs a reconciliation operation; a
// dry run reports these instead of creating tasks.
type DueReconciliation struct {
	UploadRef
	Operation ReconciliationOperation
	Status    UploadStatus
}
