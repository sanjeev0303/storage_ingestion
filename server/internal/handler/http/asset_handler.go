package http

import (
	"context"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// AssetHandler serves an owner's asset metadata.
type AssetHandler struct {
	service *delivery.Service
	log     *slog.Logger
}

func NewAssetHandler(service *delivery.Service, log *slog.Logger) *AssetHandler {
	return &AssetHandler{service: service, log: log}
}

// Get handles GET /api/v1/assets/{id}.
func (h *AssetHandler) Get(w nethttp.ResponseWriter, r *nethttp.Request) {
	principal, _ := middleware.PrincipalFrom(r.Context())

	info, err := h.service.Metadata(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		writeDeliveryError(w, h.log, err, 0)
		return
	}

	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, response.AssetResponse{
		ID: info.ID, FileName: info.FileName, ContentType: info.ContentType, SizeBytes: info.SizeBytes,
		ChecksumSHA256: info.ChecksumSHA256, Status: string(info.Status), RejectionReason: info.RejectionReason,
		CreatedAt: info.CreatedAt, UpdatedAt: info.UpdatedAt,
	})
}

// writeDeliveryError maps delivery errors to fixed messages; provider and
// database errors never reach the client.
func writeDeliveryError(w nethttp.ResponseWriter, log *slog.Logger, err error, totalSize int64) {
	switch {
	case errors.Is(err, context.Canceled):
		// The client went away; there is no one to answer.
	case errors.Is(err, model.ErrUnauthenticated):
		writeJSON(w, nethttp.StatusUnauthorized, map[string]string{"error": "authentication required"})
	case errors.Is(err, delivery.ErrNotFound):
		writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "asset not found"})
	case errors.Is(err, delivery.ErrNotAvailable):
		writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "asset is not available for delivery"})
	case errors.Is(err, delivery.ErrRangeNotSatisfiable):
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(totalSize, 10))
		writeJSON(w, nethttp.StatusRequestedRangeNotSatisfiable, map[string]string{"error": "requested range not satisfiable"})
	case errors.Is(err, delivery.ErrStorageUnavailable), errors.Is(err, delivery.ErrAuditUnavailable):
		writeJSON(w, nethttp.StatusServiceUnavailable, map[string]string{"error": "delivery is temporarily unavailable"})
	default:
		log.Error("delivery request failed", "error", err)
		writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "delivery failed"})
	}
}
