package response

import (
	"encoding/json"
	"time"
)

type DatasetResponse struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	CompletionPolicy string `json:"completion_policy"`
	// Status is the bulk-ingestion status (Phase 7); LifecycleStatus is
	// the managed dataset's (DRAFT, VALIDATING, READY, ...).
	Status        string     `json:"status"`
	MaxBytes      int64      `json:"max_bytes"`
	ReservedBytes int64      `json:"reserved_bytes"`
	StoredBytes   int64      `json:"stored_bytes"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ReadyAt       *time.Time `json:"ready_at,omitempty"`

	Description      string            `json:"description"`
	Tags             []string          `json:"tags"`
	Metadata         map[string]string `json:"metadata"`
	LifecycleStatus  string            `json:"lifecycle_status"`
	Revision         int64             `json:"revision"`
	MemberCount      int64             `json:"member_count"`
	CurrentVersionID string            `json:"current_version_id,omitempty"`
	ProfileID        string            `json:"profile_id,omitempty"`
	ProfileVersion   int               `json:"profile_version,omitempty"`
	LatestReportID   string            `json:"latest_report_id,omitempty"`
	Derivation       *DerivationInfo   `json:"derived_from,omitempty"`
	CreatedBy        string            `json:"created_by"`
	UpdatedBy        string            `json:"updated_by,omitempty"`
	ArchivedAt       *time.Time        `json:"archived_at,omitempty"`
	// StorageConnectionID is where the dataset's new uploads go; empty for
	// the organization's default.
	StorageConnectionID string `json:"storage_connection_id,omitempty"`
}

// DerivationInfo is what a derived dataset was made from.
type DerivationInfo struct {
	VersionIDs     []string            `json:"version_ids"`
	Splits         []string            `json:"splits,omitempty"`
	Labels         []string            `json:"labels,omitempty"`
	Transformation *TransformationInfo `json:"transformation,omitempty"`
}

type TransformationInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	ConfigSHA256 string `json:"config_sha256,omitempty"`
}

// IngestionProgress counts a job's entries by state. "published" means the
// entry's asset is AVAILABLE: verified, scanned and promoted.
type IngestionProgress struct {
	Total            int64   `json:"total"`
	Valid            int64   `json:"valid"`
	Invalid          int64   `json:"invalid"`
	Pending          int64   `json:"pending"`
	Scheduled        int64   `json:"scheduled"`
	Uploading        int64   `json:"uploading"`
	Uploaded         int64   `json:"uploaded"`
	Processing       int64   `json:"processing"`
	Published        int64   `json:"published"`
	FailedRetryable  int64   `json:"failed_retryable"`
	Failed           int64   `json:"failed"`
	Cancelled        int64   `json:"cancelled"`
	RequiredFailed   int64   `json:"required_failed"`
	ExpectedBytes    int64   `json:"expected_bytes"`
	StoredBytes      int64   `json:"stored_bytes"`
	PercentPublished float64 `json:"percent_published"`
}

type IngestionJobResponse struct {
	ID               string            `json:"id"`
	DatasetID        string            `json:"dataset_id"`
	State            string            `json:"state"`
	MaxConcurrency   int               `json:"max_concurrency"`
	ManifestSource   string            `json:"manifest_source,omitempty"`
	ManifestBytes    int64             `json:"manifest_bytes"`
	ManifestSHA256   string            `json:"manifest_sha256,omitempty"`
	ManifestComplete bool              `json:"manifest_complete"`
	ParsedLines      int64             `json:"parsed_lines"`
	Progress         IngestionProgress `json:"progress"`
	Error            string            `json:"error,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	FinalizedAt      *time.Time        `json:"finalized_at,omitempty"`
	CompletedAt      *time.Time        `json:"completed_at,omitempty"`
	CancelledAt      *time.Time        `json:"cancelled_at,omitempty"`
}

type IngestionEntryResponse struct {
	Position       int64           `json:"position"`
	Path           string          `json:"path,omitempty"`
	SizeBytes      int64           `json:"size_bytes"`
	ChecksumSHA256 string          `json:"checksum_sha256,omitempty"`
	ContentType    string          `json:"content_type,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	Required       bool            `json:"required"`
	Strategy       string          `json:"strategy,omitempty"`
	State          string          `json:"state"`
	UploadID       string          `json:"upload_id,omitempty"`
	AssetID        string          `json:"asset_id,omitempty"`
	Attempts       int             `json:"attempts"`
	ErrorCode      string          `json:"error_code,omitempty"`
	ErrorDetail    string          `json:"error_detail,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at,omitempty"`
	NextAttemptAt  *time.Time      `json:"next_attempt_at,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// EntryPageResponse is one page of entries. NextAfter, when set, is the
// "after" value of the next page.
type EntryPageResponse struct {
	Entries   []IngestionEntryResponse `json:"entries"`
	NextAfter *int64                   `json:"next_after,omitempty"`
}

type JobPageResponse struct {
	Jobs      []IngestionJobResponse `json:"jobs"`
	NextAfter string                 `json:"next_after,omitempty"`
}

type GrantResponse struct {
	Entry        IngestionEntryResponse `json:"entry"`
	UploadURL    string                 `json:"upload_url,omitempty"`
	URLExpiresAt *time.Time             `json:"url_expires_at,omitempty"`
}

type ClaimResponse struct {
	Grants            []GrantResponse `json:"grants"`
	LimitedBy         string          `json:"limited_by,omitempty"`
	RetryAfterSeconds int             `json:"retry_after_seconds,omitempty"`
	Failed            int             `json:"failed,omitempty"`
}

type ManifestReportResponse struct {
	Lines      int64                    `json:"lines"`
	Accepted   int                      `json:"accepted"`
	Invalid    int                      `json:"invalid"`
	Duplicates int                      `json:"duplicates"`
	Replayed   int                      `json:"replayed"`
	Complete   bool                     `json:"complete"`
	SHA256     string                   `json:"sha256,omitempty"`
	Samples    []IngestionEntryResponse `json:"invalid_samples,omitempty"`
}

type StagedManifestResponse struct {
	UploadURL string    `json:"upload_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type RetryResponse struct {
	Retried  int `json:"retried"`
	Requeued int `json:"requeued"`
}
