package model

import "time"

// ProcessingStage is the next step a processing job must run. Stages run
// in this order and each one is idempotent, so a crashed or repeated job
// resumes at its recorded stage.
type ProcessingStage string

const (
	// StageVerify: object existence, key, size and SHA-256 of the bytes,
	// then content-type checks. Success moves the asset to PROCESSING.
	StageVerify ProcessingStage = "VERIFY"
	// StageScan: malware scan through the configured scanner.
	StageScan ProcessingStage = "SCAN"
	// StagePromote: copy to the delivery key, verify the copy, then make
	// the asset AVAILABLE.
	StagePromote ProcessingStage = "PROMOTE"
	// StageCleanup: delete the quarantine source after promotion.
	StageCleanup ProcessingStage = "CLEANUP"
)

// ProcessingState is the job's own lifecycle.
type ProcessingState string

const (
	ProcessingPending ProcessingState = "PENDING"
	// ProcessingSucceeded: the asset is AVAILABLE and the source is gone.
	ProcessingSucceeded ProcessingState = "SUCCEEDED"
	// ProcessingFailed: final, not retried. The asset was rejected or
	// deleted, so there is nothing left to process.
	ProcessingFailed ProcessingState = "FAILED"
	// ProcessingExhausted: retries ran out or the error was permanent. The
	// asset stays unavailable until an operator acts.
	ProcessingExhausted ProcessingState = "EXHAUSTED"
)

// Scan outcomes. Only CLEAN (or the development-only NOT_SCANNED) allows
// promotion; UNSUPPORTED, TIMEOUT and ERROR are incomplete scans and are
// never treated as clean.
const (
	ScanClean       = "CLEAN"
	ScanInfected    = "INFECTED"
	ScanUnsupported = "UNSUPPORTED"
	ScanTimeout     = "TIMEOUT"
	ScanError       = "ERROR"
	ScanNotScanned  = "NOT_SCANNED"
)

// Per-stage results recorded on the job.
const (
	StagePassed   = "PASSED"
	StageFailed   = "FAILED"
	StageRejected = "REJECTED"

	// ChecksumDeclared: compared with the checksum the client sent.
	ChecksumDeclared = "DECLARED"
	// ChecksumComputed: computed here only; it proves the bytes did not
	// change after upload, not that they match the sender's original.
	ChecksumComputed = "COMPUTED"

	PromotionPending   = "PENDING"
	PromotionPublished = "PUBLISHED"
)

// ProcessingJob is one asset's way through verification, scanning and
// promotion, with the result of every stage it has passed.
type ProcessingJob struct {
	ID             string
	TenantID       string
	AssetID        string
	UploadID       string
	Stage          ProcessingStage
	State          ProcessingState
	AttemptCount   int
	MaxAttempts    int
	NextAttemptAt  time.Time
	LastError      string
	LeaseOwner     string
	LeaseExpiresAt *time.Time

	VerificationStatus  string
	ChecksumSource      string
	ChecksumAlgorithm   string
	ChecksumVerifiedAt  *time.Time
	ValidationStatus    string
	ValidationReason    string
	DetectedContentType string
	Scanner             string
	ScanResult          string
	ScanSignature       string
	ScanError           string
	ScannedAt           *time.Time
	PromotionStatus     string
	PromotionKey        string
	PromotedAt          *time.Time

	StartedAt   *time.Time
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
