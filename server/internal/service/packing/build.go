package packing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/compaction"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// BuildDue claims due packs, up to ActiveWriters, and builds them at once.
func (s *Service) BuildDue(ctx context.Context, stats *CycleStats) error {
	packs, err := s.deps.Packs.ClaimPacks(ctx, s.cfg.Owner, s.now(), s.cfg.Lease, s.cfg.ActiveWriters)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range packs {
		wg.Add(1)
		go func(p model.Pack) {
			defer wg.Done()
			outcome := s.build(ctx, p)
			mu.Lock()
			defer mu.Unlock()
			stats.Built++
			switch outcome {
			case outcomePublished:
				stats.Published++
			case outcomeFailed:
				stats.Failed++
			case outcomeRetried:
				stats.Retried++
			}
		}(p)
	}
	wg.Wait()
	return nil
}

// BuildPack builds one pack now, as the pack.planned event asks. A pack
// another worker holds, or one waiting for its retry, is left alone.
func (s *Service) BuildPack(ctx context.Context, tenantID, packID string) (string, error) {
	p, ok, err := s.deps.Packs.ClaimPack(ctx, tenantID, packID, s.cfg.Owner, s.now(), s.cfg.Lease)
	if err != nil {
		return "", err
	}
	if !ok {
		return "the pack is held by another worker, waiting for its retry, or no longer being built", nil
	}
	return "pack " + s.build(ctx, p), nil
}

const (
	outcomePublished = "published"
	outcomeFailed    = "failed"
	outcomeRetried   = "retried"
	outcomeLost      = "lease lost"
	outcomeStopped   = "interrupted"
)

// build takes a claimed pack as far as it goes. Every step that touches
// storage is recorded before the next starts, so a crash anywhere resumes
// from the last recorded state: WRITING writes again from scratch,
// VERIFYING verifies what was written.
func (s *Service) build(ctx context.Context, p model.Pack) string {
	log := s.deps.Log.With("pack_id", p.ID, "tenant_id", p.TenantID, "state", string(p.State), "attempt", p.Attempts)
	started := time.Now()
	if p.Attempts > p.MaxAttempts {
		return s.fail(ctx, p, log, errors.New("the pack's attempts are used up"), nil)
	}

	workCtx, stop := s.leased(ctx, p, log)
	err := s.advance(workCtx, &p)
	lost := stop()

	switch {
	case ctx.Err() != nil:
		log.Info("packing interrupted; the lease expires and the job resumes")
		return outcomeStopped
	case lost || errors.Is(err, model.ErrLeaseLost):
		s.deps.Metrics.Add("packs_lease_lost", 1)
		log.Warn("pack lease lost; another worker owns the job")
		return outcomeLost
	case err == nil:
		s.deps.Metrics.Add("packs_published", 1)
		s.deps.Metrics.Add("packing_build_ms_total", time.Since(started).Milliseconds())
		return outcomePublished
	}

	var source *compaction.SourceError
	switch {
	case errors.As(err, &source) && errors.Is(err, compaction.ErrSourceMismatch):
		// A data-integrity failure, not a transient one: the stored bytes
		// are not the ones recorded. The asset is never packed again; in a
		// rewrite, the published pack itself is damaged, and full reads of
		// that entry fail their checksum until an operator restores it.
		s.deps.Metrics.Add("packing_source_integrity_failures", 1)
		log.Error("a source does not match its recorded size or checksum; it is excluded from packing",
			"asset_id", source.AssetID, "rewrite_of", p.RewriteOf)
		return s.fail(ctx, p, log, err, []string{source.AssetID})
	case errors.As(err, &source) && (errors.Is(err, interfaces.ErrObjectNotFound) || errors.Is(err, interfaces.ErrPreconditionFailed)) &&
		s.stillRecorded(ctx, p, source.AssetID):
		// The asset still names exactly the object the plan read, yet that
		// object is gone or replaced. Planned again, it would make the same
		// pack and fail the same way, forever. Like a mismatching source, it
		// is never packed again.
		s.deps.Metrics.Add("packing_source_integrity_failures", 1)
		log.Error("a source's object is missing or replaced while its asset still records it; it is excluded from packing",
			"asset_id", source.AssetID)
		return s.fail(ctx, p, log, err, []string{source.AssetID})
	case errors.As(err, &source) && (errors.Is(err, interfaces.ErrObjectNotFound) || errors.Is(err, interfaces.ErrPreconditionFailed)):
		// The source was deleted or replaced after the pack was planned: the
		// plan is stale, and no retry can read it. The pack fails now and its
		// assets are planned again as they are.
		s.deps.Metrics.Add("packing_sources_changed", 1)
		log.Warn("a source changed after the pack was planned", "asset_id", source.AssetID)
		return s.fail(ctx, p, log, err, nil)
	case errors.Is(err, model.ErrPackCorrupt):
		// What was written does not read back as written. The job does not
		// go back: it fails, and its assets are planned into a new pack.
		s.deps.Metrics.Add("pack_verification_failures", 1)
		return s.fail(ctx, p, log, err, nil)
	case retry.IsPermanent(err) || p.Attempts >= p.MaxAttempts:
		return s.fail(ctx, p, log, err, nil)
	}
	next := s.now().Add(s.retryPolicy().Delay(p.Attempts))
	if rerr := s.deps.Packs.RetryPack(ctx, p.ID, p.LeaseOwner, s.now(), next, retry.Sanitize(err)); rerr != nil {
		log.Error("reschedule pack", "error", rerr)
		return outcomeLost
	}
	s.deps.Metrics.Add("packs_retried", 1)
	log.Warn("packing failed; it retries", "next_attempt_at", next, "error", retry.Sanitize(err))
	return outcomeRetried
}

