package response

import (
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// StorageCapabilities are what a provider (or a tested bucket) can do.
type StorageCapabilities struct {
	PresignedUpload       bool     `json:"presigned_upload"`
	MaxSingleUploadBytes  int64    `json:"max_single_upload_bytes"`
	Multipart             bool     `json:"multipart"`
	MultipartKind         string   `json:"multipart_kind,omitempty"`
	MaxParts              int      `json:"max_parts,omitempty"`
	MinPartBytes          int64    `json:"min_part_bytes,omitempty"`
	PartValidator         string   `json:"part_validator,omitempty"`
	ListIncompleteUploads bool     `json:"list_incomplete_uploads"`
	ServerSideCopy        bool     `json:"server_side_copy"`
	RangeReads            bool     `json:"range_reads"`
	ConditionalReads      bool     `json:"conditional_reads"`
	PresignedDownload     bool     `json:"presigned_download"`
	ListObjects           bool     `json:"list_objects"`
	ProviderVersioning    bool     `json:"provider_versioning"`
	VersioningStatus      string   `json:"versioning_status,omitempty"`
	VersionIDKind         string   `json:"version_id_kind,omitempty"`
	ChecksumAlgorithms    []string `json:"checksum_algorithms"`
}

type StorageAuthMethod struct {
	Method       string   `json:"method"`
	SecretFields []string `json:"secret_fields"`
}

type StorageProvider struct {
	Provider       string              `json:"provider"`
	Name           string              `json:"name"`
	AuthMethods    []StorageAuthMethod `json:"auth_methods"`
	RequiredFields []string            `json:"required_fields"`
	Notes          []string            `json:"notes"`
}

type StorageProvidersResponse struct {
	Providers []StorageProvider `json:"providers"`
	// BYOSEnabled: organizations may connect their own storage here.
	BYOSEnabled bool `json:"byos_enabled"`
}

type StorageProviderCapabilities struct {
	Provider     string              `json:"provider"`
	Capabilities StorageCapabilities `json:"capabilities"`
}

type ProviderCapabilitiesResponse struct {
	Providers []StorageProviderCapabilities `json:"providers"`
}

// StorageConnectionResponse is a connection with its non-secret settings
// and whether credentials are stored, never the credentials.
type StorageConnectionResponse struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	Status         string `json:"status"`
	AuthMethod     string `json:"auth_method"`
	Bucket         string `json:"bucket,omitempty"`
	Region         string `json:"region,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	AccountID      string `json:"account_id,omitempty"`
	ForcePathStyle bool   `json:"force_path_style"`
	RoleARN        string `json:"role_arn,omitempty"`
	// ExternalID goes in the customer's role trust policy; it is not a
	// secret.
	ExternalID    string `json:"external_id,omitempty"`
	StagingPrefix string `json:"staging_prefix,omitempty"`
	ReadyPrefix   string `json:"ready_prefix,omitempty"`
	StagingMode   string `json:"staging_mode,omitempty"`
	IsDefault     bool   `json:"is_default"`

	HasCredentials      bool       `json:"has_credentials"`
	CredentialRotation  int        `json:"credential_rotation"`
	CredentialRotatedAt *time.Time `json:"credential_rotated_at,omitempty"`

	Capabilities          *StorageCapabilities `json:"capabilities,omitempty"`
	CapabilitiesCheckedAt *time.Time           `json:"capabilities_checked_at,omitempty"`
	LastTestedAt          *time.Time           `json:"last_tested_at,omitempty"`
	LastTestOKAt          *time.Time           `json:"last_test_ok_at,omitempty"`
	LastErrorCategory     string               `json:"last_error_category,omitempty"`

	ChecksumMode  string `json:"checksum_mode"`
	ExpectedOwner string `json:"expected_owner,omitempty"`
	EncryptionKey string `json:"encryption_key,omitempty"`
	// StagedCredential is set while a second credential waits to be
	// promoted; PreviousCredentialUntil while a rollback is possible.
	StagedCredential        *StagedCredential `json:"staged_credential,omitempty"`
	PreviousCredentialUntil *time.Time        `json:"previous_credential_until,omitempty"`

	Revision   int64      `json:"revision"`
	CreatedBy  string     `json:"created_by,omitempty"`
	UpdatedBy  string     `json:"updated_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
}

type StagedCredential struct {
	Rotation int        `json:"rotation"`
	StagedAt *time.Time `json:"staged_at,omitempty"`
	StagedBy string     `json:"staged_by,omitempty"`
}

type StorageConnectionsResponse struct {
	Connections         []StorageConnectionResponse `json:"connections"`
	DefaultConnectionID string                      `json:"default_connection_id"`
}

type StorageTestCheck struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Category string `json:"category,omitempty"`
}

// StorageTestResponse is a sanitized test outcome: a category and a fixed
// message, never the provider's own error.
type StorageTestResponse struct {
	OK           bool                       `json:"ok"`
	Category     string                     `json:"category,omitempty"`
	Message      string                     `json:"message,omitempty"`
	Checks       []StorageTestCheck         `json:"checks"`
	Capabilities *StorageCapabilities       `json:"capabilities,omitempty"`
	TestedAt     time.Time                  `json:"tested_at"`
	Connection   *StorageConnectionResponse `json:"connection,omitempty"`
	// Posture is the bucket's posture, checked after a passing test.
	Posture *model.PostureReport `json:"posture,omitempty"`
}

// StorageDependentsResponse explains why a connection cannot be deleted.
type StorageDependentsResponse struct {
	Error         string `json:"error"`
	Code          string `json:"code"`
	ActiveUploads int64  `json:"active_uploads"`
	LiveAssets    int64  `json:"live_assets"`
	Datasets      int64  `json:"datasets"`
	IsDefault     bool   `json:"is_default"`
}
