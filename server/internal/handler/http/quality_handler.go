package http

import (
	"errors"
	nethttp "net/http"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// QualityRoutes serve column profiles, drift, leakage checks and releases
// (docs/quality.md).
func (h *DatasetHandler) QualityRoutes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/datasets/{id}/quality-settings":              h.qualitySettings,
		"PUT /api/v1/datasets/{id}/quality-settings":              h.putQualitySettings,
		"GET /api/v1/datasets/{id}/versions/{versionID}/profile":  h.versionProfile,
		"POST /api/v1/datasets/{id}/versions/{versionID}/profile": h.profileVersion,
		"GET /api/v1/datasets/{id}/versions/{versionID}/drift":    h.versionDrift,
		"POST /api/v1/datasets/{id}/versions/{versionID}/leakage": h.checkLeakage,
		"GET /api/v1/datasets/{id}/versions/{versionID}/leakage":  h.versionLeakage,
		"PUT /api/v1/datasets/{id}/versions/{versionID}/release":  h.release,
		"GET /api/v1/datasets/{id}/releases":                      h.releases,
		"GET /api/v1/datasets/{id}/releases/{label}/diff":         h.releaseDiff,
		"GET /api/v1/training-runs/{runID}/leakage":               h.runLeakage,
		"POST /api/v1/training-runs/{runID}/leakage":              h.checkRunLeakage,
		"POST /api/v1/leakage-checks":                             h.recordNgram,
	}
}

func (h *DatasetHandler) failQuality(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrProfileNotReady):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "profile_not_ready"})
	case errors.Is(err, model.ErrReleaseTaken):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "release_taken"})
	case errors.Is(err, model.ErrNoRelease):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, dataset.ErrQualityOff):
		writeJSON(w, nethttp.StatusServiceUnavailable, problem{Error: err.Error(), Code: "not_configured"})
	default:
		h.fail(w, err)
	}
}

func (h *DatasetHandler) qualitySettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.QualitySettings(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) putQualitySettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body model.QualitySettings
	if !decodeJSON(w, r, &body) {
		return
	}
	body.DatasetID = r.PathValue("id")
	out, err := h.service.PutQualitySettings(r.Context(), principalOf(r), body)
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) versionProfile(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.VersionProfile(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) profileVersion(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.ProfileVersion(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID")); err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, map[string]string{"state": model.ProfilePending})
}

func (h *DatasetHandler) versionDrift(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Drift(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), r.URL.Query().Get("against"))
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) checkLeakage(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		TrainVersionID string `json:"train_version_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.CheckLeakage(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), body.TrainVersionID)
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) versionLeakage(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.LeakageChecks(r.Context(), principalOf(r), "", r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"checks": out})
}

func (h *DatasetHandler) runLeakage(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.LeakageChecks(r.Context(), principalOf(r), r.PathValue("runID"), "", "")
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"checks": out})
}

func (h *DatasetHandler) checkRunLeakage(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.RunLeakage(r.Context(), principalOf(r), r.PathValue("runID"))
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"reports": out})
}

func (h *DatasetHandler) recordNgram(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		RunID          string                 `json:"run_id"`
		TrainVersionID string                 `json:"train_version_id"`
		EvalVersionID  string                 `json:"eval_version_id"`
		N              int                    `json:"n"`
		Threshold      float64                `json:"threshold"`
		Overlap        int64                  `json:"overlap"`
		EvalMembers    int64                  `json:"eval_members"`
		Examples       []model.LeakageExample `json:"examples"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.RecordNgram(r.Context(), principalOf(r), dataset.NgramInput{RunID: body.RunID, TrainVersionID: body.TrainVersionID,
		EvalVersionID: body.EvalVersionID, N: body.N, Threshold: body.Threshold, Overlap: body.Overlap, EvalMembers: body.EvalMembers,
		Examples: body.Examples})
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *DatasetHandler) release(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Label string `json:"label"`
		Notes string `json:"notes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.Release(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), body.Label, body.Notes)
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) releases(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Releases(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.failQuality(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"releases": out})
}

func (h *DatasetHandler) releaseDiff(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.DiffRelease(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("label"), r.URL.Query().Get("against"))
	if err != nil {
		h.failQuality(w, err)
		return
	}
	d := out.Members
	paths := make([]map[string]string, 0, len(d.Entries))
	for _, e := range d.Entries {
		p := e.TargetPath
		if p == "" {
			p = e.BasePath
		}
		paths = append(paths, map[string]string{"kind": e.Kind, "path": p})
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{
		"release": out.Release, "previous": out.Previous, "drift": out.Drift, "drift_note": out.DriftNote,
		"members": map[string]any{"added": d.Added, "removed": d.Removed, "changed": d.Changed, "relabeled": d.Relabeled,
			"unchanged": d.Unchanged, "first": paths},
	})
}
