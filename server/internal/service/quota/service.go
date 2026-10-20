// Package quota applies tenant quotas: the effective limits of a tenant,
// the reservation made when an upload starts, usage reports and their
// reconciliation.
package quota

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

type Service struct {
	repo     interfaces.QuotaRepository
	defaults model.TenantQuota
	audit    interfaces.AuditRecorder
	log      *slog.Logger
	now      func() time.Time

	// A short cache of effective policies, for the per-request rate
	// limit. Reservations always read the policy afresh.
	mu    sync.Mutex
	cache map[string]cachedPolicy
}

type cachedPolicy struct {
	policy model.TenantQuota
	until  time.Time
}

func NewService(repo interfaces.QuotaRepository, defaults model.TenantQuota, recorder interfaces.AuditRecorder, log *slog.Logger) *Service {
	return &Service{
		repo: repo, defaults: defaults, audit: recorder, log: log,
		now: func() time.Time { return time.Now().UTC() }, cache: map[string]cachedPolicy{},
	}
}

// Effective is the tenant's limits: its overrides, else the defaults.
func (s *Service) Effective(ctx context.Context, tenantID string) (model.TenantQuota, error) {
	q, err := s.repo.Policy(ctx, tenantID)
	if err != nil {
		return q, err
	}
	return q.Merge(s.defaults), nil
}

// Cached is Effective, at most 30 seconds old.
func (s *Service) Cached(ctx context.Context, tenantID string) (model.TenantQuota, error) {
	s.mu.Lock()
	c, ok := s.cache[tenantID]
	s.mu.Unlock()
	if ok && s.now().Before(c.until) {
		return c.policy, nil
	}
	q, err := s.Effective(ctx, tenantID)
	if err != nil {
		return q, err
	}
	s.mu.Lock()
	if len(s.cache) > 10000 {
		s.cache = map[string]cachedPolicy{}
	}
	s.cache[tenantID] = cachedPolicy{policy: q, until: s.now().Add(30 * time.Second)}
	s.mu.Unlock()
	return q, nil
}

// CreateReservedUpload stores an upload session with its quota
// reservation. A refusal is a *model.QuotaError, and is audited.
func (s *Service) CreateReservedUpload(ctx context.Context, upload *model.UploadSession, idempotencyKey string, multipart bool) error {
	limits, err := s.Effective(ctx, upload.TenantID)
	if err != nil {
		return err
	}
	err = s.repo.CreateReservedUpload(ctx, upload, idempotencyKey, limits, multipart)
	var qe *model.QuotaError
	if errors.As(err, &qe) {
		e := audit.Denied(ctx, model.Principal{TenantID: upload.TenantID, UserID: upload.OwnerID}, "quota.exceeded", "upload", upload.ID, qe.Limit)
		e.Details = map[string]any{"limit": qe.Limit, "max": qe.Max, "used": qe.Used, "requested": qe.Requested}
		s.audit.Record(ctx, e)
	}
	return err
}

// Report is a tenant's limits and usage.
type Report struct {
	Policy model.TenantQuota
	Usage  model.StorageUsage
}

// Get reports the caller's organization's quota and usage.
func (s *Service) Get(ctx context.Context, p model.Principal) (Report, error) {
	if err := audit.Require(ctx, s.audit, p, model.PermQuotaRead, "quota.read", "quota", p.TenantID); err != nil {
		return Report{}, err
	}
	return s.report(ctx, p.TenantID)
}

// GetFor reports any tenant's quota, for a platform administrator.
func (s *Service) GetFor(ctx context.Context, p model.Principal, tenantID string) (Report, error) {
	if err := p.RequirePlatform(model.PlatformManage); err != nil {
		return Report{}, err
	}
	return s.report(ctx, tenantID)
}

func (s *Service) report(ctx context.Context, tenantID string) (Report, error) {
	policy, err := s.Effective(ctx, tenantID)
	if err != nil {
		return Report{}, err
	}
	usage, err := s.repo.Usage(ctx, tenantID)
	return Report{Policy: policy, Usage: usage}, err
}

