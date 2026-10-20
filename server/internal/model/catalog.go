package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The catalog: logical objects and their versions.
//
// A logical object is a tenant's named file: a normalized path in a
// dataset's namespace (or the tenant's root namespace, dataset ""), with
// a stable ID that never depends on where its bytes are stored. Its
// versions are assets: each upload to the path becomes one asset, stored
// under its own server-generated key, and is numbered when it is
// published. The object points at its current version, and carries the
// mutable user metadata; everything a version says about its bytes
// (size, checksum, storage location) is immutable.

// Versioning says what a new upload to an existing path does.
type Versioning string

const (
	// VersioningEnabled: the new version becomes current and earlier ones
	// stay, retrievable and restorable.
	VersioningEnabled Versioning = "ENABLED"
	// VersioningDisabled: the new version replaces the current one, which
	// is then deleted through the lifecycle (retention and holds apply).
	VersioningDisabled Versioning = "DISABLED"
)

var (
	ErrObjectNotFound   = errors.New("object not found")
	ErrVersionNotFound  = errors.New("version not found")
	ErrPathExists       = errors.New("an object already exists at this path")
	ErrPathBusy         = errors.New("another upload is creating this path")
	ErrRevisionConflict = errors.New("the object changed since it was read")
	ErrInvalidMetadata  = errors.New("invalid metadata")
	ErrInvalidCursor    = errors.New("invalid cursor")
)

// CatalogObject is a logical object.
type CatalogObject struct {
	ID       string
	TenantID string
	// Dataset is the namespace: a dataset name, or "" for the tenant's
	// root namespace.
	Dataset string
	// Path is the normalized path as the client wrote it; PathKey is its
	// case- and normalization-insensitive form, unique per namespace.
	Path       string
	PathKey    string
	Versioning Versioning
	// Revision increases with every change a client can see: metadata,
	// name, versioning, current version. Writes may require the revision
	// they read (optimistic concurrency).
	Revision int64
	Metadata map[string]string
	// The current version, and its system metadata, copied when it became
	// current so listings read one row per object. Empty while the object
	// has no published version, or after a logical deletion.
	CurrentAssetID string
	CurrentVersion int
	SizeBytes      int64
	ContentType    string
	ChecksumSHA256 string
	PublishedAt    *time.Time
	// Versions counts the versions ever published.
	Versions int
	// DeletedAt is set by a logical deletion, until a new version is
	// published at the path.
	DeletedAt *time.Time
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Published reports whether the object has a current version.
func (o CatalogObject) Published() bool { return o.CurrentAssetID != "" }

// CatalogOrder is a listing's sort order. Both are total: the path key is
// unique within a namespace, and the object ID breaks ties in time.
type CatalogOrder string

const (
	OrderByPath    CatalogOrder = "path"    // dataset, then path key
	OrderByUpdated CatalogOrder = "updated" // last change, then ID
)

// CatalogPosition is where a listing page ends: the sort key of its last
// object.
type CatalogPosition struct {
	Dataset   string    `json:"d,omitempty"`
	PathKey   string    `json:"p,omitempty"`
	UpdatedAt time.Time `json:"u,omitempty"`
	ID        string    `json:"i,omitempty"`
}

// CatalogFilter selects a page of one tenant's objects. The tenant comes
// from the caller's principal and is always applied first.
type CatalogFilter struct {
	TenantID string
	// Dataset limits the listing to one namespace when AnyDataset is false.
	Dataset    string
	AnyDataset bool
	// PathPrefix is matched against path keys (see PathKey).
	PathPrefix  string
	ObjectID    string
	AssetID     string
	ContentType string
	MinSize     int64
	MaxSize     int64
	// Time ranges: After is inclusive, Before exclusive; zero means open.
	CreatedAfter  time.Time
	CreatedBefore time.Time
	UpdatedAfter  time.Time
	UpdatedBefore time.Time
	// Metadata entries the object's metadata must contain.
	Metadata map[string]string
	// IncludeUnpublished also lists objects without a current version
	// (uploads in progress, logically deleted objects).
	IncludeUnpublished bool
	Order              CatalogOrder
	After              *CatalogPosition
	Limit              int
}

// CatalogPage is one page of a listing; Next is nil on the last page.
type CatalogPage struct {
	Objects []CatalogObject
	Next    *CatalogPosition
}

// VersionPage is one page of an object's versions, newest first.
type VersionPage struct {
	Versions []Asset
	// NextBefore continues the listing below this version number.
	NextBefore int
}

// MetadataLimits bound user metadata.
type MetadataLimits struct {
	MaxKeys       int
	MaxBytes      int
	MaxValueBytes int
}

var DefaultMetadataLimits = MetadataLimits{MaxKeys: 64, MaxBytes: 8192, MaxValueBytes: 1024}

// reservedMetadataKeys are system fields. User metadata is stored apart
// from them and could not overwrite them anyway; refusing the names keeps
// API responses and filters unambiguous.
var reservedMetadataKeys = []string{
	"id", "object_id", "asset_id", "tenant_id", "dataset", "path", "version", "revision", "status",
	"size_bytes", "content_type", "checksum_sha256", "object_key", "storage_etag", "storage_version_id",
	"owner_id", "retention", "legal_hold", "published_at", "created_at", "updated_at",
}

// ValidateUserMetadata checks user metadata: keys as ValidMetadataKey,
// not reserved and not under "system."; values valid UTF-8 without
// control characters; within the limits.
func ValidateUserMetadata(m map[string]string, l MetadataLimits) error {
	if len(m) > l.MaxKeys {
		return fmt.Errorf("%w: more than %d keys", ErrInvalidMetadata, l.MaxKeys)
	}
	for k, v := range m {
		if !ValidMetadataKey(k) {
			return fmt.Errorf("%w: key %q: keys are 1-64 characters of letters, digits, '.', '_' and '-'", ErrInvalidMetadata, k)
		}
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "system.") {
			return fmt.Errorf("%w: key %q: the system. prefix is reserved", ErrInvalidMetadata, k)
		}
		for _, reserved := range reservedMetadataKeys {
			if lower == reserved {
				return fmt.Errorf("%w: key %q is reserved", ErrInvalidMetadata, k)
			}
		}
		if len(v) > l.MaxValueBytes || !utf8.ValidString(v) {
			return fmt.Errorf("%w: value of %q: at most %d bytes of UTF-8", ErrInvalidMetadata, k, l.MaxValueBytes)
		}
		for _, r := range v {
			if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
				return fmt.Errorf("%w: value of %q has control characters", ErrInvalidMetadata, k)
			}
		}
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMetadata, err)
	}
	if len(encoded) > l.MaxBytes {
		return fmt.Errorf("%w: larger than %d bytes", ErrInvalidMetadata, l.MaxBytes)
	}
	return nil
}

