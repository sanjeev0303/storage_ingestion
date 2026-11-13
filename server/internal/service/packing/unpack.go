package packing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/compaction"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// UnpackStats is what an unpack did, or with a dry run would do.
type UnpackStats struct {
	Packs   int
	Entries int
	// Copied entries were written back from their pack; Standalone ones
	// still had their own object, so only the record changed.
	Copied      int
	Standalone  int
	BytesCopied int64
	// Skipped entries were deleted or changed while they were unpacked.
	Skipped int
}

// unpackStore is what unpacking needs from the pack repository besides
// PackRepository.
type unpackStore interface {
	PublishedPacks(ctx context.Context, tenantID string) ([]model.Pack, error)
	UnpackEntry(ctx context.Context, e model.PackEntry, oldETag, etag, versionID string, now time.Time) (bool, error)
}

// Unpack makes every asset served from the tenant's published packs (or
// from packID only) its own object again and retires the packs, so packing
// can be rolled back (docs/packing.md §11). Each entry is copied out of its
// pack, checked against its recorded size and SHA-256, written to the
// asset's own key and read back, and only then is the asset pointed at it.
// Run it with the pack worker stopped, which would otherwise pack the
// assets again. It stops at the first error; running it again carries on.
func (s *Service) Unpack(ctx context.Context, tenantID, packID string, dryRun bool) (UnpackStats, error) {
	var stats UnpackStats
	store, ok := s.deps.Packs.(unpackStore)
	if !ok {
		return stats, errors.New("this pack repository cannot unpack")
	}
	var packs []model.Pack
	if packID != "" {
		p, err := s.deps.Packs.GetPack(ctx, tenantID, packID)
		if err != nil {
			return stats, err
		}
		if p.State != model.PackPublished {
			return stats, fmt.Errorf("pack %s is %s: only a published pack serves assets", p.ID, p.State)
		}
		packs = []model.Pack{p}
	} else {
		var err error
		if packs, err = store.PublishedPacks(ctx, tenantID); err != nil {
			return stats, err
		}
	}
	for _, p := range packs {
		if err := s.unpackPack(ctx, store, p, dryRun, &stats); err != nil {
			return stats, fmt.Errorf("unpack pack %s: %w", p.ID, err)
		}
		stats.Packs++
	}
	return stats, nil
}

func (s *Service) unpackPack(ctx context.Context, store unpackStore, p model.Pack, dryRun bool, stats *UnpackStats) error {
	loc, err := s.deps.Router.Locate(ctx, p.TenantID, p.ConnectionID)
	if err != nil {
		return fmt.Errorf("resolve storage connection: %w", err)
	}
	entries, err := s.deps.Packs.Entries(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.State != model.PackEntryPublished {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		stats.Entries++
		asset, err := s.deps.Assets.GetByID(ctx, p.TenantID, e.AssetID)
		if err != nil {
			return err
		}
		standalone, err := hasOwnObject(ctx, loc, *asset)
		if err != nil {
			return err
		}
		if dryRun {
			if standalone {
				stats.Standalone++
			} else {
				stats.Copied++
				stats.BytesCopied += e.SizeBytes
			}
			continue
		}

		etag, versionID := asset.StorageETag, asset.StorageVersionID
		if !standalone {
			meta, err := s.copyOut(ctx, p, loc, e, *asset)
			if err != nil {
				return fmt.Errorf("asset %s: %w", e.AssetID, err)
			}
			etag, versionID = meta.ETag, meta.VersionID
		}
		done, err := store.UnpackEntry(ctx, e, asset.StorageETag, etag, versionID, s.now())
		if err != nil {
			return err
		}
		if !done {
			// Deleted, or moved by a rewrite, while it was copied: the copy
			// is nobody's bytes. Unless the asset now records another
			// object at its key (an unpack run alongside), it goes.
			if !standalone {
				if err := s.dropCopy(ctx, loc, *asset); err != nil {
					return err
				}
			}
			stats.Skipped++
			continue
		}
		if standalone {
			stats.Standalone++
		} else {
			stats.Copied++
			stats.BytesCopied += e.SizeBytes
		}
	}
	s.deps.Log.Info("pack unpacked", "pack_id", p.ID, "tenant_id", p.TenantID, "dry_run", dryRun)
	return nil
}

// hasOwnObject reports whether the asset's key still holds exactly the
// object the asset records: its source was never retired. Anything else at
// the key (an earlier unpack's copy it never recorded) is written over.
func hasOwnObject(ctx context.Context, loc interfaces.StorageLocation, a model.Asset) (bool, error) {
	var meta interfaces.ObjectMetadata
	var err error
	if a.StorageVersionID != "" {
		meta, err = loc.Storage.HeadObjectVersion(ctx, loc.ReadyBucket, a.ObjectKey, a.StorageVersionID)
	} else {
		meta, err = loc.Storage.HeadObject(ctx, loc.ReadyBucket, a.ObjectKey)
	}
	if errors.Is(err, interfaces.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect the asset's object: %w", err)
	}
	return meta.ETag == a.StorageETag, nil
}

// copyOut writes an entry's bytes to its asset's own key. They are checked
// against the recorded size and SHA-256 as they leave the pack, and again
// when the written object is read back, as a pack is verified.
func (s *Service) copyOut(ctx context.Context, p model.Pack, loc interfaces.StorageLocation, e model.PackEntry, a model.Asset) (interfaces.ObjectMetadata, error) {
	body, err := loc.Storage.OpenRange(ctx, p.Bucket, p.ObjectKey, e.Offset, e.Offset+e.Length-1,
		interfaces.ReadCondition{IfMatch: p.StorageETag, VersionID: p.StorageVersionID})
	if err != nil {
		return interfaces.ObjectMetadata{}, fmt.Errorf("read the entry from its pack: %w", err)
	}
	data, err := io.ReadAll(compaction.VerifiedEntry(body, e.SizeBytes, e.ChecksumSHA256))
	body.Close()
	if err != nil {
		return interfaces.ObjectMetadata{}, fmt.Errorf("read the entry from its pack: %w", err)
	}

	meta, err := loc.Storage.WriteObject(ctx, loc.ReadyBucket, a.ObjectKey, bytes.NewReader(data), int64(len(data)), a.ContentType)
	if err != nil {
		return meta, fmt.Errorf("write the asset's object: %w", err)
	}
	back, err := loc.Storage.OpenRange(ctx, loc.ReadyBucket, a.ObjectKey, 0, e.SizeBytes-1,
		interfaces.ReadCondition{IfMatch: meta.ETag, VersionID: meta.VersionID})
	if err != nil {
		return meta, fmt.Errorf("read the asset's object back: %w", err)
	}
	_, err = io.Copy(io.Discard, compaction.VerifiedEntry(back, e.SizeBytes, e.ChecksumSHA256))
	back.Close()
	if err != nil {
		return meta, fmt.Errorf("read the asset's object back: %w", err)
	}
	return meta, nil
}

// dropCopy deletes a copy unpacking wrote but could not record, unless the
// asset has since been pointed at another object at its key.
func (s *Service) dropCopy(ctx context.Context, loc interfaces.StorageLocation, before model.Asset) error {
	now, err := s.deps.Assets.GetByID(ctx, before.TenantID, before.ID)
	if err != nil {
		return err
	}
	if now.StorageETag != before.StorageETag || now.StorageVersionID != before.StorageVersionID {
		return nil
	}
	if err := loc.Storage.DeleteObject(ctx, loc.ReadyBucket, before.ObjectKey); err != nil {
		return fmt.Errorf("delete an unrecorded copy: %w", err)
	}
	return nil
}