// Set replaces a tenant's overrides; zero fields fall back to the
// defaults. Only platform administrators set quotas. The change and its
// audit event (with the limits before and after) commit together.
func (s *Service) Set(ctx context.Context, p model.Principal, tenantID string, q model.TenantQuota) (model.TenantQuota, error) {
	if err := p.RequirePlatform(model.PlatformManage); err != nil {
		e := audit.Denied(ctx, p, "quota.update", "quota", tenantID, err.Error())
		e.TenantID = tenantID
		s.audit.Record(ctx, e)
		return model.TenantQuota{}, err
	}
	if negative(q) {
		return model.TenantQuota{}, errors.New("quota limits must not be negative")
	}
	before, err := s.repo.Policy(ctx, tenantID)
	if err != nil {
		return model.TenantQuota{}, err
	}
	e := audit.From(ctx, p, "quota.update", "quota", tenantID)
	e.TenantID = tenantID
	e.Details = map[string]any{"before": quotaDetails(before), "after": quotaDetails(q)}
	if err := s.repo.SetPolicy(ctx, tenantID, q, s.now(), e); err != nil {
		return model.TenantQuota{}, err
	}
	s.mu.Lock()
	delete(s.cache, tenantID)
	s.mu.Unlock()
	return q.Merge(s.defaults), nil
}

func negative(q model.TenantQuota) bool {
	return q.MaxStorageBytes < 0 || q.MaxActiveJobs < 0 || q.MaxActiveUploads < 0 || q.MaxPendingEntries < 0 ||
		q.MaxAssets < 0 || q.MaxObjectBytes < 0 || q.MaxActiveMultipart < 0 || q.MaxPendingProcessing < 0 ||
		q.MaxManifestEntries < 0 || q.MaxManifestBytes < 0 || q.MaxRequestsPerMinute < 0
}

func quotaDetails(q model.TenantQuota) map[string]any {
	return map[string]any{
		"max_storage_bytes": q.MaxStorageBytes, "max_assets": q.MaxAssets, "max_object_bytes": q.MaxObjectBytes,
		"max_active_uploads": q.MaxActiveUploads, "max_active_multipart": q.MaxActiveMultipart,
		"max_active_jobs": q.MaxActiveJobs, "max_pending_processing": q.MaxPendingProcessing,
		"max_pending_entries": q.MaxPendingEntries, "max_manifest_entries": q.MaxManifestEntries,
		"max_manifest_bytes": q.MaxManifestBytes, "max_requests_per_minute": q.MaxRequestsPerMinute,
	}
}

// ReconcileResult is what reconciling one tenant found.
type ReconcileResult struct {
	TenantID string
	Before   model.StorageUsage
	After    model.StorageUsage
	// Settled counts reservations released or consumed because their
	// upload had ended.
	Settled  int
	Repaired bool
}

// Reconcile settles a tenant's stale reservations and recounts its usage.
// It is for the worker and the operator CLI; Admin is the API's.
func (s *Service) Reconcile(ctx context.Context, tenantID string) (ReconcileResult, error) {
	before, after, settled, err := s.repo.Reconcile(ctx, tenantID, s.now())
	r := ReconcileResult{TenantID: tenantID, Before: before, After: after, Settled: settled}
	if err != nil {
		return r, err
	}
	r.Repaired = before.ReservedBytes != after.ReservedBytes || before.ReservedUploads != after.ReservedUploads ||
		before.StoredBytes != after.StoredBytes || before.AssetCount != after.AssetCount ||
		before.PendingDeletionBytes != after.PendingDeletionBytes
	if r.Repaired || settled > 0 {
		e := audit.System(ctx, "quota.reconcile", "quota", tenantID)
		e.TenantID = tenantID
		e.Details = map[string]any{
			"settled_reservations": settled,
			"reserved_before":      before.ReservedBytes, "reserved_after": after.ReservedBytes,
			"stored_before": before.StoredBytes, "stored_after": after.StoredBytes,
			"assets_before": before.AssetCount, "assets_after": after.AssetCount,
		}
		s.audit.Record(ctx, e)
		s.log.Warn("tenant usage reconciled", "tenant_id", tenantID, "settled", settled, "repaired", r.Repaired)
	}
	return r, nil
}

// ReconcileAll reconciles every tenant with usage, a page at a time.
func (s *Service) ReconcileAll(ctx context.Context) ([]ReconcileResult, error) {
	var results []ReconcileResult
	after := ""
	for {
		tenants, err := s.repo.TenantsWithUsage(ctx, after, 200)
		if err != nil {
			return results, err
		}
		for _, tenant := range tenants {
			r, err := s.Reconcile(ctx, tenant)
			if err != nil {
				return results, err
			}
			results = append(results, r)
		}
		if len(tenants) < 200 {
			return results, nil
		}
		after = tenants[len(tenants)-1]
	}
}

// AdminReconcile is Reconcile for a platform administrator.
func (s *Service) AdminReconcile(ctx context.Context, p model.Principal, tenantID string) (ReconcileResult, error) {
	if err := p.RequirePlatform(model.PlatformManage); err != nil {
		return ReconcileResult{}, err
	}
	return s.Reconcile(ctx, tenantID)
}
