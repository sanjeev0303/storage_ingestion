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
	LeaseOwner      string
	LeaseExpiresAt  *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ErrLeaseLost means the worker's claim expired or another worker took
// the task over, so the worker must not write its outcome.
var ErrLeaseLost = errors.New("reconciliation lease lost")
