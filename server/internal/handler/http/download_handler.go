package http

import (
	"fmt"
	"io"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// copyBufferSize bounds memory per proxied download.
const copyBufferSize = 32 * 1024

// DownloadHandler issues download URLs and proxies downloads.
type DownloadHandler struct {
	service *delivery.Service
	log     *slog.Logger
}

func NewDownloadHandler(service *delivery.Service, log *slog.Logger) *DownloadHandler {
	return &DownloadHandler{service: service, log: log}
}

// CreateURL handles POST /api/v1/assets/{id}/download-url.
func (h *DownloadHandler) CreateURL(w nethttp.ResponseWriter, r *nethttp.Request) {
	started := time.Now()
	principal, _ := middleware.PrincipalFrom(r.Context())

	var body request.DownloadURLRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return
		}
	}
	if body.ExpiresInSeconds < 0 {
		writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "expires_in_seconds must not be negative"})
		return
	}

	result, err := h.service.CreateDownloadURL(r.Context(), principal, r.PathValue("id"), delivery.URLRequest{
		TTL:      time.Duration(body.ExpiresInSeconds) * time.Second,
		Filename: body.Filename,
	})
	if err != nil {
		writeDeliveryError(w, h.log, err, 0)
		return
	}

	// The URL is a credential: never cache this response, never log the URL.
	h.log.Info("download URL issued", "asset_id", r.PathValue("id"), "tenant_id", principal.TenantID,
		"delivery", result.Via, "expires_at", result.ExpiresAt, "duration_ms", time.Since(started).Milliseconds())
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusOK, response.DownloadURLResponse{
		URL: result.URL, ExpiresAt: result.ExpiresAt, Delivery: result.Via,
	})
}

// Content handles GET /api/v1/assets/{id}/content: an authorized download
// streamed through the API with bounded memory, honoring single byte
// ranges. Cancelling the request cancels the storage read.
func (h *DownloadHandler) Content(w nethttp.ResponseWriter, r *nethttp.Request) {
	started := time.Now()
	principal, _ := middleware.PrincipalFrom(r.Context())

	download, err := h.service.Open(r.Context(), principal, r.PathValue("id"), r.Header.Get("Range"), r.URL.Query().Get("filename"))
	if err != nil {
		writeDeliveryError(w, h.log, err, download.TotalSize)
		return
	}
	defer download.Body.Close()

	header := w.Header()
	header.Set("Content-Type", download.ContentType)
	header.Set("Content-Disposition", download.ContentDisposition)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Accept-Ranges", "bytes")
	// Private: no shared cache may keep one tenant's bytes for another.
	header.Set("Cache-Control", "private, no-store")
	header.Set("Content-Length", strconv.FormatInt(download.Last-download.First+1, 10))

	status := nethttp.StatusOK
	if download.Partial {
		status = nethttp.StatusPartialContent
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", download.First, download.Last, download.TotalSize))
	}
	w.WriteHeader(status)

	written, err := io.CopyBuffer(w, download.Body, make([]byte, copyBufferSize))
	if err != nil {
		// Usually the client disconnected; the headers are already sent.
		h.log.Info("proxied download interrupted", "asset_id", r.PathValue("id"), "bytes", written,
			"client_gone", r.Context().Err() != nil)
		return
	}
	h.log.Info("proxied download served", "asset_id", r.PathValue("id"), "tenant_id", principal.TenantID,
		"partial", download.Partial, "bytes", written, "duration_ms", time.Since(started).Milliseconds())
}
