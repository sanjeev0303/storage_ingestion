package event

import (
	"errors"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// TypeVersionReplaced is written, with the publication, when a new
// version replaces the current version of an object whose versioning is
// disabled. The lifecycle consumer deletes the replaced version as its
// retention allows: the catalog never removes bytes itself.
const TypeVersionReplaced = "catalog.version_replaced"

// VersionReplaced is the payload of catalog.version_replaced, version 1.
type VersionReplaced struct {
	ObjectID string `json:"object_id"`
	AssetID  string `json:"asset_id"`
}

func (p *VersionReplaced) validate() error {
	if p.ObjectID == "" || p.AssetID == "" {
		return errors.New("object_id and asset_id are required")
	}
	return nil
}

// NewVersionReplaced builds the event for a replaced version. It is
// ordered with the replaced asset's other events.
func NewVersionReplaced(tenantID, objectID, assetID string, at time.Time) model.OutboxEvent {
	return newEvent(TypeVersionReplaced, tenantID, AggregateAsset, assetID,
		&VersionReplaced{ObjectID: objectID, AssetID: assetID}, at, "")
}