// stillRecorded reports whether the source the pack's entry was planned
// from is still the one on record: the asset's own object while the asset
// is AVAILABLE, or the pack it is copied from while that is still
// published.
// A lookup that fails answers no, so the plan is treated as stale, as before.
func (s *Service) stillRecorded(ctx context.Context, p model.Pack, assetID string) bool {
	entries, err := s.deps.Packs.Entries(ctx, p.ID)
	if err != nil {
		return false
	}
	var entry *model.PackEntry
	for i := range entries {
		if entries[i].AssetID == assetID {
			entry = &entries[i]
		}
	}
	if entry == nil {
		return false
	}
	// Copied from a pack (a rewrite or a repack): that pack must still be
	// the published pack the entry was planned from.
	if entry.SourcePackID != "" {
		old, err := s.deps.Packs.GetPack(ctx, p.TenantID, entry.SourcePackID)
		return err == nil && old.State == model.PackPublished && entry.SourceKey == old.ObjectKey &&
			entry.SourceETag == old.StorageETag && entry.SourceVersionID == old.StorageVersionID
	}
	asset, err := s.deps.Assets.GetByID(ctx, p.TenantID, assetID)
	if err != nil || asset.Status != model.AssetStatusAvailable {
		return false
	}
	return entry.SourceKey == asset.ObjectKey && entry.SourceETag == asset.StorageETag && entry.SourceVersionID == asset.StorageVersionID
}

func (s *Service) fail(ctx context.Context, p model.Pack, log *slog.Logger, cause error, excluded []string) string {
	if err := s.deps.Packs.FailPack(ctx, p.ID, p.LeaseOwner, s.now(), retry.Sanitize(cause), excluded); err != nil {
		log.Error("record failed pack", "error", err)
		return outcomeLost
	}
	s.deps.Metrics.Add("packs_failed", 1)
	log.Error("packing failed; its assets stay where they are and its objects are removed", "error", retry.Sanitize(cause))
	return outcomeFailed
}

// advance runs the pack's remaining steps.
func (s *Service) advance(ctx context.Context, p *model.Pack) error {
	loc, err := s.place(ctx, p.TenantID, p.ConnectionID)
	if err != nil {
		return err
	}
	entries, err := s.deps.Packs.Entries(ctx, p.ID)
	if err != nil {
		return err
	}
	if p.State == model.PackPlanned {
		if err := s.deps.Packs.AdvancePack(ctx, p.ID, p.LeaseOwner, model.PackPlanned, model.PackWriting, interfaces.PackChange{}, s.now()); err != nil {
			return err
		}
		p.State = model.PackWriting
	}
	if p.State == model.PackWriting {
		change, err := s.write(ctx, *p, loc, entries)
		if err != nil {
			return err
		}
		if err := s.deps.Packs.AdvancePack(ctx, p.ID, p.LeaseOwner, model.PackWriting, model.PackVerifying, change, s.now()); err != nil {
			return err
		}
		p.State, p.SHA256, p.StorageETag, p.StorageVersionID = model.PackVerifying, change.SHA256, change.StorageETag, change.StorageVersionID
	}
	manifestSum, err := s.verify(ctx, *p, loc, entries)
	if err != nil {
		return err
	}
	published, dead, err := s.deps.Packs.PublishPack(ctx, p.ID, p.LeaseOwner, manifestSum, s.now())
	if err != nil {
		return err
	}
	s.deps.Metrics.Add("pack_entries_published", int64(published))
	s.deps.Metrics.Add("pack_entries_dead_at_publication", int64(dead))
	s.deps.Log.Info("pack published", "pack_id", p.ID, "tenant_id", p.TenantID, "entries", published,
		"dead", dead, "bytes", p.TotalBytes)
	return nil
}

