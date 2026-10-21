package http

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

const maxJSONRequestBytes = 1 << 20 // 1 MiB

// UploadHandler starts single-request uploads and reports on uploads.
// The tenant and owner come from the authenticated principal only.
type UploadHandler struct {
	service *uploadservice.Service
	log     *slog.Logger
}

func NewUploadHandler(service *uploadservice.Service, log *slog.Logger) *UploadHandler {
	return &UploadHandler{service: service, log: log}
}

func (h *UploadHandler) Create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONRequestBytes)
	defer r.Body.Close()

	var req request.CreateUploadRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	// Reject a second JSON value or unexpected trailing content.
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	result, err := h.service.CreateFor(r.Context(), principalOf(r), req)
	if err != nil {
		h.fail(w, err)
		return
	}

	statusCode := http.StatusCreated
	if result.Reused {
		statusCode = http.StatusOK
	}
	w.Header().Set("Location", "/api/v1/uploads/"+result.Upload.ID)
	writeJSON(w, statusCode, response.CreateUploadResponse{
		UploadID:  result.Upload.ID,
		Status:    string(result.Upload.Status),
		UploadURL: result.Plan.UploadURL,
		ExpiresAt: result.Upload.ExpiresAt,

		UploadHeaders:       result.Plan.Headers,
		StorageConnectionID: result.Upload.StorageConnectionID,
	})
}

// Get reports an upload of the caller's organization.
func (h *UploadHandler) Get(w http.ResponseWriter, r *http.Request) {
	session, err := h.service.GetFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, uploadStatus{
		UploadID: session.ID, Status: string(session.Status), FileName: session.FileName,
		SizeBytes: session.ExpectedSize, ExpiresAt: session.ExpiresAt, Multipart: session.StorageUploadID != "",
	})
}

type uploadStatus struct {
	UploadID  string    `json:"upload_id"`
	Status    string    `json:"status"`
	FileName  string    `json:"file_name"`
	SizeBytes int64     `json:"size_bytes"`
	ExpiresAt time.Time `json:"expires_at"`
	Multipart bool      `json:"multipart"`
}

func (h *UploadHandler) fail(w http.ResponseWriter, err error) {
	var quotaErr *model.QuotaError
	switch {
	case errors.As(err, &quotaErr):
		writeQuotaError(w, quotaErr)
	case errors.Is(err, model.ErrForbidden), errors.Is(err, model.ErrTenantRequired), errors.Is(err, model.ErrUploadNotFound),
		errors.Is(err, model.ErrPathExists), errors.Is(err, model.ErrPathBusy), storageConnectionError(err):
		writeSecurityError(w, h.log, err)
	case errors.Is(err, uploadservice.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, uploadservice.ErrUploadTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, uploadservice.ErrUnsupportedMediaType):
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
	case errors.Is(err, uploadservice.ErrIdempotencyConflict), errors.Is(err, model.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency key conflicts with another request")
	case errors.Is(err, model.ErrUploadAlreadyExists):
		writeError(w, http.StatusConflict, "upload already exists")
	case errors.Is(err, uploadservice.ErrObjectNotUploaded):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, uploadservice.ErrObjectSizeMismatch):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		// Internal database or infrastructure errors must not be exposed
		// to the client.
		h.log.Error("upload request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not process the upload request")
	}
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error: message,
	})
}
