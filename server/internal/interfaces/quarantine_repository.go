package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// QuarantineRepository stores quarantine records. Every lookup is scoped to
// one tenant.
type QuarantineRepository interface {
	// Hold opens a HELD record for the asset, or returns the open one: an
	// asset has at most one HELD record at a time.
	Hold(ctx context.Context, record model.QuarantineRecord) (model.QuarantineRecord, error)

	// Reject records a permanent rejection. Any HELD record for the asset
	// is closed as REJECTED instead of adding a second record.
	Reject(ctx context.Context, record model.QuarantineRecord) error

	// Release closes the asset's HELD record, if any, as RELEASED.
	Release(ctx context.Context, tenantID, assetID string, at time.Time) (bool, error)

	// DuePurge returns up to limit REJECTED records whose retention ended.
	DuePurge(ctx context.Context, now time.Time, limit int) ([]model.QuarantineRecord, error)

	// MarkPurged closes a REJECTED record once its object is deleted.
	MarkPurged(ctx context.Context, id string, at time.Time) error

	// ListForAsset returns the asset's records, newest first.
	ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.QuarantineRecord, error)
}

// ProcessingEventRepository appends processing audit events.
type ProcessingEventRepository interface {
	Record(ctx context.Context, event model.ProcessingEvent) error
	ListForAsset(ctx context.Context, tenantID, assetID string) ([]model.ProcessingEvent, error)
}

// ContentPolicyRepository returns a tenant's own content policy. ok is
// false when the tenant has none and the default applies.
type ContentPolicyRepository interface {
	Get(ctx context.Context, tenantID string) (policy model.ContentPolicy, ok bool, err error)
}
