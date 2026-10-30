package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/tiering"
)

// TieringHandler serves tier rules, archive restores, cost estimates,
// reports and budgets, snapshot expiry and storage manifests
// (docs/tiering.md).
type TieringHandler struct {
	service *tiering.Service
	log     *slog.Logger
}

func NewTieringHandler(service *tiering.Service, log *slog.Logger) *TieringHandler {
	return &TieringHandler{service: service, log: log}
}

func (h *TieringHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/tier-rules":                                           h.rules,
		"POST /api/v1/tier-rules":                                          h.createRule,
		"PUT /api/v1/tier-rules/{ruleID}":                                  h.updateRule,
		"DELETE /api/v1/tier-rules/{ruleID}":                               h.deleteRule,
		"GET /api/v1/tier-moves":                                           h.moves,
		"POST /api/v1/assets/{id}/archive-restores":                        h.restore,
		"GET /api/v1/archive-restores":                                     h.restores,
		"POST /api/v1/datasets/{id}/versions/{versionID}/archive-restores": h.restoreVersion,
		"GET /api/v1/datasets/{id}/versions/{versionID}/cost-estimate":     h.versionEstimate,
		"GET /api/v1/datasets/{id}/versions/{versionID}/storage-manifest":  h.storageManifest,
		"GET /api/v1/datasets/{id}/costs":                                  h.datasetCosts,
		"POST /api/v1/datasets/{id}/version-expiry":                        h.expire,
		"POST /api/v1/cost-estimates":                                      h.estimate,
		"GET /api/v1/cost-prices":                                          h.prices,
		"GET /api/v1/cost-report":                                          h.report,
		"GET /api/v1/cost-budget":                                          h.budget,
		"PUT /api/v1/cost-budget":                                          h.setBudget,
	}
}

func (h *TieringHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidTiering):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrTierRuleNotFound), errors.Is(err, model.ErrAssetNotFound), errors.Is(err, model.ErrDatasetNotFound),
		errors.Is(err, model.ErrDatasetVersionNotFound), errors.Is(err, model.ErrConnectionNotFound), errors.Is(err, model.ErrRestoreNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

func (h *TieringHandler) rules(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Rules(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"rules": out})
}

func (h *TieringHandler) createRule(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in tiering.RuleInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.PutRule(r.Context(), principalOf(r), "", in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *TieringHandler) updateRule(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in tiering.RuleInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.PutRule(r.Context(), principalOf(r), r.PathValue("ruleID"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *TieringHandler) deleteRule(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.DeleteRule(r.Context(), principalOf(r), r.PathValue("ruleID")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *TieringHandler) moves(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Moves(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"moves": out})
}

func (h *TieringHandler) restore(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in tiering.RestoreInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.RequestRestore(r.Context(), principalOf(r), r.PathValue("id"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, out)
}

func (h *TieringHandler) restores(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	out, err := h.service.Restores(r.Context(), principalOf(r), q.Get("asset_id"), q.Get("version_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"restores": out})
}

func (h *TieringHandler) restoreVersion(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in tiering.RestoreInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.RestoreVersion(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, out)
}

func (h *TieringHandler) versionEstimate(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.VersionEstimate(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"),
		model.RestoreSpeed(r.URL.Query().Get("speed")))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *TieringHandler) storageManifest(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	out, err := h.service.StorageManifest(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), q.Get("after"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *TieringHandler) datasetCosts(w nethttp.ResponseWriter, r *nethttp.Request) {
	years, _ := strconv.ParseFloat(r.URL.Query().Get("years"), 64)
	out, err := h.service.DatasetCosts(r.Context(), principalOf(r), r.PathValue("id"), years)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *TieringHandler) expire(w nethttp.ResponseWriter, r *nethttp.Request) {
	in := tiering.ExpiryInput{DryRun: true}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.ExpireVersions(r.Context(), principalOf(r), r.PathValue("id"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *TieringHandler) estimate(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in tiering.EstimateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.Estimate(r.Context(), principalOf(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *TieringHandler) prices(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, nethttp.StatusOK, map[string]any{"prices": tiering.Prices()})
}

func (h *TieringHandler) report(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Report(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *TieringHandler) budget(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Budget(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *TieringHandler) setBudget(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in model.CostBudget
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.SetBudget(r.Context(), principalOf(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}
