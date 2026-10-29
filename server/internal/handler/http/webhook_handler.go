package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/webhook"
)

// WebhookHandler serves an organization's webhook endpoints and their
// deliveries. The signing secret is answered on create and rotate only.
type WebhookHandler struct {
	service *webhook.Service
	log     *slog.Logger
}

func NewWebhookHandler(service *webhook.Service, log *slog.Logger) *WebhookHandler {
	return &WebhookHandler{service: service, log: log}
}

func (h *WebhookHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/webhooks":                                    h.list,
		"POST /api/v1/webhooks":                                   h.create,
		"GET /api/v1/webhooks/{id}":                               h.get,
		"PATCH /api/v1/webhooks/{id}":                             h.update,
		"DELETE /api/v1/webhooks/{id}":                            h.delete,
		"POST /api/v1/webhooks/{id}/rotate-secret":                h.rotate,
		"POST /api/v1/webhooks/{id}/test":                         h.test,
		"GET /api/v1/webhooks/{id}/deliveries":                    h.deliveries,
		"POST /api/v1/webhooks/{id}/deliveries/{delivery}/retry": h.redeliver,
	}
}

type webhookResponse struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	EventTypes  []string  `json:"event_types"`
	Enabled     bool      `json:"enabled"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Secret is set only when it was just made.
	Secret string `json:"secret,omitempty"`
}

func toWebhook(e model.WebhookEndpoint) webhookResponse {
	return webhookResponse{ID: e.ID, URL: e.URL, Description: e.Description, EventTypes: e.EventTypes, Enabled: e.Enabled,
		CreatedBy: e.CreatedBy, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
}

type deliveryResponse struct {
	ID             string          `json:"id"`
	EventID        string          `json:"event_id"`
	EventType      string          `json:"event_type"`
	Status         string          `json:"status"`
	Attempts       int             `json:"attempts"`
	NextAttemptAt  *time.Time      `json:"next_attempt_at,omitempty"`
	LastStatusCode int             `json:"last_status_code,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	LastAttemptAt  *time.Time      `json:"last_attempt_at,omitempty"`
	DeliveredAt    *time.Time      `json:"delivered_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	Payload        json.RawMessage `json:"payload"`
}

func toDelivery(d model.WebhookDelivery) deliveryResponse {
	out := deliveryResponse{ID: d.ID, EventID: d.EventID, EventType: d.EventType, Status: string(d.Status), Attempts: d.Attempts,
		LastStatusCode: d.LastStatusCode, LastError: d.LastError, LastAttemptAt: d.LastAttemptAt, DeliveredAt: d.DeliveredAt,
		CreatedAt: d.CreatedAt, Payload: d.Payload}
	if d.Status == model.HookPending {
		out.NextAttemptAt = &d.NextAttemptAt
	}
	return out
}

type webhookRequest struct {
	URL         *string  `json:"url"`
	Description *string  `json:"description"`
	EventTypes  []string `json:"event_types"`
	Enabled     *bool    `json:"enabled"`
}

func (r webhookRequest) input() webhook.Input {
	return webhook.Input{URL: r.URL, Description: r.Description, EventTypes: r.EventTypes, Enabled: r.Enabled}
}

func (h *WebhookHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, webhook.ErrInvalid):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, webhook.ErrNotConfigured):
		writeJSON(w, nethttp.StatusNotImplemented, problem{Error: err.Error(), Code: "not_configured"})
	case errors.Is(err, model.ErrWebhookNotFound), errors.Is(err, model.ErrDeliveryNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: err.Error(), Code: "not_found"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

func (h *WebhookHandler) list(w nethttp.ResponseWriter, r *nethttp.Request) {
	endpoints, err := h.service.List(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]webhookResponse, 0, len(endpoints))
	for _, e := range endpoints {
		out = append(out, toWebhook(e))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"webhooks": out, "event_types": model.WebhookEventTypes})
}

func (h *WebhookHandler) create(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req webhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	e, secret, err := h.service.Create(r.Context(), principalOf(r), req.input())
	if err != nil {
		h.fail(w, err)
		return
	}
	out := toWebhook(e)
	out.Secret = secret
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *WebhookHandler) get(w nethttp.ResponseWriter, r *nethttp.Request) {
	e, err := h.service.Get(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, toWebhook(e))
}

func (h *WebhookHandler) update(w nethttp.ResponseWriter, r *nethttp.Request) {
	var req webhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	e, err := h.service.Update(r.Context(), principalOf(r), r.PathValue("id"), req.input())
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, toWebhook(e))
}

func (h *WebhookHandler) delete(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.Delete(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *WebhookHandler) rotate(w nethttp.ResponseWriter, r *nethttp.Request) {
	secret, err := h.service.RotateSecret(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusOK, map[string]string{"secret": secret})
}

func (h *WebhookHandler) test(w nethttp.ResponseWriter, r *nethttp.Request) {
	d, err := h.service.Test(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, toDelivery(d))
}

func (h *WebhookHandler) deliveries(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, next, err := h.service.Deliveries(r.Context(), principalOf(r), r.PathValue("id"), q.Get("status"), q.Get("before"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]deliveryResponse, 0, len(page))
	for _, d := range page {
		out = append(out, toDelivery(d))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"deliveries": out, "next_before": next})
}

func (h *WebhookHandler) redeliver(w nethttp.ResponseWriter, r *nethttp.Request) {
	d, err := h.service.Redeliver(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("delivery"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, toDelivery(d))
}
