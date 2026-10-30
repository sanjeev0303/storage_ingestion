package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/copyengine"
)

// CopyHandler serves the copy engine: replicas, migrations, restores,
// drills, export bundles, scrubs and clean points (docs/copy-engine.md).
type CopyHandler struct {
	service *copyengine.Service
	log     *slog.Logger
}

func NewCopyHandler(service *copyengine.Service, log *slog.Logger) *CopyHandler {
	return &CopyHandler{service: service, log: log}
}

func (h *CopyHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/datasets/{id}/replication":      h.replication,
		"GET /api/v1/datasets/{id}/scrubs":           h.scrubs,
		"POST /api/v1/datasets/{id}/migrations":      h.migrate,
		"POST /api/v1/datasets/{id}/restores":        h.restore,
		"POST /api/v1/datasets/{id}/drills":          h.drill,
		"PUT /api/v1/datasets/{id}/clean-point":      h.cleanPoint,
		"GET /api/v1/datasets/{id}/clean-point/diff": h.cleanDiff,
		"POST /api/v1/exports":                       h.export,
		"GET /api/v1/export-schedule":                h.schedule,
		"PUT /api/v1/export-schedule":                h.setSchedule,
		"GET /api/v1/copy-jobs":                      h.jobs,
		"GET /api/v1/copy-jobs/{id}":                 h.job,
		"GET /api/v1/copy-jobs/{id}/items":           h.items,
		"POST /api/v1/copy-jobs/{id}/cancel":         h.cancel,
	}
}

func (h *CopyHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidCopyJob):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrCopyJobNotFound), errors.Is(err, model.ErrDatasetNotFound), errors.Is(err, model.ErrDatasetVersionNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

func (h *CopyHandler) replication(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.ReplicationOf(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *CopyHandler) scrubs(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, err := h.service.ScrubFailuresFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"scrubs": append([]model.ScrubResult{}, list...)})
}

func (h *CopyHandler) migrate(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		TargetConnectionID string `json:"target_connection_id"`
		DryRun             bool   `json:"dry_run"`
		IdempotencyKey     string `json:"idempotency_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	plan, job, err := h.service.MigrateFor(r.Context(), principalOf(r), r.PathValue("id"), copyengine.MigrationInput{
		TargetConnectionID: body.TargetConnectionID, DryRun: body.DryRun, IdempotencyKey: body.IdempotencyKey})
	if err != nil {
		h.fail(w, err)
		return
	}
	if job == nil {
		writeJSON(w, nethttp.StatusOK, map[string]any{"plan": plan})
		return
	}
	writeJSON(w, nethttp.StatusAccepted, map[string]any{"plan": plan, "job": job})
}

func (h *CopyHandler) restore(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		VersionID          string `json:"version_id"`
		TargetConnectionID string `json:"target_connection_id"`
		Prefix             string `json:"prefix"`
		IdempotencyKey     string `json:"idempotency_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	job, err := h.service.RestoreFor(r.Context(), principalOf(r), r.PathValue("id"), copyengine.RestoreInput{
		VersionID: body.VersionID, TargetConnectionID: body.TargetConnectionID, Prefix: body.Prefix, IdempotencyKey: body.IdempotencyKey})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, job)
}

func (h *CopyHandler) drill(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Sample         int    `json:"sample"`
		FromReplica    bool   `json:"from_replica"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	job, err := h.service.DrillFor(r.Context(), principalOf(r), r.PathValue("id"), copyengine.DrillInput{
		Sample: body.Sample, FromReplica: body.FromReplica, IdempotencyKey: body.IdempotencyKey})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, job)
}

func (h *CopyHandler) cleanPoint(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		VersionID string `json:"version_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := h.service.SetCleanPointFor(r.Context(), principalOf(r), r.PathValue("id"), body.VersionID); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *CopyHandler) cleanDiff(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.CleanDiffFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *CopyHandler) export(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		TargetConnectionID string `json:"target_connection_id"`
		IdempotencyKey     string `json:"idempotency_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	job, err := h.service.ExportFor(r.Context(), principalOf(r), body.TargetConnectionID, body.IdempotencyKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, job)
}

func (h *CopyHandler) schedule(w nethttp.ResponseWriter, r *nethttp.Request) {
	s, ok, err := h.service.ScheduleFor(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok {
		writeJSON(w, nethttp.StatusOK, map[string]any{"interval_hours": 0})
		return
	}
	writeJSON(w, nethttp.StatusOK, s)
}

func (h *CopyHandler) setSchedule(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		ConnectionID  string `json:"connection_id"`
		IntervalHours int    `json:"interval_hours"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := h.service.SetScheduleFor(r.Context(), principalOf(r), body.ConnectionID, body.IntervalHours); err != nil {
		h.fail(w, err)
		return
	}
	h.schedule(w, r)
}

func (h *CopyHandler) jobs(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, err := h.service.JobsFor(r.Context(), principalOf(r), r.URL.Query().Get("dataset_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"jobs": append([]model.CopyJob{}, list...)})
}

func (h *CopyHandler) job(w nethttp.ResponseWriter, r *nethttp.Request) {
	j, err := h.service.JobFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, j)
}

func (h *CopyHandler) items(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	after, _ := strconv.Atoi(q.Get("after"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.service.ItemsFor(r.Context(), principalOf(r), r.PathValue("id"), q.Get("state"), after, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"items": append([]model.CopyItem{}, list...)})
}

func (h *CopyHandler) cancel(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.CancelFor(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}
