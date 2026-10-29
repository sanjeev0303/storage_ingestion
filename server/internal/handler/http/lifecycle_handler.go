package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
)

// LifecycleHandler lets an owner delete, restore and inspect the lifecycle
// of their own assets.
type LifecycleHandler struct {
	service *assetservice.Service
	log     *slog.Logger
}

func NewLifecycleHandler(service *assetservice.Service, log *slog.Logger) *LifecycleHandler {
	return &LifecycleHandler{service: service, log: log}
}

// Delete handles DELETE /api/v1/assets/{id}: the asset is soft-deleted at
// once (no longer delivered) and permanently deleted after its grace
// period, unless restored first. Repeating the request is harmless.
func (h *LifecycleHandler) Delete(w nethttp.ResponseWriter, r *nethttp.Request) {
	principal, _ := middleware.PrincipalFrom(r.Context())
	result, err := h.service.RequestDeletion(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.log.Info("asset deletion requested", "asset_id", r.PathValue("id"), "tenant_id", principal.TenantID,
		"operation_id", result.Operation.ID, "created", result.Created, "request_id", middleware.RequestIDFrom(r.Context()))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusAccepted, response.DeleteAssetResponse{
		AssetID:  result.Operation.AssetID,
		Status:   string(model.AssetStatusSoftDeleted),
		Deletion: deletionResponse(result.Operation),
		Created:  result.Created,
	})
}

// Restore handles POST /api/v1/assets/{id}/restore.
func (h *LifecycleHandler) Restore(w nethttp.ResponseWriter, r *nethttp.Request) {
	principal, _ := middleware.PrincipalFrom(r.Context())
	op, err := h.service.Restore(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusOK, response.RestoreAssetResponse{AssetID: op.AssetID, Status: string(op.PreviousStatus)})
}

// Get handles GET /api/v1/assets/{id}/lifecycle.
func (h *LifecycleHandler) Get(w nethttp.ResponseWriter, r *nethttp.Request) {
	principal, _ := middleware.PrincipalFrom(r.Context())
	inspection, err := h.service.OwnerInspect(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}

	body := response.AssetLifecycleResponse{
		AssetID:              inspection.Asset.ID,
		Status:               string(inspection.Asset.Status),
		Dataset:              inspection.Asset.Dataset,
		RetainUntil:          inspection.RetainUntil,
		ExpiresAt:            inspection.ExpiresAt,
		DeletionGraceSeconds: int64(inspection.Policy.DeletionGrace.Seconds()),
		// Only the date: whether a legal hold is also set is not the
		// owner's to see.
		LockedUntil: inspection.LockedUntil,
	}
	for _, op := range inspection.Operations {
		if op.State.IsLive() || op.State == model.DeletionFailed {
			deletion := deletionResponse(op)
			body.Deletion = &deletion
			break
		}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, body)
}

// deletionResponse shows the owner where a deletion is. A protected one
// is shown as scheduled: why it is protected is not the owner's to see.
func deletionResponse(op model.DeletionOperation) response.DeletionResponse {
	state := op.State
	if state == model.DeletionProtected {
		state = model.DeletionScheduled
	}
	return response.DeletionResponse{
		ID: op.ID, State: string(state), PurgeAfter: op.PurgeAfter, Restorable: op.State.Restorable(),
	}
}

func (h *LifecycleHandler) writeError(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrUnauthenticated):
		writeJSON(w, nethttp.StatusUnauthorized, map[string]string{"error": "authentication required"})
	case errors.Is(err, model.ErrForbidden), errors.Is(err, model.ErrTenantRequired):
		writeSecurityError(w, h.log, err)
	case errors.Is(err, assetservice.ErrNotFound):
		writeJSON(w, nethttp.StatusNotFound, map[string]string{"error": "asset not found"})
	case errors.Is(err, model.ErrDeletionBlocked):
		writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "asset is being processed; delete it once processing has finished"})
	case errors.Is(err, model.ErrNotRestorable):
		writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "nothing to restore: no deletion, or its objects are already being removed"})
	case errors.Is(err, model.ErrInvalidAssetState):
		writeJSON(w, nethttp.StatusConflict, map[string]string{"error": "the asset changed during the request; try again"})
	default:
		h.log.Error("lifecycle request failed", "error", err)
		writeJSON(w, nethttp.StatusInternalServerError, map[string]string{"error": "lifecycle request failed"})
	}
}
