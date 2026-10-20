package model

import (
	"errors"
	"strings"
	"time"
)

// AssetStatus represents the lifecycle of a stored file.
type AssetStatus string

const (
	AssetStatusQuarantined AssetStatus = "QUARANTINED"
	AssetStatusProcessing  AssetStatus = "PROCESSING"
	AssetStatusAvailable   AssetStatus = "AVAILABLE"
	AssetStatusRejected    AssetStatus = "REJECTED"
	// AssetStatusSoftDeleted: deletion was requested. The asset is never
	// delivered, and can be restored until its objects start to be removed.
	AssetStatusSoftDeleted AssetStatus = "SOFT_DELETED"
	// AssetStatusDeleted is a tombstone: the asset's objects are gone.
	AssetStatusDeleted AssetStatus = "DELETED"
)

// Rejection reasons recorded with REJECTED assets. Sanitized codes only.
const (
	RejectVerificationFailed = "verification_failed"
	RejectContentType        = "content_type_not_allowed"
	RejectContentMismatch    = "content_does_not_match_declared_type"
	RejectFileTooLarge       = "file_too_large"
	RejectMalwareDetected    = "malware_detected"
	RejectScannerUnavailable = "scanner_unavailable"
	RejectSourceMissing      = "source_object_missing"
	RejectEmptyFile          = "empty_file"
	RejectUnsafeFileName     = "unsafe_file_name"
	RejectExtensionMismatch  = "extension_does_not_match_type"
	RejectUnknownType        = "unknown_content_type"
	RejectUnsafeArchive      = "unsafe_archive"
)

// Asset contains the metadata used to identify and deliver a file.
type Asset struct {
	ID       string
	TenantID string
	OwnerID  string
	// UploadID is the upload session the asset came from (one asset each).
	UploadID string
	// ObjectKey is the quarantine key until promotion, then the delivery key.
	ObjectKey      string
	FileName       string
	ContentType    string
	SizeBytes      int64
	ChecksumSHA256 string
	// Dataset optionally groups assets for retention policies.
	Dataset string
	Status  AssetStatus
	// RejectionReason explains a REJECTED asset.
	RejectionReason string
	// StorageETag and StorageVersionID identify the exact stored object the
	// asset was published as (VersionID only where the provider versions
	// objects). The ETag is a provider validator, not a content checksum.
	StorageETag      string
	StorageVersionID string
	PublishedAt      *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// The catalog: the logical object this asset is a version of, its
	// version number (0 until published), and the user metadata it was
	// uploaded with. Assets from before the catalog have no object.
	ObjectID string
	Version  int
	Metadata map[string]string
}

// CanTransitionAsset reports whether the asset lifecycle allows from -> to:
//
//	QUARANTINED -> PROCESSING -> AVAILABLE
//	QUARANTINED / PROCESSING -> REJECTED
//	any state except DELETED -> SOFT_DELETED -> DELETED
//	any state except DELETED -> DELETED
//
// AVAILABLE is reachable only from PROCESSING, which an asset enters only
// after its stored bytes passed verification. Restoring a SOFT_DELETED
// asset is not a general transition: it returns the asset to the status
// recorded by its deletion operation, and only the deletion repository
// performs it.
func CanTransitionAsset(from, to AssetStatus) bool {
	switch from {
	case AssetStatusQuarantined:
		return to == AssetStatusProcessing || to == AssetStatusRejected || to == AssetStatusSoftDeleted || to == AssetStatusDeleted
	case AssetStatusProcessing:
		return to == AssetStatusAvailable || to == AssetStatusRejected || to == AssetStatusSoftDeleted || to == AssetStatusDeleted
	case AssetStatusAvailable, AssetStatusRejected:
		return to == AssetStatusSoftDeleted || to == AssetStatusDeleted
	case AssetStatusSoftDeleted:
		return to == AssetStatusDeleted
	}
	return false
}

// Validate checks that the asset has the minimum metadata
// required by the domain.
func (a Asset) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return errors.New("asset ID is required")
	}
	if strings.TrimSpace(a.TenantID) == "" {
		return errors.New("tenant ID is required")
	}
	if strings.TrimSpace(a.OwnerID) == "" {
		return errors.New("owner ID is required")
	}
	if strings.TrimSpace(a.ObjectKey) == "" {
		return errors.New("object key is required")
	}
	if strings.TrimSpace(a.FileName) == "" {
		return errors.New("file name is required")
	}
	if strings.TrimSpace(a.ContentType) == "" {
		return errors.New("content type is required")
	}
	if a.SizeBytes <= 0 {
		return errors.New("asset size must be greater than zero")
	}
	if a.Status == "" {
		return errors.New("asset status is required")
	}

	return nil
}

// IsAvailable reports whether the asset can be delivered to a client.
func (a Asset) IsAvailable() bool {
	return a.Status == AssetStatusAvailable
}
