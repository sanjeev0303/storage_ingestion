package model

import (
	"errors"
	"time"
)

// Erasure, withdrawal and destroy-by-provenance (docs/erasure.md).

var (
	ErrErasureNotFound   = errors.New("erasure request not found")
	ErrErasureState      = errors.New("the erasure request is not in a state that allows this")
	ErrErasureApproval   = errors.New("the erasure needs a second member's approval first")
	ErrVersionSuperseded = errors.New("the version was superseded by an erasure")
	ErrInvalidErasure    = errors.New("invalid erasure request")
	ErrNothingToErase    = errors.New("the erasure matches nothing")
)

// Erasure kinds and states.
const (
	ErasureSubjectWithdrawal = "SUBJECT_WITHDRAWAL"
	ErasureCopyright         = "COPYRIGHT"
	ErasurePII               = "PII"
	ErasureConsentExpired    = "CONSENT_EXPIRED"
	ErasureOther             = "OTHER"

	ErasurePlanned   = "PLANNED"
	ErasureApproved  = "APPROVED"
	ErasureDeleting  = "DELETING"
	ErasureWaiting   = "WAITING_ON_HOLD"
	ErasureCompleted = "COMPLETED"
	ErasureCancelled = "CANCELLED"
	ErasureFailed    = "FAILED"

	ErasureItemPending   = "PENDING"
	ErasureItemDeleting  = "DELETING"
	ErasureItemHeld      = "HELD"
	ErasureItemDestroyed = "DESTROYED"
	ErasureItemFailed    = "FAILED"
)

var ErasureKinds = []string{ErasureSubjectWithdrawal, ErasureCopyright, ErasurePII, ErasureConsentExpired, ErasureOther}

// DefaultSubjectKey is the metadata key that names a member's subject.
const DefaultSubjectKey = "subject_id"

// ErasureSelector says what to erase: subjects, content hashes, objects.
type ErasureSelector struct {
	SubjectIDs []string `json:"subject_ids"`
	SHA256s    []string `json:"sha256s"`
	ObjectIDs  []string `json:"object_ids"`
	SubjectKey string   `json:"subject_key"`
}

// PlanObject is one stored object an erasure deletes.
type PlanObject struct {
	AssetID      string   `json:"asset_id"`
	ObjectID     string   `json:"object_id"`
	Version      int      `json:"version"`
	Path         string   `json:"path"`
	SHA256       string   `json:"sha256"`
	SizeBytes    int64    `json:"size_bytes"`
	ConnectionID string   `json:"connection_id"`
	Status       string   `json:"status"`
	HeldBy       []string `json:"held_by"`
}

// PlanVersion is a snapshot that pins erased content.
type PlanVersion struct {
	DatasetID    string `json:"dataset_id"`
	DatasetName  string `json:"dataset_name"`
	VersionID    string `json:"version_id"`
	Version      int    `json:"version"`
	Members      int64  `json:"members"`
	Current      bool   `json:"current"`
	SupersededBy string `json:"superseded_by,omitempty"`
}

// PlanDraft is a dataset whose draft holds erased content.
type PlanDraft struct {
	DatasetID   string `json:"dataset_id"`
	DatasetName string `json:"dataset_name"`
	Members     int64  `json:"members"`
}

// PlanModel is a model trained on a snapshot that pins erased content.
type PlanModel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	RunID     string `json:"run_id"`
	RunName   string `json:"run_name"`
	VersionID string `json:"version_id"`
}

// ErasurePlan is everything an erasure reaches.
type ErasurePlan struct {
	Objects     []PlanObject  `json:"objects"`
	Bytes       int64         `json:"bytes"`
	Connections []string      `json:"connections"`
	Versions    []PlanVersion `json:"versions"`
	Drafts      []PlanDraft   `json:"drafts"`
	Models      []PlanModel   `json:"models"`
	// Matched lists every asset whose membership is removed, deleted ones
	// included (an erased member still leaves its snapshots).
	Matched     []string `json:"-"`
	NotSearched []string `json:"not_searched"`
}

// Successor is a snapshot made without the erased members.
type Successor struct {
	DatasetID        string `json:"dataset_id"`
	VersionID        string `json:"version_id"`
	Version          int    `json:"version"`
	SuccessorID      string `json:"successor_id"`
	SuccessorVersion int    `json:"successor_version"`
	Removed          int64  `json:"removed"`
}

// ErasureItem is one file an erasure deletes, and how that went.
type ErasureItem struct {
	AssetID       string     `json:"asset_id"`
	ObjectID      string     `json:"object_id"`
	ObjectVersion int        `json:"object_version"`
	Path          string     `json:"path"`
	SHA256        string     `json:"sha256"`
	SizeBytes     int64      `json:"size_bytes"`
	ConnectionID  string     `json:"connection_id"`
	DeletionID    string     `json:"deletion_id"`
	State         string     `json:"state"`
	Reason        string     `json:"reason"`
	CertificateID string     `json:"certificate_id"`
	DestroyedAt   *time.Time `json:"destroyed_at,omitempty"`
}

// ErasureProgress counts items by state.
type ErasureProgress struct {
	Total     int `json:"total"`
	Destroyed int `json:"destroyed"`
	Held      int `json:"held"`
	Failed    int `json:"failed"`
	Pending   int `json:"pending"`
}

// Erasure is an erasure request.
type Erasure struct {
	ID            string          `json:"id"`
	TenantID      string          `json:"-"`
	Kind          string          `json:"kind"`
	State         string          `json:"state"`
	Reason        string          `json:"reason"`
	Reference     string          `json:"reference"`
	Institution   string          `json:"institution"`
	Selector      ErasureSelector `json:"selector"`
	SubjectHashes []string        `json:"subject_hashes"`
	Plan          ErasurePlan     `json:"plan"`
	Successors    []Successor     `json:"successors"`
	Items         []ErasureItem   `json:"items,omitempty"`
	Progress      ErasureProgress `json:"progress"`
	RequestedBy   string          `json:"requested_by"`
	ApprovedBy    string          `json:"approved_by,omitempty"`
	ApprovedNote  string          `json:"approved_note,omitempty"`
	ApprovedAt    *time.Time      `json:"approved_at,omitempty"`
	ExecutedBy    string          `json:"executed_by,omitempty"`
	ExecutedAt    *time.Time      `json:"executed_at,omitempty"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
	Note          string          `json:"note,omitempty"`
	Certified     bool            `json:"certified"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// ErasureCertificate is the signed certificate of destruction.
type ErasureCertificate struct {
	Body       string `json:"body"`
	BodySHA256 string `json:"body_sha256"`
	Signature  string `json:"signature"`
	KeyID      string `json:"key_id"`
	PublicKey  string `json:"public_key"`
}
