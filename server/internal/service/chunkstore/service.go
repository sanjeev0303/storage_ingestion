// Package chunkstore keeps files as chunks stored once (docs/chunks.md). An
// asset's original object stays until its chunks have been read back and hash
// to the asset's SHA-256, and for a grace period after that. Only files that
// really share bytes with others are taken; the rest stay whole.
package chunkstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/chunkread"
)

// Router finds an organization's storage.
type Router interface {
	Locate(ctx context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error)
}

type Deps struct {
	Store  *postgres.ChunkStorageRepository
	Packs  *postgres.PackRepository
	Router Router
	Log    *slog.Logger
	// SourceRetireAfter is how long an original stays once its chunks serve.
	SourceRetireAfter time.Duration
	// MinSharedPercent: a file sharing less with others stays whole, because
	// many small objects cost more per read and per request than one.
	MinSharedPercent int64
	// RecheckAfter: how long a file left whole waits to be looked at again.
	RecheckAfter time.Duration
	// UnusedAfter: how long a chunk with no users waits before it goes.
	UnusedAfter time.Duration
}

type Service struct {
	deps Deps
	now  func() time.Time
}

func New(deps Deps) *Service {
	if deps.SourceRetireAfter <= 0 {
		deps.SourceRetireAfter = 24 * time.Hour
	}
	if deps.MinSharedPercent <= 0 {
		deps.MinSharedPercent = 20
	}
	if deps.RecheckAfter <= 0 {
		deps.RecheckAfter = time.Hour
	}
	if deps.UnusedAfter <= 0 {
		deps.UnusedAfter = time.Hour
	}
	return &Service{deps: deps, now: func() time.Time { return time.Now().UTC() }}
}

// Stats is what one pass did.
type Stats struct {
	Converted, Skipped, Failed, Retired, Rehydrated, Deleted int
}

const batch = 50

// RunOnce does one pass: converts, retires originals, rehydrates, and deletes
// chunks nothing needs. A failure on one asset leaves the others to go on.
func (s *Service) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	var errs []error
	errs = append(errs, s.convert(ctx, &stats), s.retire(ctx, &stats), s.rehydrate(ctx, &stats), s.collect(ctx, &stats))
	return stats, errors.Join(errs...)
}

func chunkKey(readyPrefix, tenantID, sha string) string {
	return fmt.Sprintf("%s/%s/chunks/%s/%s", readyPrefix, tenantID, sha[:2], sha)
}

func (s *Service) convert(ctx context.Context, stats *Stats) error {
	candidates, err := s.deps.Store.Candidates(ctx, s.now().Add(-s.deps.RecheckAfter), batch)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		shared, err := s.deps.Store.SharedBytes(ctx, c.TenantID, c.SHA256)
		if err != nil {
			return err
		}
		if c.SizeBytes <= 0 || shared*100/c.SizeBytes < s.deps.MinSharedPercent {
			stats.Skipped++
			if err := s.deps.Store.Skip(ctx, c, fmt.Sprintf("%d of its %d bytes are shared with other files", shared, c.SizeBytes), s.now()); err != nil {
				return err
			}
			continue
		}
		if err := s.convertOne(ctx, c); err != nil {
			if errors.Is(err, postgres.ErrChunkBusy) || errors.Is(err, postgres.ErrChunkStateChanged) {
				continue // the next pass, or not this pass's any more
			}
			stats.Failed++
			s.deps.Log.Warn("chunk conversion failed; the object stays as it is", "asset_id", c.AssetID, "error", err)
			if err := s.deps.Store.Abandon(ctx, c, err.Error(), s.now()); err != nil {
				return err
			}
			continue
		}
		stats.Converted++
	}
	return nil
}

