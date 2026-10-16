package model

import "time"

// Delivery operations recorded in the audit log.
const (
	DeliveryOpMetadata    = "METADATA"
	DeliveryOpDownloadURL = "DOWNLOAD_URL"
	DeliveryOpProxy       = "PROXY_DOWNLOAD"
)

// Delivery outcomes recorded in the audit log.
const (
	DeliveryAllowed = "ALLOWED"
	DeliveryDenied  = "DENIED"
	DeliveryFailed  = "FAILED"
)

// DeliveryAuditEvent records one delivery decision. It never holds the
// issued URL, credentials or file content.
type DeliveryAuditEvent struct {
	ID          string
	TenantID    string
	PrincipalID string
	AssetID     string
	Operation   string
	Outcome     string
	// Reason is a sanitized code for denials and failures.
	Reason    string
	CreatedAt time.Time
}
