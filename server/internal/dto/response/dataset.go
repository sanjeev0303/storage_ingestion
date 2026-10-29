package response

import (
	"encoding/json"
	"time"
)

// Managed datasets (Phase 10). Responses carry identifiers, records and
// counts: never storage keys, buckets, signed read URLs or object
// content.

type DatasetPageResponse struct {
	Datasets   []DatasetResponse `json:"datasets"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type DatasetMemberResponse struct {
	ID        string            `json:"id"`
	ObjectID  string            `json:"object_id"`
	Namespace string            `json:"namespace"`
	Path      string            `json:"path"`
	VersionID string            `json:"version_id,omitempty"`
	Label     string            `json:"label,omitempty"`
	Split     string            `json:"split,omitempty"`
	Metadata  map[string]string `json:"metadata"`
	Source    string            `json:"source"`
	AddedBy   string            `json:"added_by"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type DatasetMemberPageResponse struct {
	Members    []DatasetMemberResponse `json:"members"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// MemberResultResponse is what happened to one requested member: ADDED,
// UPDATED, SKIPPED or REJECTED (with a code).
type MemberResultResponse struct {
	Index     int    `json:"index"`
	Outcome   string `json:"outcome"`
	ObjectID  string `json:"object_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
}

type AddMembersResponse struct {
	Added    int                    `json:"added"`
	Updated  int                    `json:"updated"`
	Skipped  int                    `json:"skipped"`
	Rejected int                    `json:"rejected"`
	Results  []MemberResultResponse `json:"results"`
	Dataset  DatasetResponse        `json:"dataset"`
}

type ManifestImportProgress struct {
	LinesRead      int64 `json:"lines_read"`
	MembersAdded   int64 `json:"members_added"`
	MembersUpdated int64 `json:"members_updated"`
	MembersSkipped int64 `json:"members_skipped"`
	LinesRejected  int64 `json:"lines_rejected"`
}

type ManifestImportResponse struct {
	ID              string                 `json:"id"`
	DatasetID       string                 `json:"dataset_id"`
	State           string                 `json:"state"`
	Source          string                 `json:"source"`
	DuplicatePolicy string                 `json:"duplicate_policy"`
	ManifestBytes   int64                  `json:"manifest_bytes"`
	ManifestSHA256  string                 `json:"manifest_sha256,omitempty"`
	Progress        ManifestImportProgress `json:"progress"`
	Attempts        int                    `json:"attempts"`
	CancelRequested bool                   `json:"cancel_requested"`
	LastError       string                 `json:"last_error,omitempty"`
	// UploadURL is where to PUT a staged manifest while it is awaited.
	UploadURL       string     `json:"upload_url,omitempty"`
	UploadExpiresAt *time.Time `json:"upload_expires_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

type ImportErrorResponse struct {
	Line   int64  `json:"line"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type ImportErrorPageResponse struct {
	Errors []ImportErrorResponse `json:"errors"`
	// NextAfter is the line number to ask after for the next page.
	NextAfter int64 `json:"next_after,omitempty"`
}

type DatasetVersionResponse struct {
	ID               string          `json:"id"`
	DatasetID        string          `json:"dataset_id"`
	Version          int             `json:"version"`
	Status           string          `json:"status"`
	ValidationStatus string          `json:"validation_status"`
	MemberCount      int64           `json:"member_count"`
	TotalBytes       int64           `json:"total_bytes"`
	ManifestSHA256   string          `json:"manifest_sha256"`
	ManifestFormat   string          `json:"manifest_format"`
	ProfileID        string          `json:"profile_id,omitempty"`
	ProfileVersion   int             `json:"profile_version,omitempty"`
	Config           map[string]any  `json:"config"`
	ParentVersionID  string          `json:"parent_version_id,omitempty"`
	Derivation       *DerivationInfo `json:"derived_from,omitempty"`
	LatestReportID   string          `json:"latest_report_id,omitempty"`
	CreatedBy        string          `json:"created_by"`
	CreatedAt        time.Time       `json:"created_at"`
}

type DatasetVersionPageResponse struct {
	Versions []DatasetVersionResponse `json:"versions"`
	// NextBefore is the version number to ask before for the next page.
	NextBefore int `json:"next_before,omitempty"`
}

type VersionMemberResponse struct {
	Ordinal        int64             `json:"ordinal"`
	ObjectID       string            `json:"object_id"`
	VersionID      string            `json:"version_id"`
	ObjectVersion  int               `json:"object_version"`
	Path           string            `json:"path"`
	Label          string            `json:"label,omitempty"`
	Split          string            `json:"split,omitempty"`
	SizeBytes      int64             `json:"size_bytes"`
	ContentType    string            `json:"content_type"`
	ChecksumSHA256 string            `json:"checksum_sha256,omitempty"`
	Metadata       map[string]string `json:"metadata"`
	VersionMemberAvailability
}

// The member's line goes on: Availability is AVAILABLE, DELETION_PENDING or
// ERASED; ErasedAt is when its bytes were deleted for good.
type VersionMemberAvailability struct {
	Availability string     `json:"availability"`
	ErasedAt     *time.Time `json:"erased_at,omitempty"`
}

type VersionMemberPageResponse struct {
	Members   []VersionMemberResponse `json:"members"`
	NextAfter int64                   `json:"next_after,omitempty"`
}

type ValidationProgress struct {
	Total              int64 `json:"total"`
	Processed          int64 `json:"processed"`
	Passed             int64 `json:"passed"`
	Warnings           int64 `json:"warnings"`
	Failed             int64 `json:"failed"`
	ContentExamined    int64 `json:"content_examined"`
	ContentNotExamined int64 `json:"content_not_examined"`
	BytesTotal         int64 `json:"bytes_total"`
	BytesRead          int64 `json:"bytes_read"`
}

type ValidationJobResponse struct {
	ID              string             `json:"id"`
	DatasetID       string             `json:"dataset_id"`
	VersionID       string             `json:"version_id"`
	ProfileID       string             `json:"profile_id"`
	ProfileVersion  int                `json:"profile_version"`
	Mode            string             `json:"mode"`
	SampleSize      int                `json:"sample_size,omitempty"`
	Readiness       bool               `json:"readiness"`
	State           string             `json:"state"`
	Progress        ValidationProgress `json:"progress"`
	Attempts        int                `json:"attempts"`
	MaxAttempts     int                `json:"max_attempts"`
	CancelRequested bool               `json:"cancel_requested"`
	LastError       string             `json:"last_error,omitempty"`
	ReportID        string             `json:"report_id,omitempty"`
	CreatedBy       string             `json:"created_by"`
	CreatedAt       time.Time          `json:"created_at"`
	StartedAt       *time.Time         `json:"started_at,omitempty"`
	CompletedAt     *time.Time         `json:"completed_at,omitempty"`
}

type ManifestImportPageResponse struct {
	Imports    []ManifestImportResponse `json:"imports"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

type ValidationJobPageResponse struct {
	Jobs       []ValidationJobResponse `json:"jobs"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// ValidationReportResponse is a report's summary and, when one report is
// asked for, its body (layout in docs/datasets.md).
type ValidationReportResponse struct {
	ID             string          `json:"id"`
	JobID          string          `json:"job_id"`
	DatasetID      string          `json:"dataset_id"`
	VersionID      string          `json:"version_id"`
	ProfileID      string          `json:"profile_id"`
	ProfileVersion int             `json:"profile_version"`
	Mode           string          `json:"mode"`
	Status         string          `json:"status"`
	Readiness      bool            `json:"readiness"`
	SchemaVersion  int             `json:"schema_version"`
	Summary        string          `json:"summary"`
	StartedAt      time.Time       `json:"started_at"`
	CompletedAt    time.Time       `json:"completed_at"`
	Report         json.RawMessage `json:"report,omitempty"`
}

type ValidationReportPageResponse struct {
	Reports    []ValidationReportResponse `json:"reports"`
	NextCursor string                     `json:"next_cursor,omitempty"`
}

type FindingResponse struct {
	ID          string    `json:"id"`
	RuleID      string    `json:"rule_id"`
	RuleVersion int       `json:"rule_version"`
	Severity    string    `json:"severity"`
	Code        string    `json:"code"`
	ObjectID    string    `json:"object_id,omitempty"`
	VersionID   string    `json:"version_id,omitempty"`
	Message     string    `json:"message"`
	Expected    string    `json:"expected,omitempty"`
	Observed    string    `json:"observed,omitempty"`
	Evidence    string    `json:"evidence"`
	Remediation string    `json:"remediation,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type FindingPageResponse struct {
	Findings   []FindingResponse `json:"findings"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type ValidationProfileResponse struct {
	ID          string          `json:"id"`
	Version     int             `json:"version"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	BuiltIn     bool            `json:"built_in"`
	Definition  json.RawMessage `json:"definition"`
	CreatedAt   *time.Time      `json:"created_at,omitempty"`
}

type ValidationProfileListResponse struct {
	Profiles  []ValidationProfileResponse `json:"profiles"`
	RuleTypes []string                    `json:"rule_types"`
}

type LineageRefResponse struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type LineageEdgeResponse struct {
	From      LineageRefResponse `json:"from"`
	To        LineageRefResponse `json:"to"`
	Relation  string             `json:"relation"`
	Depth     int                `json:"depth"`
	Actor     string             `json:"actor"`
	JobID     string             `json:"job_id,omitempty"`
	Details   map[string]any     `json:"details,omitempty"`
	CreatedAt time.Time          `json:"created_at"`
}

type LineageResponse struct {
	Start      LineageRefResponse    `json:"start"`
	Direction  string                `json:"direction"`
	Edges      []LineageEdgeResponse `json:"edges"`
	NextCursor string                `json:"next_cursor,omitempty"`
	// Truncated: the walk stopped at its edge limit.
	Truncated bool `json:"truncated"`
}
