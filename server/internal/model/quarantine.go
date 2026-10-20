package model

import "time"

// Why an asset is in quarantine.
const (
	QuarantineVerificationFailed  = "VERIFICATION_FAILED"
	QuarantineValidationRejected  = "VALIDATION_REJECTED"
	QuarantineMalwareDetected     = "MALWARE_DETECTED"
	QuarantineScanIncomplete      = "SCAN_INCOMPLETE"
	QuarantineProcessingExhausted = "PROCESSING_EXHAUSTED"
)

// Quarantine record states.
const (
	// QuarantineHeld: kept out of delivery until processing passes or an
	// operator acts. The asset is not AVAILABLE.
	QuarantineHeld = "HELD"
	// QuarantineReleased: a held asset passed every check on a later run.
	QuarantineReleased = "RELEASED"
	// QuarantineRejected: permanent; the asset is REJECTED and its object
	// is purged after RetainUntil.
	QuarantineRejected = "REJECTED"
	// QuarantinePurged: the quarantined object was deleted after retention.
	QuarantinePurged = "PURGED"
)

// QuarantineRecord durably records why an asset was quarantined and what
// happened since. ScanSignature is for operators only and never returned
// to API clients.
type QuarantineRecord struct {
	ID            string
	TenantID      string
	AssetID       string
	JobID         string
	UploadID      string
	ObjectKey     string
	Category      string
	Reason        string
	ScanResult    string
	ScanSignature string
	Status        string
	RetainUntil   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ResolvedAt    *time.Time
}

// ProcessingEvent is one audited processing decision.
type ProcessingEvent struct {
	ID        string
	TenantID  string
	AssetID   string
	JobID     string
	Event     string
	Detail    string
	Actor     string
	CreatedAt time.Time
}

// ContentPolicy decides which content a tenant may store. Types may be
// exact ("video/mp4") or families ("image/*").
type ContentPolicy struct {
	AllowedTypes []string
	DeniedTypes  []string
	MaxSizeBytes int64
	// UnknownTypeAction is "allow" or "reject": what to do when the bytes
	// do not reveal a recognisable type.
	UnknownTypeAction string
}
