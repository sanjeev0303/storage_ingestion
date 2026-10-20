package request

type StartMultipartUploadRequest struct {
	FileName       string `json:"file_name"`
	ContentType    string `json:"content_type"`
	SizeBytes      int64  `json:"size_bytes"`
	ChecksumSHA256 string `json:"checksum_sha256"`
	IdempotencyKey string `json:"idempotency_key"`
	// Dataset optionally groups the upload for retention policies.
	Dataset string `json:"dataset,omitempty"`
	// The catalog, as in CreateUploadRequest.
	Path       string            `json:"path,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreateOnly bool              `json:"create_only,omitempty"`
	// StorageConnectionID, as in CreateUploadRequest.
	StorageConnectionID string `json:"storage_connection_id,omitempty"`
}

type PresignPartsRequest struct {
	PartNumbers []int32 `json:"part_numbers"`
}

type RecordPartRequest struct {
	PartNumber     int32  `json:"part_number"`
	ETag           string `json:"etag"`
	SizeBytes      int64  `json:"size_bytes"`
	ChecksumSHA256 string `json:"checksum_sha256,omitempty"`
}

type RecordPartsRequest struct {
	Parts []RecordPartRequest `json:"parts"`
}

type CompleteMultipartUploadRequest struct {
	Parts []CompletedPartRequest `json:"parts"`
}

type CompletedPartRequest struct {
	PartNumber int32  `json:"part_number"`
	ETag       string `json:"etag"`
}
