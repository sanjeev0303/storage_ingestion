package http

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	nethttp "net/http"
	"os"
	"strconv"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// copyBufferSize bounds memory per proxied download.
const copyBufferSize = 32 * 1024

// errStalled marks a stream cancelled because no bytes moved for the
// stall timeout.
var errStalled = errors.New("download stalled")

// StreamConfig bounds proxied downloads.
type StreamConfig struct {
	// StallTimeout ends a download when no bytes move for this long, in
	// either direction: a stuck provider or a client that stopped reading.
	StallTimeout time.Duration
	// MaxStreams caps concurrent proxied downloads; more get 503.
	MaxStreams int
	// Metrics is optional.
	Metrics *metrics.Registry
}

// DownloadHandler issues download URLs and proxies downloads.
type DownloadHandler struct {
	service *delivery.Service
	log     *slog.Logger
	cfg     StreamConfig
	slots   chan struct{}
}

func NewDownloadHandler(service *delivery.Service, log *slog.Logger, cfg StreamConfig) *DownloadHandler {
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = 30 * time.Second
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 64
	}
	return &DownloadHandler{service: service, log: log, cfg: cfg, slots: make(chan struct{}, cfg.MaxStreams)}
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
		TTL:       time.Duration(body.ExpiresInSeconds) * time.Second,
		Filename:  body.Filename,
		Mode:      body.Mode,
		RequestID: middleware.RequestIDFrom(r.Context()),
	})
	if err != nil {
		writeDeliveryError(w, h.log, err, 0)
		return
	}

	// The URL is a credential: never cache this response, never log the URL.
	h.log.Info("download URL issued", "asset_id", r.PathValue("id"), "tenant_id", principal.TenantID,
		"delivery", result.Via, "expires_at", result.ExpiresAt, "request_id", middleware.RequestIDFrom(r.Context()),
		"duration_ms", time.Since(started).Milliseconds())
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusOK, response.DownloadURLResponse{
		URL: result.URL, ExpiresAt: result.ExpiresAt, Delivery: result.Via, Revocable: result.Revocable,
	})
}

// RevokeLinks handles DELETE /api/v1/assets/{id}/download-links: every
// token link for the asset stops working at once.
func (h *DownloadHandler) RevokeLinks(w nethttp.ResponseWriter, r *nethttp.Request) {
	principal, _ := middleware.PrincipalFrom(r.Context())
	revoked, err := h.service.RevokeLinks(r.Context(), principal, r.PathValue("id"), middleware.RequestIDFrom(r.Context()))
	if err != nil {
		writeDeliveryError(w, h.log, err, 0)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusOK, response.RevokeLinksResponse{Revoked: revoked})
}

// Content handles GET and HEAD /api/v1/assets/{id}/content: an authorized
// download streamed through the API with bounded memory.
func (h *DownloadHandler) Content(w nethttp.ResponseWriter, r *nethttp.Request) {
	principal, _ := middleware.PrincipalFrom(r.Context())
	req := downloadRequest(r)
	h.serve(w, r, func(ctx context.Context) (delivery.Download, error) {
		return h.service.Open(ctx, principal, r.PathValue("id"), req)
	})
}

// TokenContent handles GET and HEAD /api/v1/downloads/{token}. The token
// is the credential; it is never logged.
func (h *DownloadHandler) TokenContent(w nethttp.ResponseWriter, r *nethttp.Request) {
	req := downloadRequest(r)
	h.serve(w, r, func(ctx context.Context) (delivery.Download, error) {
		return h.service.OpenToken(ctx, r.PathValue("token"), req)
	})
}

func downloadRequest(r *nethttp.Request) delivery.DownloadRequest {
	return delivery.DownloadRequest{
		Range:             r.Header.Get("Range"),
		IfRange:           r.Header.Get("If-Range"),
		IfMatch:           r.Header.Get("If-Match"),
		IfNoneMatch:       r.Header.Get("If-None-Match"),
		IfModifiedSince:   r.Header.Get("If-Modified-Since"),
		IfUnmodifiedSince: r.Header.Get("If-Unmodified-Since"),
		Filename:          r.URL.Query().Get("filename"),
		HeadOnly:          r.Method == nethttp.MethodHead,
		RequestID:         middleware.RequestIDFrom(r.Context()),
	}
}

