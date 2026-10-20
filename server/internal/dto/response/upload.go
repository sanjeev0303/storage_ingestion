package response

import "time"

// CreateUploadResponse describes the result of creating an upload.
type CreateUploadResponse struct {
	UploadID  string    `json:"upload_id"`
	Status    string    `json:"status"`
	UploadURL string    `json:"upload_url"`
	ExpiresAt time.Time `json:"expires_at"`
	// UploadHeaders must be sent with the PUT, besides Content-Type
	// (Azure Blob needs x-ms-blob-type).
	UploadHeaders map[string]string `json:"upload_headers,omitempty"`
	// StorageConnectionID is the connection the upload is bound to.
	StorageConnectionID string `json:"storage_connection_id"`
}

// UploadStatusResponse is returned when a client checks progress.
type UploadStatusResponse struct {
	UploadID  string    `json:"upload_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
