package processing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// A CLEAN verdict holds for the signatures that gave it. When the
// scanner's signatures change, published assets scanned with older ones
// are scanned again, a batch per cycle. One now found infected is blocked:
// it keeps its lifecycle state (its catalog version and usage stay as they
// were) but nothing is served from it, a finding records why, and the
// object.flagged webhook fires. The blast-radius API then shows which
// snapshots pinned those bytes and who was given links to them.

// RescanStats counts one cycle's re-scans.
type RescanStats struct {
	Rescanned int
	Infected  int
}

// RescanStale re-scans at most limit published assets whose CLEAN verdict
// came from other signatures than the scanner's current ones. Without a
// scanner that reports its version, there is nothing to compare.
func (s *Service) RescanStale(ctx context.Context, limit int) (RescanStats, error) {
	var stats RescanStats
	if s.deps.Scanner == nil || s.deps.Checks == nil || limit <= 0 {
		return stats, nil
	}
	version := s.scannerVersion(ctx)
	if version == "" {
		return stats, nil
	}
	if err := s.deps.Scanner.Ping(ctx); err != nil {
		return stats, nil // the next cycle tries again
	}
	stale, err := s.deps.Checks.ClaimStaleScans(ctx, s.deps.Scanner.Name(), version, s.now(), limit)
	if err != nil {
		return stats, err
	}
	for _, st := range stale {
		if ctx.Err() != nil {
			break
		}
		infected, err := s.rescan(ctx, st, version)
		if err != nil {
			// The claim lapses in an hour and the asset is tried again.
			s.log.Warn("re-scan failed", "asset_id", st.AssetID, "tenant_id", st.TenantID, "error", err)
			s.deps.Metrics.Add("rescan_failed", 1)
			continue
		}
		stats.Rescanned++
		s.deps.Metrics.Add("rescanned", 1)
		if infected {
			stats.Infected++
			s.deps.Metrics.Add("rescan_infected", 1)
		}
	}
	return stats, nil
}

func (s *Service) rescan(ctx context.Context, st model.StaleScan, version string) (bool, error) {
	asset, err := s.deps.Assets.GetByID(ctx, st.TenantID, st.AssetID)
	if err != nil {
		return false, err
	}
	if asset.Status != model.AssetStatusAvailable {
		return false, nil
	}
	result, err := s.scanPublished(ctx, *asset)
	if err != nil {
		return false, err
	}
	job := model.ProcessingJob{ID: st.JobID, TenantID: st.TenantID, AssetID: st.AssetID}
	now := s.now()
	switch result.Outcome {
	case model.ScanClean:
		if err := s.deps.Checks.MarkRescanned(ctx, st.JobID, version, now); err != nil {
			return false, err
		}
		s.event(ctx, job, "rescanned", fmt.Sprintf("CLEAN with %s (was %s)", version, orNone(st.OldVersion)))
		return false, nil
	case model.ScanInfected:
	default:
		return false, fmt.Errorf("re-scan incomplete: %s %s", result.Outcome, result.Detail)
	}

	if err := s.deps.Checks.MarkRescanInfected(ctx, st.JobID, truncate(result.Signature, 200), now); err != nil {
		return false, err
	}
	finding := model.ContentFinding{Source: "rescan", Code: "malware.detected_on_rescan", Action: model.FindingReject,
		Detail: "found by " + s.deps.Scanner.Name() + " with signatures " + version + ", after the asset was published"}
	if err := s.deps.Checks.AddFinding(ctx, st.TenantID, st.AssetID, st.JobID, finding, now); err != nil {
		return false, err
	}
	if err := s.deps.Checks.BlockAsset(ctx, st.TenantID, st.AssetID, model.RejectMalwareDetected, now); err != nil {
		return false, err
	}
	s.event(ctx, job, "rescan_infected", "blocked: "+finding.Detail)
	s.log.Warn("published asset found infected on re-scan; blocked", "asset_id", st.AssetID, "tenant_id", st.TenantID,
		"scanner", s.deps.Scanner.Name(), "signature", truncate(result.Signature, 200))
	return true, nil
}

func orNone(v string) string {
	if v == "" {
		return "no recorded version"
	}
	return v
}

// scanPublished scans an asset's published bytes where reads are served
// from: its standalone object, or its range of a pack. The bytes must
// still hash to the asset's SHA-256, or the verdict is about other bytes.
func (s *Service) scanPublished(ctx context.Context, asset model.Asset) (interfaces.ScanResult, error) {
	storage, bucket := s.deps.Storage, s.cfg.ReadyBucket
	if bucket == "" {
		bucket = s.cfg.Bucket
	}
	if !model.IsPlatformConnection(asset.StorageConnectionID) {
		if s.deps.Router == nil {
			return interfaces.ScanResult{}, model.ErrStorageNotConfigured
		}
		loc, err := s.deps.Router.Locate(ctx, asset.TenantID, asset.StorageConnectionID)
		if err != nil {
			return interfaces.ScanResult{}, fmt.Errorf("resolve storage connection: %w", err)
		}
		storage, bucket = loc.Storage, loc.ReadyBucket
	}

	if s.deps.Packs != nil {
		loc, packed, err := s.deps.Packs.PackedLocation(ctx, asset.TenantID, asset.ID)
		if err != nil {
			return interfaces.ScanResult{}, err
		}
		if packed {
			return s.scanPacked(ctx, storage, loc, asset)
		}
	}

	var result interfaces.ScanResult
	var read int64
	var sum string
	var err error
	if asset.SizeBytes > s.cfg.ScanSegmentBytes {
		result, read, sum, err = s.scanSegments(ctx, storage, bucket, asset.ObjectKey, baseType(asset.ContentType))
	} else {
		result, read, sum, err = s.scanWhole(ctx, storage, bucket, asset.ObjectKey)
	}
	if err != nil {
		return result, err
	}
	if result.Outcome == model.ScanClean && (read != asset.SizeBytes || !strings.EqualFold(sum, asset.ChecksumSHA256)) {
		return result, errors.New("the published object no longer matches the asset's SHA-256")
	}
	return result, nil
}

func (s *Service) scanPacked(ctx context.Context, storage interfaces.MultipartStorage, loc model.PackedLocation, asset model.Asset) (interfaces.ScanResult, error) {
	reader, ok := storage.(interfaces.ObjectDelivery)
	if !ok {
		return interfaces.ScanResult{}, errors.New("the storage cannot serve ranges of a pack")
	}
	first, last, err := loc.Range(0, -1)
	if err != nil {
		return interfaces.ScanResult{}, err
	}
	scanCtx, cancel := context.WithTimeout(ctx, s.cfg.ScanTimeout)
	defer cancel()
	body, err := reader.OpenRange(scanCtx, loc.Bucket, loc.ObjectKey, first, last,
		interfaces.ReadCondition{IfMatch: loc.StorageETag, VersionID: loc.StorageVersionID})
	if err != nil {
		return interfaces.ScanResult{}, fmt.Errorf("open packed asset: %w", err)
	}
	defer body.Close()
	return s.deps.Scanner.Scan(scanCtx, body), nil
}
