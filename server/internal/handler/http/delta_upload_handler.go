package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/deltaupload"
)

// DeltaUploadHandler serves delta uploads (docs/chunks.md).
type DeltaUploadHandler struct {
	service *deltaupload.Service
	log     *slog.Logger
}

func NewDeltaUploadHandler(service *deltaupload.Service, log *slog.Logger) *DeltaUploadHandler {
	return &DeltaUploadHandler{service: service, log: log}
}

func (h *DeltaUploadHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"POST /api/v1/delta-uploads":                      h.create,
		"GET /api/v1/delta-uploads/{id}":                  h.get,
		"PUT /api/v1/delta-uploads/{id}/chunks/{ordinal}": h.putChunk,
		"POST /api/v1/delta-uploads/{id}/complete":        h.complete,
	}
}

func (h *DeltaUploadHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidDelta), errors.Is(err, dataset.ErrInvalidRequest):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrDeltaNotFound), errors.Is(err, model.ErrDatasetNotFound), errors.Is(err, model.ErrVersionNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrDeltaState):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "delta_state"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

// A manifest of 1M chunks is about 100 MB of JSON; a file that size is a
// terabyte.
const maxDeltaManifestBytes = 128 << 20

func (h *DeltaUploadHandler) create(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in deltaupload.CreateInput
	body := nethttp.MaxBytesReader(w, r.Body, maxDeltaManifestBytes)
	defer body.Close()
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		nethttp.Error(w, "invalid JSON request", nethttp.StatusBadRequest)
		return
	}
	out, err := h.service.Create(r.Context(), principalOf(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *DeltaUploadHandler) putChunk(w nethttp.ResponseWriter, r *nethttp.Request) {
	ordinal, err := strconv.Atoi(r.PathValue("ordinal"))
	if err != nil || ordinal < 0 {
		h.fail(w, model.ErrDeltaNotFound)
		return
	}
	body := nethttp.MaxBytesReader(w, r.Body, 16<<20+1)
	defer body.Close()
	if err := h.service.PutChunk(r.Context(), principalOf(r), r.PathValue("id"), ordinal, body); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *DeltaUploadHandler) complete(w nethttp.ResponseWriter, r *nethttp.Request) {
	d, err := h.service.Complete(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, d)
}

func (h *DeltaUploadHandler) get(w nethttp.ResponseWriter, r *nethttp.Request) {
	d, asset, err := h.service.Get(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := map[string]any{"upload": d}
	if asset != nil {
		out["asset_id"], out["asset_status"] = asset.ID, asset.Status
	}
	writeJSON(w, nethttp.StatusOK, out)
}
