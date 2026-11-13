package model

import (
	"errors"
	"time"
)

// Domain metadata and publishing (docs/domain.md).

var (
	ErrInvalidDomain    = errors.New("invalid request")
	ErrDomainNotFound   = errors.New("not found")
	ErrAlreadyPublished = errors.New("this snapshot is already published")
	ErrDOIFailed        = errors.New("the DOI could not be registered")
	ErrDOIOff           = errors.New("DOI registration is not configured")
	ErrWithdrawn        = errors.New("the publication is withdrawn")
	ErrPublishingOff    = errors.New("publishing needs CONSOLE_PUBLIC_URL, the address landing pages are served from")
)

// RecordingTopic is one channel of a recording.
type RecordingTopic struct {
	Topic        string `json:"topic"`
	Schema       string `json:"schema,omitempty"`
	Encoding     string `json:"encoding,omitempty"`
	MessageCount int64  `json:"message_count"`
}

// RecordingMeta is what an MCAP file or rosbag2 folder says it holds.
type RecordingMeta struct {
	VersionID    string           `json:"version_id"`
	TenantID     string           `json:"-"`
	AssetID      string           `json:"asset_id"`
	Path         string           `json:"path"`
	Kind         string           `json:"kind"`
	StartNS      int64            `json:"start_ns"`
	EndNS        int64            `json:"end_ns"`
	MessageCount int64            `json:"message_count"`
	Topics       []RecordingTopic `json:"topics"`
}

// Segment is a time range inside one file of a snapshot.
type Segment struct {
	ID         string         `json:"id"`
	TenantID   string         `json:"-"`
	DatasetID  string         `json:"dataset_id"`
	VersionID  string         `json:"version_id"`
	AssetID    string         `json:"asset_id"`
	Path       string         `json:"path"`
	StartNS    int64          `json:"start_ns"`
	EndNS      int64          `json:"end_ns"`
	Label      string         `json:"label"`
	Note       string         `json:"note"`
	Attributes map[string]any `json:"attributes"`
	CreatedBy  string         `json:"created_by"`
	CreatedAt  time.Time      `json:"created_at"`
}

// SegmentFilter narrows a listing; a time range keeps what overlaps it.
type SegmentFilter struct {
	Path    string
	Label   string
	FromNS  *int64
	ToNS    *int64
	AfterID string
	Limit   int
}

// Embedding metrics.
const (
	MetricCosine    = "COSINE"
	MetricDot       = "DOT"
	MetricEuclidean = "EUCLIDEAN"
)

// EmbeddingIndex is a set of vectors someone computed over a snapshot.
type EmbeddingIndex struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"-"`
	DatasetID  string    `json:"dataset_id"`
	VersionID  string    `json:"version_id"`
	Name       string    `json:"name"`
	Model      string    `json:"model"`
	Dimensions int       `json:"dimensions"`
	Metric     string    `json:"metric"`
	Vectors    int64     `json:"vectors"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// Embedding is one vector, of a whole file or of a segment.
type Embedding struct {
	Path      string    `json:"path"`
	SegmentID string    `json:"segment_id,omitempty"`
	Vector    []float32 `json:"vector"`
}

// Upstream states.
const (
	UpstreamWatching = "WATCHING"
	UpstreamChanged  = "CHANGED"
	UpstreamGone     = "GONE"
)

// UpstreamSource is the copy an import came from.
type UpstreamSource struct {
	TenantID     string     `json:"-"`
	URL          string     `json:"url"`
	AssetID      string     `json:"asset_id"`
	Path         string     `json:"path"`
	SHA256       string     `json:"sha256"`
	SizeBytes    int64      `json:"size_bytes"`
	State        string     `json:"state"`
	ETag         string     `json:"etag,omitempty"`
	LastModified string     `json:"last_modified,omitempty"`
	Note         string     `json:"note,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	ChangedAt    *time.Time `json:"changed_at,omitempty"`
	NextCheckAt  time.Time  `json:"next_check_at"`
}

// Creator is an author of a publication.
type Creator struct {
	Name        string `json:"name"`
	Affiliation string `json:"affiliation,omitempty"`
	ORCID       string `json:"orcid,omitempty"`
}

// PublicationMeta is what a publisher says about a snapshot.
type PublicationMeta struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Creators    []Creator `json:"creators"`
	Publisher   string    `json:"publisher"`
	License     string    `json:"license"`
}

// Publication is a snapshot made citable. Everything in it is public on the
// landing page, except the tenant and the person who published it; the data
// stays behind its access controls.
type Publication struct {
	PublicationMeta
	ID              string     `json:"id"`
	TenantID        string     `json:"-"`
	DatasetID       string     `json:"dataset_id"`
	VersionID       string     `json:"version_id"`
	Slug            string     `json:"slug"`
	PublicationYear int        `json:"publication_year"`
	VersionNumber   int        `json:"version_number"`
	ManifestSHA256  string     `json:"manifest_sha256"`
	MemberCount     int64      `json:"member_count"`
	TotalBytes      int64      `json:"total_bytes"`
	DOI             string     `json:"doi,omitempty"`
	DOIError        string     `json:"doi_error,omitempty"`
	WithdrawnAt     *time.Time `json:"withdrawn_at,omitempty"`
	WithdrawnReason string     `json:"withdrawn_reason,omitempty"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
}
