package copyengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ScrubStats counts one scrub pass.
type ScrubStats struct {
	Checked, Damaged, Repaired int
}

// Scrub re-reads and re-hashes the next members of datasets that scrub
// every N days, so each is read once in that time. A damaged or missing
// object is rewritten from a verified replica when there is one; either
// way it is recorded, raised as an alert and sent as the webhook
// object.scrub_failed. A provider that does not answer says nothing about
// the object: it is tried again next time.
func (s *Service) Scrub(ctx context.Context) (ScrubStats, error) {
	var stats ScrubStats
	due, err := s.deps.Store.ScrubDue(ctx, s.now(), s.cfg.Batch)
	if err != nil {
		return stats, err
	}
	for _, a := range due {
		err := s.reread(ctx, a, false)
		if err != nil && strings.HasPrefix(err.Error(), "UNREADABLE") {
			continue
		}
		result := model.ScrubResult{AssetID: a.AssetID, CheckedAt: s.now(), OK: err == nil}
		stats.Checked++
		if err != nil {
			stats.Damaged++
			result.Detail = truncate(err.Error(), 300)
			if repairErr := s.repair(ctx, a); repairErr == nil {
				stats.Repaired++
				result.Repaired = true
				result.Detail += "; rewritten from a verified replica"
			} else {
				result.Detail += "; not repaired: " + truncate(repairErr.Error(), 200)
			}
			if s.deps.Alerts != nil {
				_, _ = s.deps.Alerts.AddAlert(ctx, model.SecurityAlert{
					ID: s.newID(), TenantID: a.TenantID, PrincipalID: "asset:" + a.AssetID, Kind: model.AlertScrubMismatch,
					Detail: "a scrub found an object damaged or missing: " + result.Detail, State: "OPEN",
					Metrics:  map[string]any{"asset_id": a.AssetID, "repaired": result.Repaired},
					DedupKey: "scrub:" + a.AssetID + ":" + s.now().Format("2006-01-02"), CreatedAt: s.now(),
				})
			}
		}
		if err := s.deps.Store.PutScrub(ctx, a.TenantID, result); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// repair writes a verified replica's bytes back over an asset's damaged
// or missing object, and points the asset at the rewritten version.
// Packed assets are left to the pack rewrite.
func (s *Service) repair(ctx context.Context, a postgres.StoredAsset) error {
	if s.deps.Packs != nil {
		if _, packed, err := s.deps.Packs.PackedLocation(ctx, a.TenantID, a.AssetID); err != nil || packed {
			return errors.New("the object is packed; the pack rewrite repairs it")
		}
	}
	rep, ok, err := s.deps.Store.VerifiedReplica(ctx, a.TenantID, a.AssetID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no verified replica")
	}
	dst, err := s.deps.Router.Locate(ctx, a.TenantID, a.ConnectionID)
	if err != nil {
		return err
	}
	meta, err := s.copyFrom(ctx, func(ctx context.Context) (io.ReadCloser, error) { return s.openReplica(ctx, rep) }, a, dst, a.ObjectKey)
	if err != nil {
		return err
	}
	return s.deps.Store.Repaired(ctx, a.TenantID, a.AssetID, meta.VersionID, meta.ETag, s.now())
}

// ScrubFailuresFor lists a dataset's members whose last scrub failed or
// repaired them.
func (s *Service) ScrubFailuresFor(ctx context.Context, p model.Principal, datasetID string) ([]model.ScrubResult, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead, "dataset.scrubs.read", "dataset", datasetID); err != nil {
		return nil, err
	}
	return s.deps.Store.ScrubFailures(ctx, p.TenantID, datasetID, 500)
}

// ─── Clean points ─────────────────────────────────────────────────────

// SetCleanPointFor marks the last snapshot known good, before a
// compromise; "" clears it.
func (s *Service) SetCleanPointFor(ctx context.Context, p model.Principal, datasetID, versionID string) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.clean_point", "dataset", datasetID); err != nil {
		return err
	}
	return s.deps.Store.SetCleanVersion(ctx, p.TenantID, datasetID, versionID)
}

// CleanDiff is everything that changed since the clean point, with who
// uploaded each new version.
type CleanDiff struct {
	CleanVersionID string               `json:"clean_version_id"`
	Changes        []postgres.CleanDiff `json:"changes"`
}

func (s *Service) CleanDiffFor(ctx context.Context, p model.Principal, datasetID string) (CleanDiff, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead, "dataset.clean_point.diff", "dataset", datasetID); err != nil {
		return CleanDiff{}, err
	}
	prot, _, err := s.deps.Store.Protection(ctx, p.TenantID, datasetID)
	if err != nil {
		return CleanDiff{}, err
	}
	out := CleanDiff{CleanVersionID: prot.CleanVersionID, Changes: []postgres.CleanDiff{}}
	if prot.CleanVersionID == "" {
		return out, fmt.Errorf("%w: mark a clean point first", model.ErrInvalidCopyJob)
	}
	changes, err := s.deps.Store.DiffSinceClean(ctx, p.TenantID, datasetID, prot.CleanVersionID, 10000)
	if err != nil {
		return out, err
	}
	out.Changes = append(out.Changes, changes...)
	return out, nil
}

// ─── Export schedule ──────────────────────────────────────────────────

func (s *Service) ScheduleFor(ctx context.Context, p model.Principal) (postgres.ExportSchedule, bool, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermAuditRead, "organization.export_schedule.read", "organization", p.TenantID); err != nil {
		return postgres.ExportSchedule{}, false, err
	}
	return s.deps.Store.Schedule(ctx, p.TenantID)
}

// SetScheduleFor sets, or with intervalHours 0 removes, the scheduled
// export bundle.
func (s *Service) SetScheduleFor(ctx context.Context, p model.Principal, connectionID string, intervalHours int) error {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermOrgManage|model.PermAuditRead, "organization.export_schedule.update",
		"organization", p.TenantID); err != nil {
		return err
	}
	if intervalHours == 0 {
		return s.deps.Store.DeleteSchedule(ctx, p.TenantID)
	}
	if intervalHours < 1 || intervalHours > 8760 {
		return fmt.Errorf("%w: interval_hours is 1 to 8760", model.ErrInvalidCopyJob)
	}
	if _, err := s.deps.Router.Locate(ctx, p.TenantID, connectionID); err != nil {
		return err
	}
	return s.deps.Store.PutSchedule(ctx, postgres.ExportSchedule{TenantID: p.TenantID, ConnectionID: connectionID,
		IntervalHours: intervalHours, UpdatedBy: p.UserID, UpdatedAt: s.now()})
}
