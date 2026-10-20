package upload

import (
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// StartMultipartInput is created by the HTTP handler after
// validating the incoming request and authenticated identity.
type StartMultipartInput struct {
	// Dataset is optional; see model.ValidDataset.
	Dataset        string
	TenantID       string
	OwnerID        string
	FileName       string
	ContentType    string
	SizeBytes      int64
	ChecksumSHA256 string
	IdempotencyKey string
	// The catalog: the logical path (the file name when empty), the
	// version's user metadata, and create-only.
	Path       string
	Metadata   map[string]string
	CreateOnly bool
}

// MultipartResult contains application-level session information.
// It does not expose AWS SDK types.
type MultipartResult struct {
	Session model.UploadSession
	Reused  bool
}

// PartURL represents one temporary URL for a part.
type PartURL struct {
	PartNumber int32
	URL        string
	ExpiresAt  time.Time
}

// RecordPartInput contains metadata reported after a client uploads
// a part. The service validates it, but this report is not by itself
// proof that the provider accepted those exact bytes.
type RecordPartInput struct {
	PartNumber     int32
	ETag           string
	SizeBytes      int64
	ChecksumSHA256 string
}

// CompleteMultipartInput contains the ordered part list supplied
// by the client for completion.
type CompleteMultipartInput struct {
	Parts []CompletedPartInput
}

// CompletedPartInput is an application-owned completion entry.
type CompletedPartInput struct {
	PartNumber int32
	ETag       string
}
