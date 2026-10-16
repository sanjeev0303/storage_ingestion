package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// AssetRepository owns persistence operations for stored assets.
// Every method is scoped to one tenant.
type AssetRepository interface {
	Create(
		ctx context.Context,
		asset *model.Asset,
	) error

	GetByID(
		ctx context.Context,
		tenantID string,
		assetID string,
	) (*model.Asset, error)

	// TransitionStatus moves the asset to `to` only while its status is one
	// of `from` (a compare-and-set), so concurrent rejection, deletion and
	// promotion cannot overwrite one another. objectKey, when not empty,
	// replaces the object key in the same write (promotion). It returns
	// model.ErrInvalidAssetState when the asset is in any other status.
	TransitionStatus(
		ctx context.Context,
		tenantID string,
		assetID string,
		from []model.AssetStatus,
		to model.AssetStatus,
		rejectionReason string,
		objectKey string,
		at time.Time,
	) error
}
