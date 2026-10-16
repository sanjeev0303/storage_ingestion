package memory

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// AssetRepository keeps assets in memory with the same compare-and-set
// rules as the PostgreSQL adapter. For tests only.
type AssetRepository struct {
	mu     sync.Mutex
	assets map[string]model.Asset
}

var _ interfaces.AssetRepository = (*AssetRepository)(nil)

func NewAssetRepository() *AssetRepository {
	return &AssetRepository{assets: make(map[string]model.Asset)}
}

func (r *AssetRepository) Create(ctx context.Context, asset *model.Asset) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if asset == nil {
		return errors.New("asset must not be nil")
	}
	if err := asset.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.assets {
		if existing.ID == asset.ID || existing.UploadID == asset.UploadID {
			return errors.New("asset already exists")
		}
	}
	r.assets[asset.ID] = *asset
	return nil
}

func (r *AssetRepository) GetByID(ctx context.Context, tenantID string, assetID string) (*model.Asset, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	asset, ok := r.assets[assetID]
	if !ok || asset.TenantID != tenantID {
		return nil, model.ErrAssetNotFound
	}
	return &asset, nil
}

func (r *AssetRepository) TransitionStatus(
	ctx context.Context,
	tenantID string,
	assetID string,
	from []model.AssetStatus,
	to model.AssetStatus,
	rejectionReason string,
	objectKey string,
	at time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	asset, ok := r.assets[assetID]
	if !ok || asset.TenantID != tenantID {
		return model.ErrAssetNotFound
	}
	if !slices.Contains(from, asset.Status) || !model.CanTransitionAsset(asset.Status, to) {
		return model.ErrInvalidAssetState
	}

	asset.Status = to
	if to == model.AssetStatusRejected {
		asset.RejectionReason = rejectionReason
	}
	if objectKey != "" {
		asset.ObjectKey = objectKey
	}
	asset.UpdatedAt = at
	r.assets[assetID] = asset
	return nil
}

// byUpload returns the asset made from an upload, for EnqueueDue.
func (r *AssetRepository) byUpload(uploadID string) (model.Asset, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, asset := range r.assets {
		if asset.UploadID == uploadID {
			return asset, true
		}
	}
	return model.Asset{}, false
}
