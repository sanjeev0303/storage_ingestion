package response

import "time"

// Catalog responses. They never carry storage keys, bucket names or
// signed URLs: a version's bytes are reached through the asset routes,
// by its asset ID.

type CatalogObjectResponse struct {
	ID         string            `json:"id"`
	Dataset    string            `json:"dataset"`
	Path       string            `json:"path"`
	Versioning string            `json:"versioning"`
	Revision   int64             `json:"revision"`
	Metadata   map[string]string `json:"metadata"`
	// CurrentVersion is absent while nothing is published at the path, or
	// after a logical deletion.
	CurrentVersion *CurrentVersionResponse `json:"current_version,omitempty"`
	Versions       int                     `json:"versions"`
	DeletedAt      *time.Time              `json:"deleted_at,omitempty"`
	CreatedAt      time.Time               `json:"created_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
}

type CurrentVersionResponse struct {
	Version        int        `json:"version"`
	AssetID        string     `json:"asset_id"`
	SizeBytes      int64      `json:"size_bytes"`
	ContentType    string     `json:"content_type"`
	ChecksumSHA256 string     `json:"checksum_sha256,omitempty"`
	PublishedAt    *time.Time `json:"published_at,omitempty"`
}

type CatalogPageResponse struct {
	Objects    []CatalogObjectResponse `json:"objects"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// ObjectVersionResponse is one immutable version. ProviderETag is the
// store's validator, not a checksum; ProviderVersionID is set where the
// bucket versions objects.
type ObjectVersionResponse struct {
	Version           int               `json:"version"`
	AssetID           string            `json:"asset_id"`
	Status            string            `json:"status"`
	Current           bool              `json:"current"`
	FileName          string            `json:"file_name"`
	SizeBytes         int64             `json:"size_bytes"`
	ContentType       string            `json:"content_type"`
	ChecksumAlgorithm string            `json:"checksum_algorithm,omitempty"`
	ChecksumSHA256    string            `json:"checksum_sha256,omitempty"`
	ProviderETag      string            `json:"provider_etag,omitempty"`
	ProviderVersionID string            `json:"provider_version_id,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	PublishedAt       *time.Time        `json:"published_at,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
}

type VersionPageResponse struct {
	Versions   []ObjectVersionResponse `json:"versions"`
	NextBefore int                     `json:"next_before,omitempty"`
}

type StorageCapabilitiesResponse struct {
	LogicalVersioning  bool   `json:"logical_versioning"`
	ProviderVersioning bool   `json:"provider_versioning"`
	VersioningStatus   string `json:"provider_versioning_status,omitempty"`
}