// write streams every source into the pack object, from scratch. Whatever
// an earlier attempt left at the pack's keys (an unfinished multipart
// upload, a whole object) is removed first: the keys are the pack's own.
func (s *Service) write(ctx context.Context, p model.Pack, loc interfaces.StorageLocation, entries []model.PackEntry) (interfaces.PackChange, error) {
	// The keys may hold a pack another worker has since built and published,
	// if this one was paused past its lease: the claim is confirmed just
	// before anything is deleted, and a lost one stops here.
	if err := s.deps.Packs.RenewPack(ctx, p.ID, p.LeaseOwner, s.now(), s.cfg.Lease); err != nil {
		return interfaces.PackChange{}, err
	}
	if err := s.removeObjects(ctx, p, loc); err != nil {
		return interfaces.PackChange{}, err
	}
	sources, err := s.sources(ctx, p, loc, entries)
	if err != nil {
		return interfaces.PackChange{}, err
	}
	stream := compaction.NewStream(ctx, sources, s.cfg.SourceConcurrency)
	defer stream.Close()
	meta, err := loc.Storage.WriteObject(ctx, p.Bucket, p.ObjectKey, stream, p.TotalBytes, "application/octet-stream")
	if err != nil {
		if serr := stream.Err(); serr != nil {
			return interfaces.PackChange{}, serr // the source's own error says more than the upload's
		}
		return interfaces.PackChange{}, fmt.Errorf("write pack: %w", err)
	}
	layout, sum, err := stream.Result()
	if err != nil {
		return interfaces.PackChange{}, err
	}
	for i, l := range layout {
		if l.Offset != entries[i].Offset || l.Length != entries[i].Length {
			return interfaces.PackChange{}, fmt.Errorf("%w: entry %d landed at %d+%d, not %d+%d", model.ErrPackCorrupt,
				i, l.Offset, l.Length, entries[i].Offset, entries[i].Length)
		}
	}
	if meta.SizeBytes != p.TotalBytes {
		return interfaces.PackChange{}, fmt.Errorf("%w: stored %d bytes of %d", model.ErrPackCorrupt, meta.SizeBytes, p.TotalBytes)
	}
	s.deps.Metrics.Add("packing_bytes_written", p.TotalBytes)
	return interfaces.PackChange{SHA256: sum, StorageETag: meta.ETag, StorageVersionID: meta.VersionID}, nil
}

// sources reads each entry from where its bytes are now: the asset's own
// object at its recorded version, or its entry in the pack it is copied
// from (a rewrite or a repack). Reads are pinned, so a replaced source
// fails instead of being packed.
func (s *Service) sources(ctx context.Context, p model.Pack, loc interfaces.StorageLocation, entries []model.PackEntry) ([]compaction.Source, error) {
	from := map[string]model.Pack{}
	out := make([]compaction.Source, len(entries))
	for i, e := range entries {
		bucket, key, first := p.Bucket, e.SourceKey, int64(0)
		cond := interfaces.ReadCondition{IfMatch: e.SourceETag, VersionID: e.SourceVersionID}
		if e.SourcePackID != "" {
			src, ok := from[e.SourcePackID]
			if !ok {
				var err error
				if src, err = s.deps.Packs.GetPack(ctx, p.TenantID, e.SourcePackID); err != nil {
					return nil, err
				}
				if src.ConnectionID != p.ConnectionID {
					return nil, retry.Permanent(fmt.Errorf("pack %s is on another connection", src.ID))
				}
				from[e.SourcePackID] = src
			}
			bucket, key, first = src.Bucket, src.ObjectKey, e.SourceOffset
			cond = interfaces.ReadCondition{IfMatch: src.StorageETag, VersionID: src.StorageVersionID}
		}
		size := e.SizeBytes
		out[i] = compaction.Source{AssetID: e.AssetID, Size: size, SHA256: e.ChecksumSHA256,
			Open: func(ctx context.Context) (io.ReadCloser, error) {
				s.deps.Metrics.Add("packing_bytes_read", size)
				return loc.Storage.OpenRange(ctx, bucket, key, first, first+size-1, cond)
			}}
	}
	return out, nil
}

