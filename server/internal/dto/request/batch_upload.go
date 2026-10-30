package request

// Bulk ingestion requests. Part URLs, part confirmation and multipart
// completion reuse PresignPartsRequest, RecordPartsRequest and
// CompleteMultipartUploadRequest.

type CreateDatasetRequest struct {
	Name string `json:"name"`
	// CompletionPolicy is ALL (default), REQUIRED or PARTIAL.
	CompletionPolicy string `json:"completion_policy,omitempty"`
	// MaxBytes caps the dataset's size; 0 means only the tenant quota.
	MaxBytes int64 `json:"max_bytes,omitempty"`

	// The managed dataset (Phase 10).
	Description    string            `json:"description,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	ProfileID      string            `json:"profile_id,omitempty"`
	ProfileVersion int               `json:"profile_version,omitempty"`
	// DerivedFrom makes a derived dataset from exact versions.
	DerivedFrom *DerivationRequest `json:"derived_from,omitempty"`
}

// DerivationRequest names a derived dataset's source versions, optionally
// only some of their splits and labels, and the transformation applied.
type DerivationRequest struct {
	VersionIDs     []string               `json:"version_ids"`
	Splits         []string               `json:"splits,omitempty"`
	Labels         []string               `json:"labels,omitempty"`
	Transformation *TransformationRequest `json:"transformation,omitempty"`
	// CopyMembers false starts the dataset empty, with the sources as its
	// lineage only; true (the default) copies their members.
	CopyMembers *bool `json:"copy_members,omitempty"`
}

type TransformationRequest struct {
	Name    string         `json:"name"`
	Version string         `json:"version"`
	Config  map[string]any `json:"config,omitempty"`
}

type CreateIngestionJobRequest struct {
	DatasetID      string `json:"dataset_id"`
	IdempotencyKey string `json:"idempotency_key"`
	MaxConcurrency int    `json:"max_concurrency,omitempty"`
}

type StageManifestRequest struct {
	SizeBytes int64 `json:"size_bytes"`
}

type SubmitStagedManifestRequest struct {
	SHA256 string `json:"sha256,omitempty"`
}

type ClaimEntriesRequest struct {
	Max int `json:"max,omitempty"`
}

type RetryEntriesRequest struct {
	Positions []int64 `json:"positions,omitempty"`
	All       bool    `json:"all,omitempty"`
}