func (s *Service) convertOne(ctx context.Context, c postgres.ChunkCandidate) error {
	loc, err := s.deps.Router.Locate(ctx, c.TenantID, c.ConnectionID)
	if err != nil {
		return fmt.Errorf("resolve storage connection: %w", err)
	}
	// A bucket with Object Lock can keep what it is given past any use of it,
	// and what is locked there must be the object itself.
	if locker, ok := loc.Storage.(interfaces.ObjectLocker); ok {
		locked, err := locker.ObjectLockEnabled(ctx, loc.ReadyBucket)
		if err != nil {
			return fmt.Errorf("check object lock: %w", err)
		}
		if locked {
			return errors.New("the bucket has Object Lock on")
		}
	}
	extents, err := s.deps.Store.FileChunks(ctx, c.TenantID, c.SHA256)
	if err != nil {
		return err
	}
	if err := s.deps.Store.StartWriting(ctx, c, s.now()); err != nil {
		return err
	}
	body, err := loc.Storage.OpenRange(ctx, loc.ReadyBucket, c.ObjectKey, 0, -1, interfaces.ReadCondition{VersionID: c.VersionID})
	if err != nil {
		return fmt.Errorf("read the object: %w", err)
	}
	defer body.Close()
	seen := map[string]bool{}
	buf := make([]byte, 0, 8<<20)
	for i := range extents {
		e := &extents[i]
		if cap(buf) < e.Length {
			buf = make([]byte, e.Length)
		}
		buf = buf[:e.Length]
		if _, err := io.ReadFull(body, buf); err != nil {
			return fmt.Errorf("read chunk at byte %d: %w", e.Offset, err)
		}
		sum := sha256.Sum256(buf)
		if hex.EncodeToString(sum[:]) != e.SHA256 {
			return fmt.Errorf("%w: the object does not match its index at byte %d", model.ErrChunkCorrupt, e.Offset)
		}
		e.Key = chunkKey(loc.ReadyPrefix, c.TenantID, e.SHA256)
		if seen[e.SHA256] {
			continue
		}
		seen[e.SHA256] = true
		toWrite, err := s.deps.Store.AddRef(ctx, c, e.SHA256, e.Key, e.Length, s.now())
		if err != nil {
			return err
		}
		if !toWrite {
			continue
		}
		if _, err := loc.Storage.WriteObject(ctx, loc.ReadyBucket, e.Key, bytes.NewReader(buf), int64(e.Length), "application/octet-stream"); err != nil {
			return fmt.Errorf("write chunk: %w", err)
		}
		if err := s.deps.Store.MarkWritten(ctx, c, e.SHA256, s.now()); err != nil {
			return err
		}
	}
	// Read everything back as a reader will. Nothing serves from chunks until
	// they prove to be exactly this file.
	check, err := chunkread.Open(ctx, loc.Storage, loc.ReadyBucket, extents, 0, -1)
	if err != nil {
		return err
	}
	defer check.Close()
	whole := sha256.New()
	if _, err := io.Copy(whole, check); err != nil {
		return fmt.Errorf("read the chunks back: %w", err)
	}
	if got := hex.EncodeToString(whole.Sum(nil)); got != c.SHA256 {
		return fmt.Errorf("%w: the chunks hash to %s", model.ErrChunkCorrupt, got)
	}
	ok, err := s.deps.Store.Publish(ctx, c, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the asset changed while its chunks were written")
	}
	return nil
}

func (s *Service) retire(ctx context.Context, stats *Stats) error {
	sources, err := s.deps.Store.SourcesToRetire(ctx, s.now().Add(-s.deps.SourceRetireAfter), batch)
	if err != nil {
		return err
	}
	var errs []error
	for _, src := range sources {
		loc, err := s.deps.Router.Locate(ctx, src.TenantID, src.ConnectionID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := loc.Storage.DeleteObject(ctx, loc.ReadyBucket, src.SourceKey); err != nil {
			errs = append(errs, fmt.Errorf("delete original: %w", err))
			continue
		}
		if err := s.deps.Store.MarkSourceRetired(ctx, src.TenantID, src.AssetID, s.now()); err != nil {
			errs = append(errs, err)
			continue
		}
		stats.Retired++
	}
	return errors.Join(errs...)
}

// rehydrate makes an asset one object again. An original still there just
// serves again. Otherwise the bytes are written back, hashed on the way, and
// the asset only points at them once they are the file.
func (s *Service) rehydrate(ctx context.Context, stats *Stats) error {
	assets, err := s.deps.Store.ToRehydrate(ctx, batch)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range assets {
		err := s.rehydrateOne(ctx, a)
		if errors.Is(err, postgres.ErrChunkStateChanged) {
			continue // released meanwhile, or its original went: the next pass sees which
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("rehydrate %s: %w", a.AssetID, err))
			continue
		}
		stats.Rehydrated++
	}
	return errors.Join(errs...)
}

func (s *Service) rehydrateOne(ctx context.Context, a postgres.ChunkSource) error {
	if !a.Retired {
		return s.deps.Store.Rehydrated(ctx, a, false, "", "", s.now())
	}
	loc, err := s.deps.Router.Locate(ctx, a.TenantID, a.ConnectionID)
	if err != nil {
		return err
	}
	extents, found, err := s.deps.Packs.ChunkedExtents(ctx, a.TenantID, a.AssetID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	body, err := chunkread.Open(ctx, loc.Storage, loc.ReadyBucket, extents, 0, -1)
	if err != nil {
		return err
	}
	defer body.Close()
	whole := sha256.New()
	meta, err := loc.Storage.WriteObject(ctx, loc.ReadyBucket, a.ObjectKey, io.TeeReader(body, whole), a.SizeBytes, a.ContentType)
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(whole.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("%w: rehydrated bytes hash to %s", model.ErrChunkCorrupt, got)
	}
	return s.deps.Store.Rehydrated(ctx, a, true, meta.ETag, meta.VersionID, s.now())
}

func (s *Service) collect(ctx context.Context, stats *Stats) error {
	unused, err := s.deps.Store.UnusedChunks(ctx, s.now().Add(-s.deps.UnusedAfter), 500)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range unused {
		claimed, err := s.deps.Store.ClaimUnused(ctx, c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !claimed {
			continue
		}
		loc, err := s.deps.Router.Locate(ctx, c.TenantID, c.ConnectionID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := loc.Storage.DeleteObject(ctx, loc.ReadyBucket, c.ObjectKey); err != nil {
			errs = append(errs, fmt.Errorf("delete chunk: %w", err))
			continue
		}
		if err := s.deps.Store.ForgetChunk(ctx, c); err != nil {
			errs = append(errs, err)
			continue
		}
		stats.Deleted++
	}
	return errors.Join(errs...)
}
