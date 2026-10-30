package processing

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Quarantine reads and releases the processing records of quarantined
// assets. It needs only the repositories, so the API can serve it without
// running processing itself.
type Quarantine struct {
	assets  interfaces.AssetRepository
	jobs    interfaces.ProcessingJobRepository
	records interfaces.QuarantineRepository
	events  interfaces.ProcessingEventRepository
	audit   interfaces.AuditRecorder
	now     func() time.Time
	// findings is optional; with it, inspections include content findings.
	findings interface {
		ListFindings(ctx context.Context, tenantID, assetID string) ([]model.ContentFinding, error)
	}
	checks interfaces.ContentChecks
}

// WithChecks adds content findings to inspections, and serves blast
// radius and the tenant content policy.
func (q *Quarantine) WithChecks(checks interfaces.ContentChecks) *Quarantine {
	q.findings, q.checks = checks, checks
	return q
}

func NewQuarantine(assets interfaces.AssetRepository, jobs interfaces.ProcessingJobRepository,
	records interfaces.QuarantineRepository, events interfaces.ProcessingEventRepository, recorder interfaces.AuditRecorder) *Quarantine {
	return &Quarantine{assets: assets, jobs: jobs, records: records, events: events, audit: recorder,
		now: func() time.Time { return time.Now().UTC() }}
}

// Inspect returns an asset's processing record: its job, quarantine
// records and processing events.
func (q *Quarantine) Inspect(ctx context.Context, tenantID, assetID string) (Inspection, error) {
	var out Inspection
	var err error
	if out.Asset, err = q.assets.GetByID(ctx, tenantID, assetID); err != nil {
		return out, err
	}
	if out.Job, err = q.jobs.GetByAsset(ctx, tenantID, assetID); err != nil {
		return out, err
	}
	if out.Quarantine, err = q.records.ListForAsset(ctx, tenantID, assetID); err != nil {
		return out, err
	}
	if q.findings != nil {
		if out.Findings, err = q.findings.ListFindings(ctx, tenantID, assetID); err != nil {
			return out, err
		}
	}
	out.Events, err = q.events.ListForAsset(ctx, tenantID, assetID)
	return out, err
}

// Requeue gives an EXHAUSTED job a fresh attempt budget, from its recorded
// stage. Its HELD record stays open until the job succeeds or is rejected.
// A soft-deleted asset is not requeued: its deletion owns it now.
func (q *Quarantine) Requeue(ctx context.Context, tenantID, assetID, actor string) (model.ProcessingJob, error) {
	asset, err := q.assets.GetByID(ctx, tenantID, assetID)
	if err != nil {
		return model.ProcessingJob{}, err
	}
	if asset.Status != model.AssetStatusQuarantined && asset.Status != model.AssetStatusProcessing {
		return model.ProcessingJob{}, model.ErrInvalidAssetState
	}
	job, err := q.jobs.Requeue(ctx, tenantID, assetID, q.now())
	if err != nil {
		return job, err
	}
	// The job is requeued either way; a lost trail entry only loses detail.
	_ = q.events.Record(ctx, model.ProcessingEvent{
		TenantID: job.TenantID, AssetID: job.AssetID, JobID: job.ID, Event: "requeued", Actor: actor, CreatedAt: q.now(),
	})
	return job, nil
}

// InspectFor is Inspect for a member allowed to see quarantine records.
func (q *Quarantine) InspectFor(ctx context.Context, p model.Principal, assetID string) (Inspection, error) {
	if err := audit.Require(ctx, q.audit, p, model.PermQuarantineRead, "quarantine.read", "asset", assetID); err != nil {
		return Inspection{}, err
	}
	return q.Inspect(ctx, p.TenantID, assetID)
}

// RequeueFor releases a held asset for another processing attempt, for a
// member allowed to. The scan still decides: a release never promotes
// anything by itself.
func (q *Quarantine) RequeueFor(ctx context.Context, p model.Principal, assetID string) (model.ProcessingJob, error) {
	if err := audit.Require(ctx, q.audit, p, model.PermQuarantineRelease, "quarantine.release", "asset", assetID); err != nil {
		return model.ProcessingJob{}, err
	}
	job, err := q.Requeue(ctx, p.TenantID, assetID, "user:"+p.UserID)
	if err == nil && q.audit != nil {
		q.audit.Record(ctx, audit.From(ctx, p, "quarantine.release", "asset", assetID))
	}
	return job, err
}
