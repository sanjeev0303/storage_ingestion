// Package packing stores many small published assets as one physical pack
// on their own storage connection, keeping every asset's identity: readers
// still resolve an asset by its ID and read exactly its bytes. A pack is
// planned (its entries reserved), written, read back and verified, then
// published in one transaction. Sources are retired only after a grace
// period; packs whose entries were deleted are rewritten or retired, so a
// deleted asset's bytes do not outlive its deletion for long.
package packing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

type Dependencies struct {
	Packs interfaces.PackRepository
	// Router reaches every connection, the platform's included.
	Router  interfaces.StorageRouter
	Metrics *metrics.Registry
	Log     *slog.Logger
}

type Config struct {
	config.Packing
	// Owner names this worker on the leases it takes.
	Owner string
}

type Service struct {
	deps Dependencies
	cfg  Config
	now  func() time.Time

	// Where planning resumes: the next tenant, and within each tenant the
	// next asset. Kept in this process only; a restart starts over, which
	// costs a rescan and nothing else.
	mu            sync.Mutex
	tenantCursor  string
	assetCursors  map[string]string
	objectCursors map[string]string
}

func NewService(deps Dependencies, cfg Config) (*Service, error) {
	if deps.Packs == nil || deps.Router == nil || deps.Log == nil {
		return nil, errors.New("packing dependencies are required")
	}
	if cfg.Owner == "" {
		return nil, errors.New("packing needs a worker owner")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Service{deps: deps, cfg: cfg, now: func() time.Time { return time.Now().UTC() },
		assetCursors: map[string]string{}, objectCursors: map[string]string{}}, nil
}

// CycleStats is what one cycle did.
type CycleStats struct {
	Planned         int
	Built           int
	Published       int
	Failed          int
	Retried         int
	SourcesRetired  int
	PacksRetired    int
	Rewrites        int
	PacksRemoved    int
	OrphansReported int
}

// RunOnce plans new packs, builds the due ones, and retires what packing
// no longer needs. A failing step is reported and the others still run.
func (s *Service) RunOnce(ctx context.Context) (CycleStats, error) {
	var stats CycleStats
	var errs []error
	errs = append(errs, s.Plan(ctx, &stats))
	if ctx.Err() == nil {
		errs = append(errs, s.BuildDue(ctx, &stats))
	}
	if ctx.Err() == nil {
		errs = append(errs, s.Retire(ctx, &stats))
	}
	return stats, errors.Join(errs...)
}

func (s *Service) retryPolicy() retry.Policy {
	return retry.Policy{BaseDelay: s.cfg.RetryBase, MaxDelay: s.cfg.RetryMax, MaxAttempts: s.cfg.MaxAttempts}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// packKeys are a pack's object and manifest keys under the connection's
// ready prefix, beside the tenant's published assets, so a connection
// scoped to its prefixes can hold them.
func packKeys(readyPrefix, tenantID, packID string) (string, string) {
	base := readyPrefix + "/" + tenantID + "/packs/" + packID
	return base + ".pack", base + ".manifest.json"
}

// place resolves a connection for packing. A tenant connection must do
// ranged reads, or its assets are never packed; the platform's always does.
func (s *Service) place(ctx context.Context, tenantID, connectionID string) (interfaces.StorageLocation, error) {
	loc, err := s.deps.Router.Locate(ctx, tenantID, connectionID)
	if err != nil {
		return loc, fmt.Errorf("resolve storage connection: %w", err)
	}
	if !model.IsPlatformConnection(connectionID) && !loc.Capabilities.RangeReads {
		return loc, fmt.Errorf("%w: storage connection %s does not do ranged reads", model.ErrUnsupportedProvider, connectionID)
	}
	return loc, nil
}

// ─── Planning ─────────────────────────────────────────────────────────

// Plan picks the next tenants in turn and plans at most one pack for each,
// from assets eligible now.
func (s *Service) Plan(ctx context.Context, stats *CycleStats) error {
	tenants, err := s.nextTenants(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, tenantID := range tenants {
		if ctx.Err() != nil {
			break
		}
		planned, err := s.planTenant(ctx, tenantID)
		if err != nil {
			s.deps.Log.Warn("plan pack", "tenant_id", tenantID, "error", retry.Sanitize(err))
			errs = append(errs, err)
		}
		if planned {
			stats.Planned++
		}
	}
	return errors.Join(errs...)
}

func (s *Service) nextTenants(ctx context.Context) ([]string, error) {
	if len(s.cfg.Tenants) > 0 {
		return s.cfg.Tenants, nil
	}
	s.mu.Lock()
	after := s.tenantCursor
	s.mu.Unlock()
	tenants, err := s.deps.Packs.PackingTenants(ctx, after, s.cfg.TenantsPerCycle)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(tenants) < s.cfg.TenantsPerCycle {
		s.tenantCursor = "" // round done; the next cycle starts over
	} else {
		s.tenantCursor = tenants[len(tenants)-1]
	}
	return tenants, nil
}

// planTenant reserves one pack's worth of the tenant's eligible assets on
// one connection: the first connection in the batch, in asset order, that
// fills a pack. A pack is planned when it reaches its target size or entry
// count, or when the tenant's candidates run out and at least two remain
// to pack together. A connection that cannot be packed is passed over, so
// it never holds up the tenant's others.
func (s *Service) planTenant(ctx context.Context, tenantID string) (bool, error) {
	building, err := s.deps.Packs.BuildingPacks(ctx, tenantID)
	if err != nil || building >= s.cfg.TenantActivePacks {
		return false, err
	}
	s.mu.Lock()
	after := s.assetCursors[tenantID]
	s.mu.Unlock()
	filter := interfaces.PackCandidateFilter{MinBytes: s.cfg.MinObjectBytes, MaxBytes: s.cfg.MaxObjectBytes,
		PublishedBefore: s.now().Add(-s.cfg.MinAge)}
	batch, err := s.deps.Packs.PackCandidates(ctx, tenantID, after, filter, s.cfg.MaxEntries)
	if err != nil {
		return false, err
	}
	exhausted := len(batch) < s.cfg.MaxEntries
	next := ""
	if !exhausted {
		next = batch[len(batch)-1].ID
	}
	defer func() {
		s.mu.Lock()
		s.assetCursors[tenantID] = next
		s.mu.Unlock()
	}()
	if len(batch) == 0 {
		return false, nil
	}

	tried := map[string]bool{}
	var errs []error
	for _, first := range batch {
		connectionID := first.StorageConnectionID
		if tried[connectionID] {
			continue
		}
		tried[connectionID] = true
		loc, err := s.place(ctx, tenantID, connectionID)
		if errors.Is(err, model.ErrUnsupportedProvider) {
			s.deps.Metrics.Add("packing_unsupported_connections", 1)
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}

		var chosen []model.Asset
		var total int64
		full := false
		for _, a := range batch {
			if a.StorageConnectionID != connectionID || a.ObjectKey != loc.ReadyPrefix+"/"+a.TenantID+"/"+a.ID {
				continue // another connection's, or not at its published key
			}
			if total+a.SizeBytes > s.cfg.TargetPackBytes || len(chosen) == s.cfg.MaxEntries {
				full = true
				break
			}
			chosen = append(chosen, a)
			total += a.SizeBytes
		}
		full = full || total >= s.cfg.TargetPackBytes || len(chosen) == s.cfg.MaxEntries
		if len(chosen) < 2 || (!full && !exhausted) {
			continue
		}
		if full && !exhausted {
			next = chosen[len(chosen)-1].ID // the rest of the batch comes next time
		}
		s.deps.Metrics.Add("packing_candidates_selected", int64(len(chosen)))
		return true, s.createPackFrom(ctx, tenantID, loc, "", len(chosen), func(i int) model.PackEntry {
			a := chosen[i]
			return model.PackEntry{AssetID: a.ID, ObjectID: a.ObjectID, Version: a.Version, SizeBytes: a.SizeBytes,
				ChecksumSHA256: a.ChecksumSHA256, SourceKey: a.ObjectKey, SourceETag: a.StorageETag, SourceVersionID: a.StorageVersionID}
		})
	}
	return false, errors.Join(errs...)
}

// createPackFrom reserves n entries for a new pack in one transaction, with
// the pack.planned event. Offsets are fixed now: entries follow each other
// in the order given.
func (s *Service) createPackFrom(ctx context.Context, tenantID string, loc interfaces.StorageLocation, rewriteOf string, n int, entry func(int) model.PackEntry) error {
	now := s.now()
	id := newID()
	key, manifestKey := packKeys(loc.ReadyPrefix, tenantID, id)
	pack := model.Pack{ID: id, TenantID: tenantID, ConnectionID: loc.ConnectionID, Bucket: loc.ReadyBucket,
		ObjectKey: key, ManifestKey: manifestKey, Format: model.PackFormat, FormatVersion: model.PackFormatVersion,
		RewriteOf: rewriteOf, MaxAttempts: s.cfg.MaxAttempts, CreatedAt: now}
	entries := make([]model.PackEntry, n)
	for i := range entries {
		e := entry(i)
		e.Ordinal, e.Offset, e.Length = i, pack.TotalBytes, e.SizeBytes
		pack.TotalBytes += e.SizeBytes
		entries[i] = e
	}
	err := s.deps.Packs.CreatePack(ctx, pack, entries, event.NewPackPlanned(tenantID, id, now))
	if errors.Is(err, model.ErrPackConflict) {
		// Another worker reserved one of them first; the next cycle plans
		// around it.
		s.deps.Metrics.Add("packing_plan_conflicts", 1)
		return nil
	}
	if err != nil {
		return err
	}
	s.deps.Metrics.Add("packs_planned", 1)
	s.deps.Log.Info("pack planned", "pack_id", id, "tenant_id", tenantID, "connection", loc.ConnectionID,
		"entries", n, "bytes", pack.TotalBytes, "rewrite_of", rewriteOf)
	return nil
}

// isPackKey reports whether key is a pack object or manifest, and its ID.
func isPackKey(key, prefix string) (string, bool) {
	name, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return "", false
	}
	for _, suffix := range []string{".pack", ".manifest.json"} {
		if id, ok := strings.CutSuffix(name, suffix); ok && id != "" && !strings.Contains(id, "/") {
			return id, true
		}
	}
	return "", false
}
