package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// PackLocator tells readers where a packed asset version's bytes are.
type PackLocator interface {
	// PackedLocation returns the published pack entry serving the asset,
	// of this tenant only; false when the asset is not packed.
	PackedLocation(ctx context.Context, tenantID, assetID string) (model.PackedLocation, bool, error)
}

// PackReleaser lets lifecycle deletion take an asset out of its pack.
type PackReleaser interface {
	PackLocator
	// ReleaseAsset stops serving the asset from any pack and reports
	// whether a pack that still exists holds its bytes.
	ReleaseAsset(ctx context.Context, tenantID, assetID string, now time.Time) (holding bool, err error)
}

// PackCandidateFilter selects the published assets packing may take.
type PackCandidateFilter struct {
	MinBytes, MaxBytes int64
	// PublishedBefore: only assets published at least this long ago.
	PublishedBefore time.Time
}

// PackSource is a published entry whose standalone source copy may be
// retired, with its asset as it is now.
type PackSource struct {
	Entry model.PackEntry
	Asset model.Asset
}

// PackChange is what a step of a packing job records with its state.
type PackChange struct {
	SHA256           string
	ManifestSHA256   string
	StorageETag      string
	StorageVersionID string
}

// PackRepository stores packs, their entries and their jobs.
type PackRepository interface {
	PackReleaser

	// PackingTenants pages through the tenants with published assets.
	PackingTenants(ctx context.Context, after string, limit int) ([]string, error)
	// PackCandidates pages through one tenant's assets that packing may
	// take, in ID order after afterAssetID: published, verified, standalone,
	// within the filter, under no legal hold, and in no pack.
	PackCandidates(ctx context.Context, tenantID, afterAssetID string, f PackCandidateFilter, limit int) ([]model.Asset, error)
	// BuildingPacks counts a tenant's packs still being built.
	BuildingPacks(ctx context.Context, tenantID string) (int, error)
	// CreatePack stores a PLANNED pack with its RESERVED entries and the
	// outbox events, in one transaction. model.ErrPackConflict when an
	// entry is reserved or published elsewhere.
	CreatePack(ctx context.Context, pack model.Pack, entries []model.PackEntry, outbox ...model.OutboxEvent) error

	// ClaimPacks leases due packs still being built, oldest first.
	ClaimPacks(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]model.Pack, error)
	// ClaimPack leases one pack if it is due and free.
	ClaimPack(ctx context.Context, tenantID, packID, owner string, now time.Time, lease time.Duration) (model.Pack, bool, error)
	RenewPack(ctx context.Context, packID, owner string, now time.Time, lease time.Duration) error
	GetPack(ctx context.Context, tenantID, packID string) (model.Pack, error)
	Entries(ctx context.Context, packID string) ([]model.PackEntry, error)
	// AdvancePack moves a leased pack from one state to the next, with
	// what the step recorded. model.ErrLeaseLost when the lease is gone.
	AdvancePack(ctx context.Context, packID, owner string, from, to model.PackState, change PackChange, now time.Time) error
	// RetryPack releases the lease and schedules the next attempt.
	RetryPack(ctx context.Context, packID, owner string, now, next time.Time, lastError string) error
	// FailPack ends the job: the pack is FAILED, its reserved entries
	// released, those of excluded assets marked EXCLUDED.
	FailPack(ctx context.Context, packID, owner string, now time.Time, lastError string, excluded []string) error
	// PublishPack publishes a verified pack in one transaction: entries of
	// assets still published become PUBLISHED (moving them out of the pack
	// rewritten, if any), the others DELETED, and the pack PUBLISHED with
	// its manifest's checksum.
	PublishPack(ctx context.Context, packID, owner, manifestSHA256 string, now time.Time) (published, dead int, err error)

	// SourcesToRetire lists published entries whose standalone source is
	// still there, in packs published before cutoff, of assets under no
	// legal hold.
	SourcesToRetire(ctx context.Context, cutoff time.Time, limit int) ([]PackSource, error)
	MarkSourceRetired(ctx context.Context, packID string, ordinal int, now time.Time) error
	// RetireEmptyPacks moves published packs no entry is served from any
	// more to RETIRING, and returns how many.
	RetireEmptyPacks(ctx context.Context, now time.Time) (int, error)
	// PacksToRewrite lists published packs with deleted entries worth a
	// rewrite: dead bytes at least ratio of the pack, or a deletion waiting
	// since before waitedBefore, and no rewrite already under way.
	PacksToRewrite(ctx context.Context, ratio float64, waitedBefore time.Time, limit int) ([]model.Pack, error)
	// PacksToRemove lists RETIRING and FAILED packs whose objects are to be
	// deleted, changed before cutoff.
	PacksToRemove(ctx context.Context, cutoff time.Time, limit int) ([]model.Pack, error)
	// MarkPackDeleted records that a RETIRING or FAILED pack's objects are
	// gone.
	MarkPackDeleted(ctx context.Context, packID string, from model.PackState, now time.Time) error
	// PackPlaces lists every tenant and connection packs were made on (in
	// TenantID and ConnectionID).
	PackPlaces(ctx context.Context) ([]model.Pack, error)
}
