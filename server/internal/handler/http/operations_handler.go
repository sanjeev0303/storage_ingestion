package http

import (
	"context"
	"log/slog"
	nethttp "net/http"
	"slices"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// OperationsHandler serves GET /api/v1/operations/status: the caller's
// organization's background work, so a console can show whether uploads
// are waiting on processing, imports or validations instead of guessing.
type OperationsHandler struct {
	status func(ctx context.Context, tenantID string, now time.Time) (model.OperationsStatus, error)
	log    *slog.Logger
}

func NewOperationsHandler(status func(ctx context.Context, tenantID string, now time.Time) (model.OperationsStatus, error), log *slog.Logger) *OperationsHandler {
	return &OperationsHandler{status: status, log: log}
}

func (h *OperationsHandler) Status(w nethttp.ResponseWriter, r *nethttp.Request) {
	p := principalOf(r)
	if err := p.Require(model.PermIngestionRead); err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	s, err := h.status(r.Context(), p.TenantID, time.Now().UTC())
	if err != nil {
		h.log.Error("operations status failed", "tenant_id", p.TenantID, "error", err)
		writeJSON(w, nethttp.StatusInternalServerError, problem{Error: "operations status unavailable", Code: "internal"})
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, map[string]any{
		"at": s.At,
		"processing": map[string]any{
			"waiting": s.ProcessingWaiting, "held": s.ProcessingHeld,
			"oldest_wait_seconds": int64(s.OldestProcessingWait.Seconds()),
		},
		"ingestion": map[string]any{
			"active_jobs": s.IngestionJobsActive, "entries_waiting": s.EntriesWaiting, "entries_in_flight": s.EntriesInFlight,
		},
		"imports":     map[string]any{"active": s.ImportsActive},
		"validations": map[string]any{"queued": s.ValidationsQueued, "running": s.ValidationsRunning},
		"deletions":   map[string]any{"pending": s.DeletionsScheduled},
		"storage":     map[string]any{"shared_assets": s.SharedAssets, "shared_bytes": s.SharedBytes},
		"workers":     workerHealth(s.Workers, s.At, p.PlatformRole == model.PlatformAdmin),
	})
}

// workerHealth says, per kind of worker, whether any process of it has
// written a heartbeat recently. Workers are shared by every organization,
// so a member sees only the state; host names and process IDs are for
// platform administrators.
func workerHealth(beats []model.WorkerHeartbeat, now time.Time, detailed bool) []map[string]any {
	byKind := map[string][]model.WorkerHeartbeat{}
	kinds := append([]string(nil), model.WorkerKinds...)
	for _, b := range beats {
		if !slices.Contains(kinds, b.Kind) {
			kinds = append(kinds, b.Kind)
		}
		byKind[b.Kind] = append(byKind[b.Kind], b)
	}

	out := make([]map[string]any, 0, len(kinds))
	for _, kind := range kinds {
		entry := map[string]any{"kind": kind, "state": "NOT_SEEN"}
		var latest time.Time
		live := 0
		instances := []map[string]any{}
		for _, b := range byKind[kind] {
			stale := now.Sub(b.LastSeenAt) > model.WorkerStaleAfter
			if !stale {
				live++
			}
			if b.LastSeenAt.After(latest) {
				latest = b.LastSeenAt
			}
			instances = append(instances, map[string]any{
				"instance": b.Instance, "started_at": b.StartedAt, "last_seen_at": b.LastSeenAt, "stale": stale,
			})
		}
		if !latest.IsZero() {
			entry["last_seen_at"] = latest
			entry["state"] = "STALE"
		}
		if live > 0 {
			entry["state"] = "RUNNING"
		}
		if detailed {
			entry["instances"] = instances
		}
		out = append(out, entry)
	}
	return out
}
