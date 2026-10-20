package response

import "time"

// AssetResponse is an asset's metadata as its owner sees it. It never
// includes the bucket or the object key.
type AssetResponse struct {
	ID              string `json:"id"`
	FileName        string `json:"file_name"`
	ContentType     string `json:"content_type"`
	SizeBytes       int64  `json:"size_bytes"`
	ChecksumSHA256  string `json:"checksum_sha256,omitempty"`
	Dataset         string `json:"dataset,omitempty"`
	Status          string `json:"status"`
	RejectionReason string `json:"rejection_reason,omitempty"`
	// Deliverable reports whether a download would be served now.
	Deliverable bool `json:"deliverable"`
	// ETag is the download validator for If-Range / If-None-Match.
	ETag        string     `json:"etag,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// DownloadURLResponse carries a temporary download URL. The URL is a
// bearer credential until expires_at.
type DownloadURLResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Delivery  string    `json:"delivery"`
	// Revocable is true for token links (DELETE .../download-links).
	Revocable bool `json:"revocable"`
}

// RevokeLinksResponse reports how many links were revoked.
type RevokeLinksResponse struct {
	Revoked int `json:"revoked"`
}
