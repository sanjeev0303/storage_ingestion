package http

import (
	"encoding/json"
	"errors"
	"io"
	nethttp "net/http"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// MultipartUploadHandler handles the multipart upload lifecycle endpoints.
type MultipartUploadHandler struct {
	service *uploadservice.MultipartService
}

// NewMultipartUploadHandler creates a handler backed by the given service.
func NewMultipartUploadHandler(
	service *uploadservice.MultipartService,
) *MultipartUploadHandler {
	return &MultipartUploadHandler{service: service}
}

// Register mounts all multipart routes on the provided mux.
func (h *MultipartUploadHandler) Register(mux *nethttp.ServeMux) {
	mux.HandleFunc("POST /api/v1/uploads/multipart", h.start)
	mux.HandleFunc("POST /api/v1/uploads/{id}/parts/urls", h.presignParts)
	mux.HandleFunc("POST /api/v1/uploads/{id}/parts/confirm", h.recordParts)
	mux.HandleFunc("POST /api/v1/uploads/{id}/complete", h.complete)
	mux.HandleFunc("POST /api/v1/uploads/{id}/abort", h.abort)
}

func (h *MultipartUploadHandler) start(
	w nethttp.ResponseWriter,
	r *nethttp.Request,
) {
	var body request.StartMultipartUploadRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	// Prototype identity only. Replace with authenticated identity.
	result, err := h.service.Start(r.Context(), uploadservice.StartMultipartInput{
		TenantID:       "local-tenant",
		OwnerID:        "local-user",
		FileName:       body.FileName,
		ContentType:    body.ContentType,
		SizeBytes:      body.SizeBytes,
		ChecksumSHA256: body.ChecksumSHA256,
		IdempotencyKey: body.IdempotencyKey,
	})
	if err != nil {
		writeMultipartError(w, err)
		return
	}

	writeJSON(w, nethttp.StatusCreated, response.StartMultipartUploadResponse{
		UploadID:  result.Session.ID,
		Status:    string(result.Session.Status),
		ExpiresAt: result.Session.ExpiresAt,
		Reused:    result.Reused,
	})
}

func (h *MultipartUploadHandler) presignParts(
	w nethttp.ResponseWriter,
	r *nethttp.Request,
) {
	var body request.PresignPartsRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	urls, err := h.service.PresignParts(
		r.Context(),
		"local-tenant",
		"local-user",
		r.PathValue("id"),
		body.PartNumbers,
	)
	if err != nil {
		writeMultipartError(w, err)
		return
	}

	result := response.PresignPartsResponse{
		UploadID: r.PathValue("id"),
		Parts:    make([]response.PresignedPartURL, 0, len(urls)),
	}
	for _, item := range urls {
		result.Parts = append(result.Parts, response.PresignedPartURL{
			PartNumber: item.PartNumber,
			URL:        item.URL,
			ExpiresAt:  item.ExpiresAt,
		})
	}

	writeJSON(w, nethttp.StatusOK, result)
}

func (h *MultipartUploadHandler) recordParts(
	w nethttp.ResponseWriter,
	r *nethttp.Request,
) {
	var body request.RecordPartsRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	parts := make([]uploadservice.RecordPartInput, 0, len(body.Parts))
	for _, item := range body.Parts {
		parts = append(parts, uploadservice.RecordPartInput{
			PartNumber:     item.PartNumber,
			ETag:           item.ETag,
			SizeBytes:      item.SizeBytes,
			ChecksumSHA256: item.ChecksumSHA256,
		})
	}

	err := h.service.RecordParts(
		r.Context(),
		"local-tenant",
		"local-user",
		r.PathValue("id"),
		parts,
	)
	if err != nil {
		writeMultipartError(w, err)
		return
	}

	writeJSON(w, nethttp.StatusOK, map[string]string{"status": "parts_recorded"})
}

func (h *MultipartUploadHandler) complete(
	w nethttp.ResponseWriter,
	r *nethttp.Request,
) {
	var body request.CompleteMultipartUploadRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	parts := make([]uploadservice.CompletedPartInput, 0, len(body.Parts))
	for _, item := range body.Parts {
		parts = append(parts, uploadservice.CompletedPartInput{
			PartNumber: item.PartNumber,
			ETag:       item.ETag,
		})
	}

	session, err := h.service.Complete(
		r.Context(),
		"local-tenant",
		"local-user",
		r.PathValue("id"),
		uploadservice.CompleteMultipartInput{Parts: parts},
	)
	if err != nil {
		writeMultipartError(w, err)
		return
	}

	writeJSON(w, nethttp.StatusOK, response.MultipartStatusResponse{
		UploadID:  session.ID,
		Status:    string(session.Status),
		ExpiresAt: session.ExpiresAt,
	})
}

func (h *MultipartUploadHandler) abort(
	w nethttp.ResponseWriter,
	r *nethttp.Request,
) {
	session, err := h.service.Abort(
		r.Context(),
		"local-tenant",
		"local-user",
		r.PathValue("id"),
	)
	if err != nil {
		writeMultipartError(w, err)
		return
	}

	writeJSON(w, nethttp.StatusOK, response.MultipartStatusResponse{
		UploadID:  session.ID,
		Status:    string(session.Status),
		ExpiresAt: session.ExpiresAt,
	})
}

// decodeJSON reads and decodes a JSON request body.
// It enforces a 1 MiB body limit and disallows unknown fields.
func decodeJSON(
	w nethttp.ResponseWriter,
	r *nethttp.Request,
	destination any,
) bool {
	r.Body = nethttp.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(destination); err != nil {
		nethttp.Error(w, "invalid JSON request", nethttp.StatusBadRequest)
		return false
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		nethttp.Error(w, "request must contain one JSON value", nethttp.StatusBadRequest)
		return false
	}

	return true
}

// writeJSON serialises value as JSON and writes it with the given status code.
func writeJSON(
	w nethttp.ResponseWriter,
	status int,
	value any,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeMultipartError maps service errors to appropriate HTTP status codes.
func writeMultipartError(w nethttp.ResponseWriter, err error) {
	status := nethttp.StatusInternalServerError
	message := "multipart upload operation failed"

	switch {
	// Pending outcomes wrap provider and database errors; only the fixed
	// sentinel text reaches the client.
	case errors.Is(err, uploadservice.ErrOperationPending):
		status = nethttp.StatusAccepted
		message = uploadservice.ErrOperationPending.Error()

	case errors.Is(err, uploadservice.ErrMultipartStartPending):
		status = nethttp.StatusConflict
		message = uploadservice.ErrMultipartStartPending.Error()

	case errors.Is(err, uploadservice.ErrInvalidRequest),
		errors.Is(err, uploadservice.ErrInvalidPart),
		errors.Is(err, uploadservice.ErrInvalidCompletion):
		status = nethttp.StatusBadRequest
		message = err.Error()

	case errors.Is(err, uploadservice.ErrUploadTooLarge):
		status = nethttp.StatusRequestEntityTooLarge
		message = err.Error()

	case errors.Is(err, uploadservice.ErrMultipartUploadNotFound):
		status = nethttp.StatusNotFound
		message = err.Error()

	case errors.Is(err, uploadservice.ErrMultipartNotActive):
		status = nethttp.StatusConflict
		message = uploadservice.ErrMultipartNotActive.Error()

	case errors.Is(err, uploadservice.ErrMultipartNotStarted):
		status = nethttp.StatusConflict
		message = err.Error()

	case errors.Is(err, uploadservice.ErrPartSizeMismatch),
		errors.Is(err, uploadservice.ErrObjectSizeMismatch):
		status = nethttp.StatusUnprocessableEntity
		message = err.Error()

	case errors.Is(err, model.ErrIdempotencyConflict):
		status = nethttp.StatusConflict
		message = "idempotency key was already used for a different request"
	}

	writeJSON(w, status, map[string]string{"error": strings.TrimSpace(message)})
}
