package model

import (
	"errors"
	"time"
)

// ErrDownloadTokenNotFound: no token has this hash.
var ErrDownloadTokenNotFound = errors.New("download token not found")

// Delivery operations recorded in the audit log.
const (
	DeliveryOpMetadata    = "METADATA"
	DeliveryOpDownloadURL = "DOWNLOAD_URL"
	DeliveryOpProxy       = "PROXY_DOWNLOAD"
	DeliveryOpToken       = "TOKEN_DOWNLOAD"
	DeliveryOpTokenRevoke = "TOKEN_REVOKE"
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
	Reason string
	// RequestID correlates the event with the request's logs.
	RequestID string
	// ByteRange is "first-last/size" for partial downloads.
	ByteRange string
	// BytesSent is set on transfer failures.
	BytesSent int64
	// SourceIP is where the request came from; CredentialID the API token
	// it used ("" for a session).
	SourceIP     string
	CredentialID string
	CreatedAt    time.Time
}

// DownloadToken is a revocable download link. Only the SHA-256 of the
// token is stored; the token itself is shown once, to its issuer.
type DownloadToken struct {
	ID          string
	TokenHash   string
	TenantID    string
	AssetID     string
	PrincipalID string
	Filename    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}
