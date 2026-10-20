package request

// DownloadURLRequest asks for a temporary download URL. All fields are
// optional; the server bounds the lifetime and sanitizes the filename.
// There is deliberately no bucket or key field.
type DownloadURLRequest struct {
	ExpiresInSeconds int    `json:"expires_in_seconds,omitempty"`
	Filename         string `json:"filename,omitempty"`
	// Mode is "storage" (presigned, expires only) or "token" (revocable
	// API link); empty uses the server default.
	Mode string `json:"mode,omitempty"`
}
