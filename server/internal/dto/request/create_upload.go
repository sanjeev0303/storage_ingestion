package request

// CreateUploadRequest contains the client-supplied metadata
// required to begin a file upload.
type CreateUploadRequest struct {
	FileName       string `json:"file_name"`
	ContentType    string `json:"content_type"`
	SizeBytes      int64  `json:"size_bytes"`
	ChecksumSHA256 string `json:"checksum_sha256,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}
