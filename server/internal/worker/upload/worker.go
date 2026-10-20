// Package upload runs the bulk-ingestion worker loop: staged manifest
// reads, expired upload leases, due retries, processing outcomes,
// cancellations and reconciliation.
package upload

import (
	"context"
	"log/slog"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
)

// Stats counts what one round of sweeps handled.
type Stats struct {
	Manifests int `json:"manifests"`
	Expired   int `json:"expired_leases"`
	Retried   int `json:"due_retries"`
	Synced    int `json:"outcomes_checked"`
	Cancelled int `json:"cancelled"`
}

// RunOnce runs every sweep once. A failing sweep is logged and the others
// still run.
func RunOnce(ctx context.Context, svc *ingestion.Service, owner string, log *slog.Logger) Stats {
	var stats Stats
	sweeps := []struct {
		name   string
		target *int
		run    func(context.Context) (int, error)
	}{
		{"read staged manifests", &stats.Manifests, func(ctx context.Context) (int, error) { return svc.ResumeManifests(ctx, owner) }},
		{"expire upload leases", &stats.Expired, svc.ExpireLeases},
		{"run due retries", &stats.Retried, svc.RunDueRetries},
		{"check processing outcomes", &stats.Synced, svc.SyncOutcomes},
		{"advance cancellations", &stats.Cancelled, svc.AdvanceCancellations},
	}
	for _, s := range sweeps {
		if ctx.Err() != nil {
			break
		}
		n, err := s.run(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("ingestion sweep failed", "sweep", s.name, "error", retry.Sanitize(err))
		}
		*s.target = n
	}
	svc.Gauges(ctx)
	return stats
}

// Run repeats the sweeps every interval, and reconciles ingestion counters
// and tenant storage usage every reconcileEvery, until ctx ends.
func Run(ctx context.Context, svc *ingestion.Service, quotas *quota.Service, owner string, interval, reconcileEvery time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	nextReconcile := time.Now().Add(reconcileEvery)

	for {
		stats := RunOnce(ctx, svc, owner, log)
		if stats != (Stats{}) {
			log.Info("ingestion sweeps", "manifests", stats.Manifests, "expired_leases", stats.Expired,
				"due_retries", stats.Retried, "outcomes_checked", stats.Synced, "cancelled", stats.Cancelled)
		}
		if time.Now().After(nextReconcile) {
			report, err := svc.Reconcile(ctx)
			if err != nil && ctx.Err() == nil {
				log.Error("ingestion reconciliation failed", "error", retry.Sanitize(err))
			} else if report.JobsRepaired > 0 || report.TenantsRepaired > 0 {
				log.Warn("ingestion reconciliation repaired drift", "jobs_repaired", report.JobsRepaired,
					"tenants_repaired", report.TenantsRepaired)
			}
			if quotas != nil {
				// Releases reservations of uploads that ended or expired
				// without anyone telling the quota, and recounts usage
				// from sessions and assets.
				if _, err := quotas.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
					log.Error("quota reconciliation failed", "error", retry.Sanitize(err))
				}
			}
			nextReconcile = time.Now().Add(reconcileEvery)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
