package response

import "time"

// AssetResponse is an asset's metadata as its owner sees it. It never
// includes the bucket or the object key.
type AssetResponse struct {
	ID              string    `json:"id"`
	FileName        string    `json:"file_name"`
	ContentType     string    `json:"content_type"`
	SizeBytes       int64     `json:"size_bytes"`
	ChecksumSHA256  string    `json:"checksum_sha256,omitempty"`
	Status          string    `json:"status"`
	RejectionReason string    `json:"rejection_reason,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// DownloadURLResponse carries a temporary download URL. The URL is a
// bearer credential until expires_at.
type DownloadURLResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Delivery  string    `json:"delivery"`
}