// verify reads the stored pack back through the connection and checks
// every entry and the whole against the manifest, then writes the
// manifest and reads it back. It returns the manifest's SHA-256.
func (s *Service) verify(ctx context.Context, p model.Pack, loc interfaces.StorageLocation, entries []model.PackEntry) (string, error) {
	m := compaction.Manifest{Format: p.Format, FormatVersion: p.FormatVersion, PackID: p.ID, TenantID: p.TenantID,
		ConnectionID: p.ConnectionID, PackBytes: p.TotalBytes, PackSHA256: p.SHA256, EntryCount: len(entries)}
	for _, e := range entries {
		m.Entries = append(m.Entries, compaction.ManifestEntry{Ordinal: e.Ordinal, AssetID: e.AssetID, ObjectID: e.ObjectID,
			Version: e.Version, Offset: e.Offset, Length: e.Length, Size: e.SizeBytes, SHA256: e.ChecksumSHA256,
			Compression: compaction.CompressionNone})
	}
	if err := m.Validate(s.cfg.MaxEntries); err != nil {
		return "", err
	}

	cond := interfaces.ReadCondition{IfMatch: p.StorageETag, VersionID: p.StorageVersionID}
	body, err := loc.Storage.OpenRange(ctx, p.Bucket, p.ObjectKey, 0, p.TotalBytes-1, cond)
	if err != nil {
		return "", fmt.Errorf("read the pack back: %w", err)
	}
	err = compaction.VerifyPack(body, m)
	body.Close()
	if err != nil {
		return "", err
	}

	doc, err := m.Encode()
	if err != nil {
		return "", err
	}
	stored, err := loc.Storage.WriteObject(ctx, p.Bucket, p.ManifestKey, bytes.NewReader(doc), int64(len(doc)), "application/json")
	if err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	read, err := loc.Storage.OpenRange(ctx, p.Bucket, p.ManifestKey, 0, int64(len(doc))-1,
		interfaces.ReadCondition{IfMatch: stored.ETag, VersionID: stored.VersionID})
	if err != nil {
		return "", fmt.Errorf("read the manifest back: %w", err)
	}
	back, err := io.ReadAll(io.LimitReader(read, int64(len(doc))+1))
	read.Close()
	if err != nil {
		return "", err
	}
	if _, err := compaction.ParseManifest(back, s.cfg.MaxEntries); err != nil || !bytes.Equal(back, doc) {
		return "", fmt.Errorf("%w: the manifest does not read back as written", model.ErrPackCorrupt)
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:]), nil
}

// removeObjects deletes a pack's objects and any unfinished upload of
// them, and confirms they are gone. On a versioned bucket every version of
// the keys goes too.
func (s *Service) removeObjects(ctx context.Context, p model.Pack, loc interfaces.StorageLocation) error {
	for _, key := range []string{p.ObjectKey, p.ManifestKey} {
		uploads, err := loc.Storage.ListMultipartUploads(ctx, p.Bucket, key)
		if err != nil {
			return fmt.Errorf("list unfinished uploads: %w", err)
		}
		for _, id := range uploads {
			if err := loc.Storage.AbortMultipart(ctx, p.Bucket, key, id); err != nil && !errors.Is(err, interfaces.ErrStorageUploadNotFound) {
				return fmt.Errorf("abort unfinished upload: %w", err)
			}
		}
		if err := loc.Storage.DeleteObject(ctx, p.Bucket, key); err != nil {
			return fmt.Errorf("delete pack object: %w", err)
		}
		if _, err := loc.Storage.HeadObject(ctx, p.Bucket, key); !errors.Is(err, interfaces.ErrObjectNotFound) {
			return fmt.Errorf("pack object still present after delete: %v", err)
		}
	}
	return nil
}

// leased renews the pack's lease while it is built. The context ends when
// the lease is lost, when renewal has failed for a whole lease, or after
// MaxJobDuration. stop ends the renewal and reports whether the lease was
// lost.
func (s *Service) leased(ctx context.Context, p model.Pack, log *slog.Logger) (context.Context, func() bool) {
	leaseCtx, cancel := context.WithCancelCause(ctx)
	workCtx, cancelTimeout := context.WithTimeout(leaseCtx, s.cfg.MaxJobDuration)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(s.cfg.Lease / 3)
		defer ticker.Stop()
		renewed := time.Now()
		for {
			select {
			case <-done:
				return
			case <-workCtx.Done():
				return
			case <-ticker.C:
			}
			err := s.deps.Packs.RenewPack(workCtx, p.ID, p.LeaseOwner, s.now(), s.cfg.Lease)
			switch {
			case err == nil:
				renewed = time.Now()
			case errors.Is(err, model.ErrLeaseLost):
				cancel(model.ErrLeaseLost)
				return
			default:
				log.Warn("renew pack lease", "error", retry.Sanitize(err))
				if time.Since(renewed) >= s.cfg.Lease {
					cancel(model.ErrLeaseLost)
					return
				}
			}
		}
	}()
	return workCtx, func() bool {
		close(done)
		wg.Wait()
		lost := errors.Is(context.Cause(leaseCtx), model.ErrLeaseLost)
		cancelTimeout()
		cancel(nil)
		return lost
	}
}