// serve authorizes and opens the download, then streams it. Every check
// runs before the status line is written; once it is, a failure can only
// end the connection, and is audited instead of answered.
func (h *DownloadHandler) serve(w nethttp.ResponseWriter, r *nethttp.Request, open func(context.Context) (delivery.Download, error)) {
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.cfg.Metrics.Add("streams_rejected_busy", 1)
		w.Header().Set("Retry-After", "1")
		writeJSON(w, nethttp.StatusServiceUnavailable, map[string]string{"error": "too many downloads in progress"})
		return
	}
	h.cfg.Metrics.Add("streams_in_flight", 1)
	defer h.cfg.Metrics.Add("streams_in_flight", -1)

	// The watchdog cancels the storage read when nothing moves for the
	// stall timeout; each chunk pushes it back.
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	watchdog := time.AfterFunc(h.cfg.StallTimeout, func() { cancel(errStalled) })
	defer watchdog.Stop()

	started := time.Now()
	download, err := open(ctx)
	if err != nil {
		if errors.Is(context.Cause(ctx), errStalled) {
			err = delivery.ErrStorageUnavailable
		}
		writeDeliveryError(w, h.log, err, download.TotalSize)
		return
	}

	header := w.Header()
	header.Set("ETag", download.ETag)
	// RFC 9530: the digest of the whole object, also on a range, so a client
	// can check what it reassembles.
	if sum, err := hex.DecodeString(download.ChecksumSHA256); err == nil && len(sum) == sha256.Size {
		header.Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum)+":")
	}
	header.Set("Last-Modified", download.LastModified.Format(nethttp.TimeFormat))
	// Private: no shared cache may keep one tenant's bytes for another, and
	// every use must pass authorization again.
	header.Set("Cache-Control", "private, no-store")
	if download.NotModified {
		w.WriteHeader(nethttp.StatusNotModified)
		return
	}
	defer func() {
		if download.Body != nil {
			download.Body.Close()
		}
	}()

	header.Set("Content-Type", download.ContentType)
	header.Set("Content-Disposition", download.ContentDisposition)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Accept-Ranges", "bytes")
	header.Set("Content-Length", strconv.FormatInt(download.Length(), 10))
	status := nethttp.StatusOK
	if download.Partial {
		status = nethttp.StatusPartialContent
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", download.First, download.Last, download.TotalSize))
	}
	w.WriteHeader(status)
	if download.Body == nil {
		return // HEAD
	}

	written, reason := h.copy(ctx, w, download.Body, download.Length(), watchdog)
	requestID := middleware.RequestIDFrom(r.Context())
	if reason != "" {
		h.service.TransferFailed(r.Context(), download, written, reason)
		h.log.Info("proxied download interrupted", "asset_id", r.PathValue("id"), "bytes", written,
			"expected", download.Length(), "reason", reason, "request_id", requestID)
		return
	}
	h.service.TransferDone(download, written)
	h.log.Info("proxied download served", "asset_id", r.PathValue("id"), "partial", download.Partial,
		"bytes", written, "duration_ms", time.Since(started).Milliseconds(), "request_id", requestID)
}

// copy streams body to the client through one bounded buffer. It returns
// the bytes written and, if the transfer did not complete, why.
func (h *DownloadHandler) copy(ctx context.Context, w nethttp.ResponseWriter, body io.Reader, expected int64, watchdog *time.Timer) (int64, string) {
	controller := nethttp.NewResponseController(w)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()

	buf := make([]byte, copyBufferSize)
	var written int64
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			// A client that stops reading is cut off after the stall timeout.
			_ = controller.SetWriteDeadline(time.Now().Add(h.cfg.StallTimeout))
			if _, err := w.Write(buf[:n]); err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					return written, "client_stalled"
				}
				return written, "client_disconnected"
			}
			written += int64(n)
			watchdog.Reset(h.cfg.StallTimeout)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			switch {
			case errors.Is(context.Cause(ctx), errStalled):
				return written, "stalled"
			case ctx.Err() != nil:
				return written, "client_disconnected"
			default:
				return written, "upstream_failed"
			}
		}
	}
	if written != expected {
		// The provider ended early; the client sees a short body against
		// its Content-Length and must not treat it as complete.
		return written, "short_read"
	}
	return written, ""
}
