package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/edge"
)

// EdgeHandler serves edge devices (docs/edge.md).
type EdgeHandler struct {
	service *edge.Service
	log     *slog.Logger
}

func NewEdgeHandler(service *edge.Service, log *slog.Logger) *EdgeHandler {
	return &EdgeHandler{service: service, log: log}
}

func (h *EdgeHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"POST /api/v1/edge-devices":                h.register,
		"GET /api/v1/edge-devices":                 h.devices,
		"DELETE /api/v1/edge-devices/{id}":         h.revoke,
		"GET /api/v1/edge-recordings":              h.recordings,
		"GET /api/v1/edge-recordings/{id}":         h.recording,
		"GET /api/v1/edge/me":                      h.me,
		"POST /api/v1/edge/heartbeat":              h.heartbeat,
		"GET /api/v1/edge/confirm":                 h.confirm,
		"POST /api/v1/edge/recordings":             h.open,
		"POST /api/v1/edge/recordings/{id}/chunks": h.addChunk,
		"POST /api/v1/edge/recordings/{id}/close":  h.close,
	}
}

func (h *EdgeHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidEdge), errors.Is(err, model.ErrInvalidAPIToken):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrDeviceNotFound), errors.Is(err, model.ErrDatasetNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrNotADevice):
		writeJSON(w, nethttp.StatusForbidden, problem{Error: err.Error(), Code: "not_a_device"})
	case errors.Is(err, model.ErrOutsideDevicePath):
		writeJSON(w, nethttp.StatusForbidden, problem{Error: err.Error(), Code: "outside_device_path"})
	case errors.Is(err, model.ErrChunkDiffers):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "chunk_differs"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

func (h *EdgeHandler) register(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Name       string `json:"name"`
		DatasetID  string `json:"dataset_id"`
		PathPrefix string `json:"path_prefix"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	d, token, err := h.service.Register(r.Context(), principalOf(r), body.Name, body.DatasetID, body.PathPrefix)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusCreated, map[string]any{"device": d, "token": token})
}

func (h *EdgeHandler) devices(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Devices(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"devices": out})
}

func (h *EdgeHandler) revoke(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.Revoke(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *EdgeHandler) recordings(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Recordings(r.Context(), principalOf(r), r.URL.Query().Get("device_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"recordings": out})
}

func (h *EdgeHandler) recording(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Recording(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EdgeHandler) me(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Me(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EdgeHandler) heartbeat(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body model.EdgeHeartbeat
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := h.service.Heartbeat(r.Context(), principalOf(r), body); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *EdgeHandler) confirm(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	out, err := h.service.Confirm(r.Context(), principalOf(r), q.Get("path"), q.Get("sha256"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EdgeHandler) open(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		GapSeconds int    `json:"gap_seconds"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.OpenRecording(r.Context(), principalOf(r), body.Name, body.Kind, body.GapSeconds)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EdgeHandler) addChunk(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body model.EdgeChunk
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.AddChunk(r.Context(), principalOf(r), r.PathValue("id"), body)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EdgeHandler) close(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Expected int64    `json:"expected"`
		Missing  []string `json:"missing"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.Close(r.Context(), principalOf(r), r.PathValue("id"), body.Expected, body.Missing)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}
