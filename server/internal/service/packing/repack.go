package packing

import (
	"context"
	"errors"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// Repack plans the pending repacks: a dataset's packed entries, in path
// order, into new packs of the target size, a sample never split between
// two. Each pass plans what the tenant's free pack slots allow and the
// next resumes from the request's cursor. The packs are built and
// published like any other; publishing moves the entries out of their old
// packs, which retire once nothing is served from them.
func (s *Service) Repack(ctx context.Context, stats *CycleStats) error {
	requests, err := s.deps.Packs.PendingRepacks(ctx, s.cfg.TenantsPerCycle)
	if err != nil {
		return err
	}
	var errs []error
	for _, q := range requests {
		if ctx.Err() != nil {
			break
		}
		planned, err := s.repackOne(ctx, q)
		if err != nil {
			s.deps.Log.Warn("plan repack", "repack_id", q.ID, "dataset_id", q.DatasetID, "error", retry.Sanitize(err))
			errs = append(errs, err)
		}
		stats.Repacked += planned
	}
	return errors.Join(errs...)
}

func (s *Service) repackOne(ctx context.Context, q model.RepackRequest) (int, error) {
	building, err := s.deps.Packs.BuildingPacks(ctx, q.TenantID)
	if err != nil {
		return 0, err
	}
	free := s.cfg.TenantActivePacks - building
	if free <= 0 {
		return 0, nil // the tenant's turn comes when its packs are built
	}
	entries, err := s.deps.Packs.RepackEntries(ctx, q.TenantID, q.DatasetID, q.CursorKey, q.CursorAsset, s.cfg.MaxEntries)
	if err != nil {
		return 0, err
	}
	exhausted := len(entries) < s.cfg.MaxEntries
	chunks := repackChunks(entries, s.targetFor(ctx, q.TenantID), s.cfg.MaxPackBytes, s.cfg.MaxEntries)
	if !exhausted && len(chunks) > 1 {
		chunks = chunks[:len(chunks)-1] // it may carry on into the next page
	}

	cursorKey, cursorAsset := q.CursorKey, q.CursorAsset
	planned, moved, used := 0, 0, 0
	for _, chunk := range chunks {
		if planned == free {
			break
		}
		used++
		last := chunk[len(chunk)-1]
		if worthRepacking(chunk) {
			loc, err := s.place(ctx, q.TenantID, chunk[0].Pack.ConnectionID)
			if err != nil {
				return planned, err
			}
			err = s.createPackFrom(ctx, q.TenantID, loc, "", len(chunk), func(i int) model.PackEntry {
				e, from := chunk[i].Entry, chunk[i].Pack
				return model.PackEntry{AssetID: e.AssetID, ObjectID: e.ObjectID, Version: e.Version, SizeBytes: e.SizeBytes,
					ChecksumSHA256: e.ChecksumSHA256, SourceKey: from.ObjectKey, SourceETag: from.StorageETag,
					SourceVersionID: from.StorageVersionID, SourcePackID: from.ID, SourceOffset: e.Offset}
			})
			if err != nil {
				return planned, err
			}
			planned++
			moved += len(chunk)
		}
		cursorKey, cursorAsset = last.PathKey, last.Entry.AssetID
	}
	done := exhausted && used == len(chunks)
	if err := s.deps.Packs.AdvanceRepack(ctx, q.ID, cursorKey, cursorAsset, planned, moved, done, s.now()); err != nil {
		return planned, err
	}
	if planned > 0 || done {
		s.deps.Log.Info("repack planned", "repack_id", q.ID, "dataset_id", q.DatasetID, "packs", planned, "entries", moved, "done", done)
	}
	return planned, nil
}

// repackChunks splits entries, in path order, into packs: one connection
// each, filled to target, ending between samples, never past maxBytes or
// maxEntries.
func repackChunks(entries []interfaces.RepackEntry, target, maxBytes int64, maxEntries int) [][]interfaces.RepackEntry {
	var chunks [][]interfaces.RepackEntry
	var cur []interfaces.RepackEntry
	var total int64
	for _, e := range entries {
		if len(cur) > 0 {
			size := e.Entry.SizeBytes
			sameSample := sampleKey("", cur[len(cur)-1].PathKey) == sampleKey("", e.PathKey)
			mustEnd := e.Pack.ConnectionID != cur[0].Pack.ConnectionID || len(cur) == maxEntries || total+size > maxBytes
			if mustEnd || (total+size > target && !sameSample) {
				chunks = append(chunks, cur)
				cur, total = nil, 0
			}
		}
		cur = append(cur, e)
		total += e.Entry.SizeBytes
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// worthRepacking is false for a lone entry, and for a chunk that is
// already one whole pack in the same order: rewriting it changes nothing.
func worthRepacking(chunk []interfaces.RepackEntry) bool {
	if len(chunk) < 2 {
		return false
	}
	first := chunk[0]
	if first.PackLive != len(chunk) {
		return true
	}
	for i := 1; i < len(chunk); i++ {
		if chunk[i].Entry.PackID != first.Entry.PackID || chunk[i].Entry.Ordinal < chunk[i-1].Entry.Ordinal {
			return true
		}
	}
	return false
}
