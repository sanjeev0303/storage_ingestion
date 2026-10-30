package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/erasure"
)

// ErasureHandler serves erasure, withdrawal and destroy-by-provenance
// (docs/erasure.md).
type ErasureHandler struct {
	service *erasure.Service
	log     *slog.Logger
}

func NewErasureHandler(service *erasure.Service, log *slog.Logger) *ErasureHandler {
	return &ErasureHandler{service: service, log: log}
}

func (h *ErasureHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"POST /api/v1/erasures/search":          h.search,
		"POST /api/v1/erasures":                 h.create,
		"GET /api/v1/erasures":                  h.list,
		"GET /api/v1/erasures/{id}":             h.get,
		"POST /api/v1/erasures/{id}/approve":    h.approve,
		"POST /api/v1/erasures/{id}/execute":    h.execute,
		"POST /api/v1/erasures/{id}/cancel":     h.cancel,
		"GET /api/v1/erasures/{id}/certificate": h.certificate,
	}
}

func (h *ErasureHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidErasure):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrErasureNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrNothingToErase):
		writeJSON(w, nethttp.StatusUnprocessableEntity, problem{Error: err.Error(), Code: "nothing_matched"})
	case errors.Is(err, model.ErrErasureApproval):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "approval_required"})
	case errors.Is(err, model.ErrSelfApproval):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "self_approval"})
	case errors.Is(err, model.ErrErasureState):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "invalid_state"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

type erasureBody struct {
	Kind        string   `json:"kind"`
	Reason      string   `json:"reason"`
	Reference   string   `json:"reference"`
	Institution string   `json:"institution"`
	SubjectIDs  []string `json:"subject_ids"`
	SHA256s     []string `json:"sha256s"`
	ObjectIDs   []string `json:"object_ids"`
	SubjectKey  string   `json:"subject_key"`
}

func (b erasureBody) selector() model.ErasureSelector {
	return model.ErasureSelector{SubjectIDs: b.SubjectIDs, SHA256s: b.SHA256s, ObjectIDs: b.ObjectIDs, SubjectKey: b.SubjectKey}
}

func (h *ErasureHandler) search(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body erasureBody
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.Search(r.Context(), principalOf(r), body.selector())
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *ErasureHandler) create(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body erasureBody
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.Create(r.Context(), principalOf(r), erasure.CreateInput{Kind: body.Kind, Reason: body.Reason,
		Reference: body.Reference, Institution: body.Institution, Selector: body.selector()})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *ErasureHandler) list(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.List(r.Context(), principalOf(r), r.URL.Query().Get("state"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"erasures": out})
}

func (h *ErasureHandler) get(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Get(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func noteOf(w nethttp.ResponseWriter, r *nethttp.Request) (string, bool) {
	var body struct {
		Note string `json:"note"`
	}
	if r.ContentLength == 0 {
		return "", true
	}
	if !decodeJSON(w, r, &body) {
		return "", false
	}
	if len(body.Note) > 2000 {
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: "a note is at most 2000 characters", Code: "invalid_request"})
		return "", false
	}
	return body.Note, true
}

func (h *ErasureHandler) approve(w nethttp.ResponseWriter, r *nethttp.Request) {
	note, ok := noteOf(w, r)
	if !ok {
		return
	}
	out, err := h.service.Approve(r.Context(), principalOf(r), r.PathValue("id"), note)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *ErasureHandler) cancel(w nethttp.ResponseWriter, r *nethttp.Request) {
	note, ok := noteOf(w, r)
	if !ok {
		return
	}
	out, err := h.service.Cancel(r.Context(), principalOf(r), r.PathValue("id"), note)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *ErasureHandler) execute(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Execute(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, out)
}

func (h *ErasureHandler) certificate(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Certificate(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}