// ErrQueryTimeout refuses a listing that ran past its time budget.
var ErrQueryTimeout = errors.New("the listing took too long; narrow it with a dataset, prefix or time range")

// Catalog reconciliation findings.
const (
	// The current version is not AVAILABLE, or its copied metadata is
	// stale, or an object has AVAILABLE versions and no current one.
	FindingCurrentInvalid = "current_version_invalid"
	FindingCurrentStale   = "current_metadata_stale"
	FindingCurrentMissing = "current_version_missing"
	// An AVAILABLE version without a number: an incomplete publication.
	FindingUnnumbered = "version_unnumbered"
	// A replaced version of an object without versioning not yet deleted.
	FindingReplacedLive = "replaced_version_not_deleted"
	// An AVAILABLE asset from before the catalog.
	FindingUncatalogued = "asset_not_in_catalog"
	// A published version whose stored object is missing or differs.
	FindingObjectMissing = "physical_object_missing"
	FindingSizeMismatch  = "physical_size_mismatch"
	// A stored object under the published prefix no asset refers to.
	FindingUnreferenced = "physical_object_unreferenced"
	// Completed uploads that never became assets.
	FindingStaleUploads = "stale_uploads"
	// The path uniqueness index is not valid (duplicates possible).
	FindingPathIndexInvalid = "path_index_invalid"
)

// CatalogFinding is one inconsistency reconciliation found, and whether
// it repaired it. Physical findings are reported, never repaired by
// deleting anything.
type CatalogFinding struct {
	Kind     string `json:"kind"`
	TenantID string `json:"tenant_id,omitempty"`
	ObjectID string `json:"object_id,omitempty"`
	AssetID  string `json:"asset_id,omitempty"`
	Key      string `json:"key,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Repaired bool   `json:"repaired"`
}

// ObjectCheck is what reconciliation reads about one object: the status
// of its current version, whether its copied metadata is stale, whether it
// lacks a current version while it has AVAILABLE ones, whether a version
// was published without a number, and, for an object without versioning,
// the replaced versions still AVAILABLE.
type ObjectCheck struct {
	Object         CatalogObject
	CurrentStatus  string
	CurrentStale   bool
	MissingCurrent bool
	Unnumbered     bool
	ReplacedLive   []string
}

// CatalogStats sizes the catalog, for capacity planning.
type CatalogStats struct {
	Objects          int64            `json:"objects"`
	PublishedObjects int64            `json:"published_objects"`
	Versions         int64            `json:"versions"`
	TableBytes       map[string]int64 `json:"table_bytes"`
	IndexBytes       map[string]int64 `json:"index_bytes"`
}
