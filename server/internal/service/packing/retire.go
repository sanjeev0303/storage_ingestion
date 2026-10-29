package packing

import (
	"context"
	"errors"
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// batch bounds each retirement step per cycle.
const batch = 500

// Retire does what follows publication: it deletes sources past their
// grace period, retires packs nothing is served from, rewrites packs
// holding deleted entries, deletes the objects of retired and failed
// packs, and reports pack objects nothing records.
func (s *Service) Retire(ctx context.Context, stats *CycleStats) error {
	var errs []error
	errs = append(errs, s.retireSources(ctx, stats))
	retired, err := s.deps.Packs.RetireEmptyPacks(ctx, s.now())
	stats.PacksRetired += retired
	s.deps.Metrics.Add("packs_retired", int64(retired))
	errs = append(errs, err)
	errs = append(errs, s.planRewrites(ctx, stats))
	errs = append(errs, s.removePacks(ctx, stats))
	errs = append(errs, s.scanOrphans(ctx, stats))
	return errors.Join(errs...)
}

// retireSources deletes the standalone copies of assets served from a pack
// for at least SourceRetireAfter, as long as the asset is still exactly the
// version the pack holds.
func (s *Service) retireSources(ctx context.Context, stats *CycleStats) error {
	sources, err := s.deps.Packs.SourcesToRetire(ctx, s.now().Add(-s.cfg.SourceRetireAfter), batch)
	if err != nil {
		return err
	}
	var errs []error
	for _, src := range sources {
		if ctx.Err() != nil {
			break
		}
		a, e := src.Asset, src.Entry
		same := a.ObjectKey == e.SourceKey && a.StorageETag == e.SourceETag &&
			(a.Status == model.AssetStatusAvailable || a.Status == model.AssetStatusSoftDeleted)
		if !same {
			s.deps.Log.Warn("packed asset changed since it was packed; its source is left alone",
				"pack_id", e.PackID, "asset_id", a.ID, "status", string(a.Status))
			continue
		}
		if s.deps.Shared != nil {
			user, err := s.deps.Shared.KeyInUse(ctx, a.TenantID, e.SourceKey, a.ID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if user != "" {
				// Another asset is served from these same bytes; they stay.
				continue
			}
		}
		loc, err := s.deps.Router.Locate(ctx, a.TenantID, a.StorageConnectionID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve storage connection: %w", err))
			continue
		}
		// The whole key goes, every version of it on a versioned bucket:
		// the bytes live on in the pack.
		if err := loc.Storage.DeleteObject(ctx, loc.ReadyBucket, e.SourceKey); err != nil {
			errs = append(errs, fmt.Errorf("delete packed source: %w", err))
			continue
		}
		if err := s.deps.Packs.MarkSourceRetired(ctx, e.PackID, e.Ordinal, s.now()); err != nil {
			errs = append(errs, err)
			continue
		}
		stats.SourcesRetired++
		s.deps.Metrics.Add("packing_sources_retired", 1)
	}
	return errors.Join(errs...)
}

// planRewrites plans, for packs holding enough deleted entries, a new pack
// of their live entries. Publishing it moves those entries and retires the
// old pack, whose deleted entries' bytes then go with it.
func (s *Service) planRewrites(ctx context.Context, stats *CycleStats) error {
	packs, err := s.deps.Packs.PacksToRewrite(ctx, s.cfg.RewriteDeadRatio, s.now().Add(-s.cfg.MaxDeletionDelay), s.cfg.ActiveWriters)
	if err != nil {
		return err
	}
	var errs []error
	for _, old := range packs {
		building, err := s.deps.Packs.BuildingPacks(ctx, old.TenantID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if building >= s.cfg.TenantActivePacks {
			continue // the tenant's turn comes when its packs are built
		}
		entries, err := s.deps.Packs.Entries(ctx, old.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var live []model.PackEntry
		for _, e := range entries {
			if e.State == model.PackEntryPublished {
				live = append(live, e)
			}
		}
		if len(live) == 0 {
			continue // retired whole instead
		}
		loc, err := s.place(ctx, old.TenantID, old.ConnectionID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = s.createPackFrom(ctx, old.TenantID, loc, old.ID, len(live), func(i int) model.PackEntry {
			e := live[i]
			return model.PackEntry{AssetID: e.AssetID, ObjectID: e.ObjectID, Version: e.Version, SizeBytes: e.SizeBytes,
				ChecksumSHA256: e.ChecksumSHA256, SourceKey: old.ObjectKey, SourceETag: old.StorageETag,
				SourceVersionID: old.StorageVersionID}
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		stats.Rewrites++
		s.deps.Metrics.Add("pack_rewrites_planned", 1)
	}
	return errors.Join(errs...)
}

// removePacks deletes the objects of retired and failed packs, after
// RemoveAfter: reads that resolved a retired pack just before it was
// retired may still be streaming from it.
func (s *Service) removePacks(ctx context.Context, stats *CycleStats) error {
	packs, err := s.deps.Packs.PacksToRemove(ctx, s.now().Add(-s.cfg.RemoveAfter), batch)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range packs {
		if ctx.Err() != nil {
			break
		}
		loc, err := s.deps.Router.Locate(ctx, p.TenantID, p.ConnectionID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve storage connection: %w", err))
			continue
		}
		if err := s.removeObjects(ctx, p, loc); err != nil {
			s.deps.Log.Warn("remove pack objects; retried next cycle", "pack_id", p.ID, "error", retry.Sanitize(err))
			errs = append(errs, err)
			continue
		}
		if err := s.deps.Packs.MarkPackDeleted(ctx, p.ID, p.State, s.now()); err != nil {
			errs = append(errs, err)
			continue
		}
		stats.PacksRemoved++
		s.deps.Metrics.Add("packs_removed", 1)
		s.deps.Log.Info("pack objects removed", "pack_id", p.ID, "tenant_id", p.TenantID, "was", string(p.State))
	}
	return errors.Join(errs...)
}

// scanOrphans lists, one page per place per cycle, the pack objects under
// each tenant's packs prefix. An object of a pack recorded as deleted is
// removed once RemoveAfter has passed; one no pack record names is only
// reported: nothing proves the platform wrote it.
func (s *Service) scanOrphans(ctx context.Context, stats *CycleStats) error {
	places, err := s.deps.Packs.PackPlaces(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, place := range places {
		if ctx.Err() != nil {
			break
		}
		loc, err := s.deps.Router.Locate(ctx, place.TenantID, place.ConnectionID)
		if err != nil {
			continue // unreachable now; its packs' own records still drive their cleanup
		}
		prefix := loc.ReadyPrefix + "/" + place.TenantID + "/packs/"
		cursorKey := place.TenantID + " " + place.ConnectionID
		s.mu.Lock()
		cursor := s.objectCursors[cursorKey]
		s.mu.Unlock()
		page, err := loc.Storage.ListObjectsPage(ctx, loc.ReadyBucket, prefix, cursor, 1000)
		if err != nil {
			errs = append(errs, fmt.Errorf("list pack objects: %w", err))
			continue
		}
		s.mu.Lock()
		s.objectCursors[cursorKey] = page.Cursor
		s.mu.Unlock()
		for _, o := range page.Objects {
			id, ok := isPackKey(o.ObjectKey, prefix)
			var p model.Pack
			if ok {
				p, err = s.deps.Packs.GetPack(ctx, place.TenantID, id)
			}
			switch {
			case !ok || errors.Is(err, model.ErrPackNotFound):
				stats.OrphansReported++
				s.deps.Metrics.Add("pack_orphan_objects", 1)
				s.deps.Log.Warn("an object under a packs prefix is not a recorded pack; left in place",
					"tenant_id", place.TenantID, "connection", place.ConnectionID, "key", o.ObjectKey)
			case err != nil:
				errs = append(errs, err)
			case p.State == model.PackDeleted && s.now().Sub(o.LastModified) >= s.cfg.RemoveAfter:
				if err := s.removeObjects(ctx, p, loc); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}
