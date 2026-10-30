package model

import (
	"errors"
	"time"
)

// EvidenceSettings is an organization's evidence configuration
// (docs/evidence.md).
type EvidenceSettings struct {
	TenantID           string    `json:"-"`
	AnchorConnectionID string    `json:"anchor_connection_id"`
	AnchorEveryMinutes int       `json:"anchor_every_minutes"`
	AnchorLockDays     int       `json:"anchor_lock_days"`
	ReviewEveryDays    int       `json:"review_every_days"`
	WorkStartHour      int       `json:"work_start_hour"`
	WorkEndHour        int       `json:"work_end_hour"`
	Timezone           string    `json:"timezone"`
	BulkReadThreshold  int       `json:"bulk_read_threshold"`
	RequireLawfulBasis bool      `json:"require_lawful_basis"`
	UpdatedBy          string    `json:"updated_by,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// DefaultEvidenceSettings applies until an organization saves its own.
func DefaultEvidenceSettings(tenantID string) EvidenceSettings {
	return EvidenceSettings{TenantID: tenantID, AnchorEveryMinutes: 60, AnchorLockDays: 2555, ReviewEveryDays: 7,
		WorkStartHour: 8, WorkEndHour: 19, Timezone: "UTC", BulkReadThreshold: 200}
}

// AuditAnchor is a chain head, signed and written outside the database.
type AuditAnchor struct {
	ID            string     `json:"id"`
	Chain         string     `json:"chain"`
	Seq           int64      `json:"seq"`
	Hash          string     `json:"hash"`
	Body          string     `json:"body"`
	Signature     string     `json:"signature,omitempty"`
	KeyID         string     `json:"key_id,omitempty"`
	ConnectionID  string     `json:"connection_id,omitempty"`
	ObjectKey     string     `json:"object_key,omitempty"`
	ObjectVersion string     `json:"object_version,omitempty"`
	LockedUntil   *time.Time `json:"locked_until,omitempty"`
	TSAToken      []byte     `json:"tsa_token,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// ReviewFinding is one thing a reviewer must look at.
type ReviewFinding struct {
	Kind    string    `json:"kind"` // DELETION, RETENTION_CHANGE, FAILED_HASH, AFTER_HOURS_READS, PERMISSION_CHANGE
	At      time.Time `json:"at"`
	Actor   string    `json:"actor,omitempty"`
	Subject string    `json:"subject,omitempty"`
	Detail  string    `json:"detail"`
	// AuditSeq points at the chain entry, when there is one.
	AuditSeq int64 `json:"audit_seq,omitempty"`
}

// AuditReview is a period's exceptions and the reviewer's sign-off.
type AuditReview struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"-"`
	PeriodStart  time.Time       `json:"period_start"`
	PeriodEnd    time.Time       `json:"period_end"`
	Findings     []ReviewFinding `json:"findings"`
	Counts       map[string]int  `json:"counts"`
	ReportSHA256 string          `json:"report_sha256"`
	State        string          `json:"state"` // OPEN or SIGNED
	SignedBy     string          `json:"signed_by,omitempty"`
	SignedAt     *time.Time      `json:"signed_at,omitempty"`
	Note         string          `json:"note,omitempty"`
	CreatedBy    string          `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Provenance is what a dataset's data rests on: the lawful basis for
// using it, its licence and source, recorded before it is snapshotted.
type Provenance struct {
	LawfulBasis string     `json:"lawful_basis"`
	License     string     `json:"license"`
	Source      string     `json:"source"`
	SourceKind  string     `json:"source_kind"`
	RecordedAt  *time.Time `json:"recorded_at,omitempty"`
	RecordedBy  string     `json:"recorded_by,omitempty"`
}

// Complete reports whether every field the gate needs is there.
func (p Provenance) Complete() bool {
	return p.LawfulBasis != "" && p.License != "" && p.Source != "" && p.SourceKind != ""
}

var SourceKinds = []string{"PUBLIC_DATASET", "LICENSED_PRIVATE", "WEB_CRAWLED", "USER_DATA", "SYNTHETIC", "OTHER"}

const AlertChainBroken = "AUDIT_CHAIN_BROKEN"

var (
	ErrInvalidEvidence = errors.New("invalid evidence request")
	// ErrProvenanceRequired: the organization requires a lawful basis,
	// licence and source before a snapshot.
	ErrProvenanceRequired = errors.New("record the dataset's lawful basis, licence and source before snapshotting it")
	// ErrProvenanceOrder: a run read a snapshot taken before its dataset's
	// provenance was recorded.
	ErrProvenanceOrder  = errors.New("a snapshot this run reads was taken before its dataset's lawful basis was recorded")
	ErrReviewNotFound   = errors.New("review not found")
	ErrReviewSigned     = errors.New("the review is already signed")
	ErrModelNotFound    = errors.New("model not found")
	ErrUnknownFramework = errors.New("unknown compliance framework")
)
