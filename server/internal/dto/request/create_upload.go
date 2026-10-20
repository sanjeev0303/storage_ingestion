package request

// CreateUploadRequest contains the client-supplied metadata
// required to begin a file upload.
type CreateUploadRequest struct {
	FileName       string `json:"file_name"`
	ContentType    string `json:"content_type"`
	SizeBytes      int64  `json:"size_bytes"`
	ChecksumSHA256 string `json:"checksum_sha256,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	// The catalog: the dataset namespace and logical path the upload
	// publishes to (the file name when no path is given), the version's
	// user metadata, and whether an existing object at the path is a
	// conflict rather than an earlier version.
	Dataset    string            `json:"dataset,omitempty"`
	Path       string            `json:"path,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreateOnly bool              `json:"create_only,omitempty"`
}
