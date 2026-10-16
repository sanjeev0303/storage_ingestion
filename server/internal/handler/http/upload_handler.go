package http

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

const maxJSONRequestBytes = 1 << 20 // 1 MiB

type UploadHandler struct {
	service  *uploadservice.Service
	tenantID string
	ownerID  string
}

// NewUploadHandler receives the service and the trusted identity context.
// The identity arguments are temporary local-development placeholders.
// Production code must derive them from authenticated credentials.
func NewUploadHandler(
	service *uploadservice.Service,
	tenantID string,
	ownerID string,
) *UploadHandler {
	return &UploadHandler{
		service:  service,
		tenantID: tenantID,
		ownerID:  ownerID,
	}
}

func (h *UploadHandler) Create(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

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

	if !strings.HasPrefix(
		r.Header.Get("Content-Type"),
		"application/json",
	) {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	result, err := h.service.Create(r.Context(), uploadservice.CreateInput{
		TenantID: h.tenantID,
		OwnerID:  h.ownerID,
		Request:  req,
	})
	if err != nil {
		switch {
		case errors.Is(err, uploadservice.ErrInvalidRequest):
			writeError(w, http.StatusBadRequest, err.Error())

		case errors.Is(err, uploadservice.ErrUploadTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())

		case errors.Is(err, uploadservice.ErrUnsupportedMediaType):
			writeError(w, http.StatusUnsupportedMediaType, err.Error())

		case errors.Is(err, uploadservice.ErrIdempotencyConflict),
			errors.Is(err, model.ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, "idempotency key conflicts with another request")

		case errors.Is(err, model.ErrUploadAlreadyExists):
			writeError(w, http.StatusConflict, "upload already exists")

		default:
			// Internal database or infrastructure errors must not be
			// exposed to the client.
			log.Printf("create upload failed: %v", err)
			writeError(w, http.StatusInternalServerError, "could not create upload")
		}
		return
	}

	statusCode := http.StatusCreated
	if result.Reused {
		statusCode = http.StatusOK
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/api/v1/uploads/"+result.Upload.ID)
	w.WriteHeader(statusCode)

	responseBody := response.CreateUploadResponse{
		UploadID:  result.Upload.ID,
		Status:    string(result.Upload.Status),
		UploadURL: result.Plan.UploadURL,
		ExpiresAt: result.Upload.ExpiresAt,
	}

	if err := json.NewEncoder(w).Encode(responseBody); err != nil {
		// The response may already have been committed.
		// Structured error logging will be wired in later.
		return
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
