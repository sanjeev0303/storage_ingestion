package request

import "encoding/json"

// UpdateDatasetRequest changes a dataset's descriptive fields; absent
// fields stay. Metadata is a merge patch: null removes a key. The
// revision may come in If-Match instead.
type UpdateDatasetRequest struct {
	Revision       int64              `json:"revision,omitempty"`
	Description    *string            `json:"description,omitempty"`
	Tags           *[]string          `json:"tags,omitempty"`
	Metadata       map[string]*string `json:"metadata,omitempty"`
	ProfileID      *string            `json:"profile_id,omitempty"`
	ProfileVersion *int               `json:"profile_version,omitempty"`
}

// MemberRequest names a catalog object by ID, or by namespace and path,
// and optionally one version by ID or number.
type MemberRequest struct {
	ObjectID  string            `json:"object_id,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Path      string            `json:"path,omitempty"`
	VersionID string            `json:"version_id,omitempty"`
	Version   int               `json:"version,omitempty"`
	Label     string            `json:"label,omitempty"`
	Split     string            `json:"split,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// AddMembersRequest adds members; on_duplicate is REJECT (default), SKIP
// or REPLACE.
type AddMembersRequest struct {
	OnDuplicate string          `json:"on_duplicate,omitempty"`
	Members     []MemberRequest `json:"members"`
}

// StageDatasetManifestRequest starts a staged manifest import: the response
// carries the URL to PUT the manifest to.
type StageDatasetManifestRequest struct {
	IdempotencyKey  string `json:"idempotency_key"`
	DuplicatePolicy string `json:"duplicate_policy,omitempty"`
	SizeBytes       int64  `json:"size_bytes"`
	SHA256          string `json:"sha256,omitempty"`
}

type SnapshotRequest struct {
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ValidationRequest starts a validation or readiness assessment.
type ValidationRequest struct {
	VersionID      string `json:"version_id,omitempty"`
	ProfileID      string `json:"profile_id,omitempty"`
	ProfileVersion int    `json:"profile_version,omitempty"`
	// Mode is METADATA (default), FULL or SAMPLED.
	Mode           string `json:"mode,omitempty"`
	SampleSize     int    `json:"sample_size,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// ValidationProfileRequest writes a profile's next version.
type ValidationProfileRequest struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	Kind        string               `json:"kind,omitempty"`
	Rules       []ValidationRuleSpec `json:"rules"`
	Thresholds  struct {
		MaxErrors   int64  `json:"max_errors"`
		MaxWarnings *int64 `json:"max_warnings,omitempty"`
	} `json:"thresholds"`
}

// ValidationRuleSpec is one rule; enabled defaults to true.
type ValidationRuleSpec struct {
	ID       string          `json:"id"`
	Version  int             `json:"version"`
	Type     string          `json:"type"`
	Severity string          `json:"severity"`
	Enabled  *bool           `json:"enabled,omitempty"`
	Scope    string          `json:"scope,omitempty"`
	Config   json.RawMessage `json:"config,omitempty"`
}
