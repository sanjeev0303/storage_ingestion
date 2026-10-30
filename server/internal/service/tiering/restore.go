package tiering

import (
	"context"
	"errors"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// RestoreInput asks for archived objects to be readable for Days, at a
// Speed (docs/tiering.md).
type RestoreInput struct {
	Days  int                `json:"days"`
	Speed model.RestoreSpeed `json:"speed"`
}

func (in *RestoreInput) check() error {
	if in.Days == 0 {
		in.Days = 7
	}
	if in.Speed == "" {
		in.Speed = model.RestoreStandard
	}
	if in.Days < 1 || in.Days > 365 || !in.Speed.Valid() {
		return invalid("days is 1 to 365 and speed FAST, STANDARD or BULK")
	}
	return nil
}

// RequestRestore asks for one archived asset to be readable.
func (s *Service) RequestRestore(ctx context.Context, p model.Principal, assetID string, in RestoreInput) (model.ArchiveRestore, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsDownload, "asset.restore", "asset", assetID); err != nil {
		return model.ArchiveRestore{}, err
	}
	if err := in.check(); err != nil {
		return model.ArchiveRestore{}, err
	}
	a, err := s.deps.Assets.GetByID(ctx, p.TenantID, assetID)
	if err != nil || a.Status != model.AssetStatusAvailable || !p.Scope.Allows(a.Dataset) {
		return model.ArchiveRestore{}, model.ErrAssetNotFound
	}
	if a.StorageTier != model.TierArchive && a.StorageTier != model.TierDeepArchive {
		return model.ArchiveRestore{}, invalid("the asset is not archived")
	}
	x, _, err := s.deps.Store.CreateRestore(ctx, model.ArchiveRestore{ID: newID(), TenantID: p.TenantID, AssetID: a.ID,
		Days: in.Days, Speed: in.Speed, RequestedBy: p.UserID, CreatedAt: s.now()})
	if err != nil {
		return model.ArchiveRestore{}, err
	}
	s.record(ctx, p, "asset.restore", "asset", a.ID, map[string]any{"days": in.Days, "speed": in.Speed})
	return x, nil
}

// VersionRestore is what restoring a snapshot asked for.
type VersionRestore struct {
	Archived    int                    `json:"archived"`
	Requested   int                    `json:"requested"`
	AlreadyOpen int                    `json:"already_open"`
	Readable    int                    `json:"readable"`
	Estimate    Estimate               `json:"estimate"`
	Restores    []model.ArchiveRestore `json:"restores"`
}

// RestoreVersion asks for every archived member of a snapshot to be
// readable, so a training run or an audit can read it whole.
func (s *Service) RestoreVersion(ctx context.Context, p model.Principal, datasetID, versionID string, in RestoreInput) (VersionRestore, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsDownload, "dataset.restore_archived", "dataset_version", versionID); err != nil {
		return VersionRestore{}, err
	}
	if _, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID); err != nil {
		return VersionRestore{}, err
	}
	if err := in.check(); err != nil {
		return VersionRestore{}, err
	}
	members, err := s.deps.Store.ArchivedMembers(ctx, p.TenantID, versionID)
	if err != nil {
		return VersionRestore{}, err
	}
	out := VersionRestore{Archived: len(members), Restores: []model.ArchiveRestore{}}
	now := s.now()
	var bytes int64
	provider := model.ProviderAWSS3
	for _, c := range members {
		if !c.Asset.NeedsRestore(c.Provider, now) {
			out.Readable++
			continue
		}
		provider = c.Provider
		x, created, err := s.deps.Store.CreateRestore(ctx, model.ArchiveRestore{ID: newID(), TenantID: p.TenantID, AssetID: c.Asset.ID,
			VersionID: versionID, Days: in.Days, Speed: in.Speed, RequestedBy: p.UserID, CreatedAt: now})
		if err != nil {
			return out, err
		}
		if created {
			out.Requested++
			bytes += c.Asset.SizeBytes
		} else {
			out.AlreadyOpen++
		}
		out.Restores = append(out.Restores, x)
	}
	out.Estimate = estimate(EstimateInput{Operation: "restore", Bytes: bytes, Files: int64(out.Requested), Speed: in.Speed}, model.PricesFor(provider))
	s.record(ctx, p, "dataset.restore_archived", "dataset_version", versionID,
		map[string]any{"requested": out.Requested, "days": in.Days, "speed": in.Speed})
	return out, nil
}

