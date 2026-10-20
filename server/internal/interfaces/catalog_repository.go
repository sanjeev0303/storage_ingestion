package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// CatalogRepository stores logical objects; their versions are assets.
// Every method is scoped to one tenant.
type CatalogRepository interface {
	Get(ctx context.Context, tenantID, objectID string) (model.CatalogObject, error)
	GetByPath(ctx context.Context, tenantID, dataset, pathKey string) (model.CatalogObject, error)
	// List returns one keyset page; the statement may run at most timeout.
	List(ctx context.Context, f model.CatalogFilter, timeout time.Duration) (model.CatalogPage, error)
	ListVersions(ctx context.Context, tenantID, objectID string, before, limit int) (model.VersionPage, error)
	GetVersion(ctx context.Context, tenantID, objectID string, version int) (model.Asset, error)
	LiveVersions(ctx context.Context, tenantID, objectID string) ([]model.Asset, error)
	// Change applies a client's change under the object's row lock, if
	// the revision the client read is current; with its audit event.
	Change(ctx context.Context, c ObjectChange) (model.CatalogObject, error)
	SetCurrent(ctx context.Context, tenantID, objectID string, version int, expectedRevision int64, at time.Time,
		audit func(before model.CatalogObject, assetID string) model.AuditEvent) (model.CatalogObject, error)
	MarkDeleted(ctx context.Context, tenantID, objectID string, at time.Time, audit model.AuditEvent) (model.CatalogObject, error)
}

// ObjectChange edits an object: Apply changes it in place, from the
// object as it is under its row lock.
type ObjectChange struct {
	TenantID         string
	ObjectID         string
	ExpectedRevision int64
	At               time.Time
	Apply            func(o *model.CatalogObject) error
	Audit            func(before, after model.CatalogObject) model.AuditEvent
}

// CatalogMaintenance is what catalog reconciliation and the operator
// tools need: bounded scans of one tenant, and repairs of catalog rows.
type CatalogMaintenance interface {
	CatalogTenantsAfter(ctx context.Context, after string, limit int) ([]string, error)
	CheckObjects(ctx context.Context, tenantID string, after model.CatalogPosition, limit int) ([]model.ObjectCheck, error)
	RepairCurrent(ctx context.Context, tenantID, objectID string, at time.Time) error
	UncataloguedAssets(ctx context.Context, tenantID, afterID string, limit int) ([]model.Asset, error)
	AttachLegacy(ctx context.Context, asset model.Asset, path string, at time.Time) (bool, error)
	PublishedVersions(ctx context.Context, tenantID, afterID string, limit int) ([]model.Asset, error)
	ExistingAssets(ctx context.Context, tenantID string, ids []string) (map[string]bool, error)
	StaleUploads(ctx context.Context, tenantID string, before time.Time) (int, error)
	PathIndexValid(ctx context.Context) (bool, error)
	Stats(ctx context.Context) (model.CatalogStats, error)
}
