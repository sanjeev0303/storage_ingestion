package model

import (
	"errors"
	"time"
)

// Ecosystem connectors (docs/connectors.md).

var (
	ErrChatNotFound = errors.New("chat integration or share not found")
	ErrChatExpired  = errors.New("this link has expired or was revoked")
	ErrChatDelivery = errors.New("the chat service did not accept the message")
	ErrInvalidConn  = errors.New("invalid connector request")
	ErrLockHeld     = errors.New("the path is locked by someone else")
)

// ChatIntegration is a Slack or Teams incoming webhook; its URL is sealed.
type ChatIntegration struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"-"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	URLHost    string    `json:"url_host"`
	KeyVersion int       `json:"-"`
	Nonce      []byte    `json:"-"`
	Cipher     []byte    `json:"-"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// ChatShare is what a chat message links to.
type ChatShare struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"-"`
	AssetID       string     `json:"asset_id"`
	IntegrationID string     `json:"integration_id"`
	Note          string     `json:"note"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	Opens         int64      `json:"opens"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	Path          string     `json:"path"`
	SizeBytes     int64      `json:"size_bytes"`
	// URL is the page the message links to.
	URL string `json:"url,omitempty"`
}

// LFSLock is a Git LFS file lock.
type LFSLock struct {
	ID       string
	TenantID string
	Dataset  string
	Path     string
	OwnerID  string
	LockedAt time.Time
}

// Collection formats.
const (
	CollectionCommsJSONL = "COMMS_JSONL"
	CollectionRSMF       = "RSMF"
	CollectionUFDR       = "UFDR"
	CollectionManifest   = "MANIFEST"
)

// CollectionImport records a comms-archive or forensic export brought in.
type CollectionImport struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"-"`
	DatasetID    string    `json:"dataset_id"`
	Format       string    `json:"format"`
	Source       string    `json:"source"`
	ExportSHA256 string    `json:"export_sha256"`
	Items        int64     `json:"items"`
	Verified     int64     `json:"verified"`
	Mismatches   []string  `json:"mismatches"`
	Retention    string    `json:"retention"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// DicomInstance is one DICOM instance of a version.
type DicomInstance struct {
	SOPUID    string
	StudyUID  string
	SeriesUID string
	Modality  string
	PatientID string
	StudyDate string
	AssetID   string
	Path      string
	Tags      map[string]map[string]any
}

// DicomFilter narrows the instances of a version.
type DicomFilter struct {
	Study, Series, Instance, PatientID, Modality, StudyDate string
}