func (s *Service) Restores(ctx context.Context, p model.Principal, assetID, versionID string) ([]model.ArchiveRestore, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAssetsRead, "asset.restores_read", "organization", p.TenantID); err != nil {
		return nil, err
	}
	return s.deps.Store.Restores(ctx, p.TenantID, assetID, versionID, 200)
}

// restores starts pending restores, finishes those the provider has made
// readable, and ends those that lapsed.
func (s *Service) restores(ctx context.Context, st *Stats) error {
	n, err := s.deps.Store.ExpireRestores(ctx, s.now())
	st.RestoresExpired += n
	if err != nil {
		return err
	}
	open, err := s.deps.Store.OpenRestores(ctx, 500)
	if err != nil {
		return err
	}
	var errs []error
	for _, x := range open {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, s.advance(ctx, x, st))
	}
	return errors.Join(errs...)
}

func (s *Service) advance(ctx context.Context, x model.ArchiveRestore, st *Stats) error {
	fail := func(reason string) error {
		x.State, x.Error, x.UpdatedAt = "FAILED", reason, s.now()
		st.RestoresFailed++
		return s.deps.Store.UpdateRestore(ctx, x)
	}
	a, err := s.deps.Assets.GetByID(ctx, x.TenantID, x.AssetID)
	if err != nil || a.Status != model.AssetStatusAvailable {
		return fail("the asset is gone")
	}
	provider, err := s.deps.Store.ProviderOf(ctx, a.TenantID, a.StorageConnectionID)
	if err != nil {
		return err
	}
	loc, err := s.deps.Router.Locate(ctx, a.TenantID, connectionOf(*a))
	if err != nil {
		return nil // tried again next pass
	}
	// A packed file is restored by restoring its pack.
	bucket, key, version := loc.ReadyBucket, a.ObjectKey, a.StorageVersionID
	pack, packed, err := s.deps.Store.PackOf(ctx, a.TenantID, a.ID)
	if err != nil {
		return err
	}
	if packed {
		bucket, key, version = pack.Bucket, pack.ObjectKey, pack.VersionID
	}
	tierer, ok := loc.Storage.(interfaces.TierStorage)
	if !ok || !model.ArchiveNeedsRestore(provider, a.StorageTier) {
		// Nothing to restore: the object reads as it is.
		now := s.now()
		x.State, x.ReadyAt, x.UpdatedAt = "READY", &now, now
		st.RestoresReady++
		return s.deps.Store.UpdateRestore(ctx, x)
	}
	if x.State == "PENDING" {
		if err := tierer.Restore(ctx, bucket, key, version, x.Days, x.Speed); err != nil {
			if retry.IsPermanent(err) {
				return fail("the provider refused the restore: " + retry.Sanitize(err))
			}
			return nil
		}
		x.State, x.UpdatedAt = "RESTORING", s.now()
		st.RestoresStarted++
		return s.deps.Store.UpdateRestore(ctx, x)
	}
	state, err := tierer.TierState(ctx, bucket, key, version)
	if err != nil || state.Restoring {
		return nil
	}
	now := s.now()
	until := state.ReadableUntil
	if until == nil {
		if model.ArchiveNeedsRestore(provider, state.Tier) {
			return nil // not started on the provider's side yet
		}
		// Rehydrated to a tier that reads directly (Azure).
		far := now.AddDate(100, 0, 0)
		until = &far
	}
	if packed {
		err = s.deps.Store.SetPackReadable(ctx, a.TenantID, pack.ID, state.Tier, until, now)
	} else {
		err = s.deps.Store.SetReadable(ctx, a.TenantID, a.ID, state.Tier, until, now)
	}
	if err != nil {
		return err
	}
	x.State, x.ReadyAt, x.ReadableUntil, x.UpdatedAt = "READY", &now, until, now
	st.RestoresReady++
	s.deps.Log.Info("archived asset readable", "tenant_id", a.TenantID, "asset_id", a.ID, "until", until.Format(time.RFC3339))
	return s.deps.Store.UpdateRestore(ctx, x)
}

// Archived tells delivery whether an asset must be restored before it is
// read (model.ErrRestoreRequired).
func (s *Service) Archived(ctx context.Context, a model.Asset) error {
	if a.StorageTier != model.TierArchive && a.StorageTier != model.TierDeepArchive {
		return nil
	}
	provider, err := s.deps.Store.ProviderOf(ctx, a.TenantID, a.StorageConnectionID)
	if err != nil {
		return err
	}
	if a.NeedsRestore(provider, s.now()) {
		return model.ErrRestoreRequired
	}
	return nil
}
