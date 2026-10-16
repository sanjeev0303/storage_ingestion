package response

import "time"

type StartMultipartUploadResponse struct {
	UploadID  string    `json:"upload_id"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	Reused    bool      `json:"reused"`
}

type PresignedPartURL struct {
	PartNumber int32     `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type PresignPartsResponse struct {
	UploadID string             `json:"upload_id"`
	Parts    []PresignedPartURL `json:"parts"`
}

type MultipartStatusResponse struct {
	UploadID  string    `json:"upload_id"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}
