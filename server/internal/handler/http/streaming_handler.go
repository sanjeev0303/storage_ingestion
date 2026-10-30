package http

import (
	"archive/tar"
	"errors"
	"io"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/streaming"
)

// StreamingHandler serves shard streams, signed snapshot manifests,
// packing settings, repacks, and runs' verification reports and
// checkpoints (docs/streaming.md).
type StreamingHandler struct {
	service *streaming.Service
	log     *slog.Logger
}

func NewStreamingHandler(service *streaming.Service, log *slog.Logger) *StreamingHandler {
	return &StreamingHandler{service: service, log: log}
}

func (h *StreamingHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/datasets/{id}/versions/{versionID}/shards":                       h.shards,
		"POST /api/v1/datasets/{id}/versions/{versionID}/stream":                      h.stream,
		"GET /api/v1/datasets/{id}/versions/{versionID}/signed-manifest":              h.signedManifest,
		"GET /api/v1/packing-settings":                                                h.packing,
		"PUT /api/v1/packing-settings":                                                h.setPacking,
		"POST /api/v1/datasets/{id}/repacks":                                          h.repack,
		"GET /api/v1/datasets/{id}/repacks":                                           h.repacks,
		"POST /api/v1/datasets/{id}/repacks/{repackID}/cancel":                        h.cancelRepack,
		"POST /api/v1/training-runs/{runID}/verifications":                            h.verify,
		"GET /api/v1/training-runs/{runID}/verifications":                             h.verifications,
		"POST /api/v1/training-runs/{runID}/checkpoints":                              h.checkpoint,
		"GET /api/v1/training-runs/{runID}/checkpoints":                               h.checkpoints,
		"POST /api/v1/training-runs/{runID}/checkpoints/{checkpointID}/verifications": h.verifyCheckpoint,
	}
}

func (h *StreamingHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case streaming.IsInvalid(err):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrDatasetNotFound), errors.Is(err, model.ErrDatasetVersionNotFound),
		errors.Is(err, model.ErrTrainingRunNotFound), errors.Is(err, model.ErrCheckpointNotFound), errors.Is(err, model.ErrRepackNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrRepackActive), errors.Is(err, model.ErrCheckpointExists):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "conflict"})
	case errors.Is(err, model.ErrSameHost):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "same_host"})
	case errors.Is(err, model.ErrVersionSuperseded):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "version_superseded"})
	case errors.Is(err, model.ErrUnauthenticated), errors.Is(err, model.ErrForbidden), errors.Is(err, model.ErrTenantRequired):
		writeSecurityError(w, h.log, err)
	default:
		writeDeliveryError(w, h.log, err, 0)
	}
}

func (h *StreamingHandler) shards(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	var target int64
	if v := q.Get("target_bytes"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			h.fail(w, streaming.ErrInvalid)
			return
		}
		target = n
	}
	page, err := h.service.Shards(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), q.Get("cursor"), target)
	if err != nil {
		h.fail(w, err)
		return
	}
	if page.Shards == nil {
		page.Shards = []model.Shard{}
	}
	writeJSON(w, nethttp.StatusOK, page)
}

// stream answers a tar of the members asked for, in that order. Each
// file's header carries its SHA-256 and ordinal as PAX records. Once the
// first byte is sent, a failure (a file that does not read back as
// published) cuts the connection, so the tar is never complete.
func (h *StreamingHandler) stream(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Ordinals []int64 `json:"ordinals"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	tw := tar.NewWriter(w)
	started := false
	err := h.service.Stream(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), body.Ordinals,
		middleware.RequestIDFrom(r.Context()), func(f streaming.StreamFile) error {
			if !started {
				started = true
				w.Header().Set("Content-Type", "application/x-tar")
				w.Header().Set("Cache-Control", "private, no-store")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(nethttp.StatusOK)
			}
			err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: 0o644, Size: f.Size, ModTime: time.Unix(0, 0), Format: tar.FormatPAX,
				PAXRecords: map[string]string{"OSP.sha256": f.SHA256, "OSP.ordinal": strconv.FormatInt(f.Ordinal, 10), "OSP.asset_id": f.AssetID}})
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f.Body)
			return err
		})
	if err != nil {
		if !started {
			h.fail(w, err)
			return
		}
		h.log.Warn("shard stream cut", "dataset_id", r.PathValue("id"), "error", err)
		panic(nethttp.ErrAbortHandler)
	}
	if !started {
		w.Header().Set("Content-Type", "application/x-tar")
	}
	_ = tw.Close()
}

func (h *StreamingHandler) signedManifest(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.SignedManifest(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *StreamingHandler) packing(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.PackingSettings(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *StreamingHandler) setPacking(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in streaming.PackingSettings
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.SetPackingSettings(r.Context(), principalOf(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *StreamingHandler) repack(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.RequestRepack(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, out)
}

func (h *StreamingHandler) repacks(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Repacks(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"repacks": out})
}

func (h *StreamingHandler) cancelRepack(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.CancelRepack(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("repackID")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *StreamingHandler) verify(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in streaming.VerificationInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.RecordVerification(r.Context(), principalOf(r), r.PathValue("runID"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *StreamingHandler) verifications(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Verifications(r.Context(), principalOf(r), r.PathValue("runID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"verifications": out})
}

func (h *StreamingHandler) checkpoint(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in streaming.CheckpointInput
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.AddCheckpoint(r.Context(), principalOf(r), r.PathValue("runID"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *StreamingHandler) checkpoints(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, last, err := h.service.Checkpoints(r.Context(), principalOf(r), r.PathValue("runID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"checkpoints": list, "last_verified": last})
}

func (h *StreamingHandler) verifyCheckpoint(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in streaming.CheckpointCheck
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.VerifyCheckpoint(r.Context(), principalOf(r), r.PathValue("runID"), r.PathValue("checkpointID"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}
