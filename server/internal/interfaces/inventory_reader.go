package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// LifecycleStorage is what lifecycle cleanup needs from object storage:
// paginated listings, metadata checks and exact deletions. It never
// deletes by prefix.
type LifecycleStorage interface {
	// ListObjectsPage lists one page of objects under prefix after cursor.
	ListObjectsPage(ctx context.Context, bucket, prefix, cursor string, limit int) (ObjectPage, error)
	HeadObject(ctx context.Context, bucket, objectKey string) (ObjectMetadata, error)
	// HeadObjectVersion checks one version of an object (versioned buckets).
	HeadObjectVersion(ctx context.Context, bucket, objectKey, versionID string) (ObjectMetadata, error)
	DeleteObject(ctx context.Context, bucket, objectKey string) error
	// DeleteObjectVersion removes exactly one version of an object
	// (versioned buckets); deleting by key there only adds a delete marker.
	DeleteObjectVersion(ctx context.Context, bucket, objectKey, versionID string) error
}

// InventoryRepository stores inventory scans and orphan candidates.
type InventoryRepository interface {
	// OpenScan leases the bucket prefix's RUNNING scan to owner, or starts
	// a new one with newID. ok is false while another worker holds a live
	// lease on the running scan.
	OpenScan(ctx context.Context, bucket, prefix, owner, newID string, now time.Time, lease time.Duration) (scan model.InventoryScan, ok bool, err error)
	// SaveProgress records the scan's cursor and counters and extends the
	// lease. model.ErrLeaseLost when owner no longer holds it.
	SaveProgress(ctx context.Context, scan model.InventoryScan, owner string, now time.Time, lease time.Duration) error
	// FinishScan records the scan's final counters, ends it as COMPLETED
	// or FAILED and releases it.
	FinishScan(ctx context.Context, scan model.InventoryScan, owner string, now time.Time, status string) error
	// LastCompleted returns the latest completed scan of the bucket prefix.
	LastCompleted(ctx context.Context, bucket, prefix string) (model.InventoryScan, bool, error)
	RecentScans(ctx context.Context, limit int) ([]model.InventoryScan, error)

	// Mark records a sighting of an unowned object in scanID. A second
	// sighting in the same scan does not count again; a changed ETag means
	// a different object and starts the count over.
	Mark(ctx context.Context, candidate model.OrphanCandidate, scanID string, now time.Time) (model.OrphanCandidate, error)
	// DueCandidates returns open candidates under keyPrefix ready to sweep:
	// seen in at least minObservations scans, first seen before seenBefore,
	// last seen in a completed scan, and with fewer than maxAttempts
	// failed deletions.
	DueCandidates(ctx context.Context, bucket, keyPrefix string, seenBefore time.Time, minObservations, maxAttempts, limit int) ([]model.OrphanCandidate, error)
	// Resolve closes a candidate as DELETED or CLEARED.
	Resolve(ctx context.Context, candidateID, status, reason string, now time.Time) error
	// FailCandidate records a failed deletion; it is retried next sweep.
	FailCandidate(ctx context.Context, candidateID, lastError string, now time.Time) error
	ListCandidates(ctx context.Context, status string, limit int) ([]model.OrphanCandidate, error)
}

// StorageCapabilities are what a bucket's provider does, as it reports
// them. Logical versioning never depends on them: every version has its
// own key. Provider versioning only adds a version ID to each stored
// object, which reads and deletes then name.
type StorageCapabilities struct {
	// ProviderVersioning: the bucket keeps object versions ("Enabled").
	ProviderVersioning bool
	// VersioningStatus is the provider's answer: "Enabled", "Suspended",
	// or "" for a bucket that never had versioning.
	VersioningStatus string
}

// CapabilityReporter reports a bucket's capabilities.
type CapabilityReporter interface {
	Capabilities(ctx context.Context, bucket string) (StorageCapabilities, error)
}
