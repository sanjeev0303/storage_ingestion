package http

import (
	"context"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/ingestion"
	"github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

// IngestionHandler serves datasets and bulk ingestion. Every route runs
// behind authentication; every call is scoped to the caller's tenant.
type IngestionHandler struct {
	service *ingestion.Service
	log     *slog.Logger
}

func NewIngestionHandler(service *ingestion.Service, log *slog.Logger) *IngestionHandler {
	return &IngestionHandler{service: service, log: log}
}

// Routes are the dataset and ingestion routes, for the router to wrap in
// authentication and rate limits.
func (h *IngestionHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"POST /api/v1/datasets":                                             h.createDataset,
		"GET /api/v1/datasets/{id}":                                         h.getDataset,
		"POST /api/v1/ingestion/jobs":                                       h.createJob,
		"GET /api/v1/ingestion/jobs":                                        h.listJobs,
		"GET /api/v1/ingestion/jobs/{id}":                                   h.getJob,
		"PUT /api/v1/ingestion/jobs/{id}/manifest":                          h.submitManifest,
		"POST /api/v1/ingestion/jobs/{id}/manifest/staged-url":              h.stageManifest,
		"POST /api/v1/ingestion/jobs/{id}/manifest/staged":                  h.submitStagedManifest,
		"POST /api/v1/ingestion/jobs/{id}/finalize":                         h.finalize,
		"POST /api/v1/ingestion/jobs/{id}/claims":                           h.claim,
		"GET /api/v1/ingestion/jobs/{id}/entries":                           h.listEntries,
		"GET /api/v1/ingestion/jobs/{id}/failures":                          h.listFailures,
		"GET /api/v1/ingestion/jobs/{id}/entries/{position}":                h.getEntry,
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/upload-url":    h.refreshUpload,
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/parts/urls":    h.partURLs,
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/parts/confirm": h.confirmParts,
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/heartbeat":     h.heartbeat,
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/complete":      h.complete,
		"POST /api/v1/ingestion/jobs/{id}/retry":                            h.retry,
		"POST /api/v1/ingestion/jobs/{id}/cancel":                           h.cancel,
	}
}

func principalOf(r *nethttp.Request) model.Principal {
	p, _ := middleware.PrincipalFrom(r.Context())
	return p
}

func (h *IngestionHandler) createDataset(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CreateDatasetRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	d, created, err := h.service.CreateDataset(r.Context(), principalOf(r), ingestion.CreateDatasetInput{
		Name: body.Name, CompletionPolicy: model.CompletionPolicy(strings.ToUpper(body.CompletionPolicy)), MaxBytes: body.MaxBytes,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	status := nethttp.StatusOK
	if created {
		status = nethttp.StatusCreated
	}
	writeJSON(w, status, datasetResponse(d))
}

func (h *IngestionHandler) getDataset(w nethttp.ResponseWriter, r *nethttp.Request) {
	d, err := h.service.GetDataset(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, datasetResponse(d))
}

func (h *IngestionHandler) createJob(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CreateIngestionJobRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	job, created, err := h.service.CreateJob(r.Context(), principalOf(r), ingestion.CreateJobInput{
		DatasetID: body.DatasetID, IdempotencyKey: body.IdempotencyKey, MaxConcurrency: body.MaxConcurrency,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	status := nethttp.StatusOK
	if created {
		status = nethttp.StatusCreated
	}
	writeJSON(w, status, jobResponse(job))
}

func (h *IngestionHandler) listJobs(w nethttp.ResponseWriter, r *nethttp.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := h.service.ListJobs(r.Context(), principalOf(r), r.URL.Query().Get("dataset_id"), r.URL.Query().Get("after"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	page := response.JobPageResponse{Jobs: make([]response.IngestionJobResponse, 0, len(jobs))}
	for _, j := range jobs {
		page.Jobs = append(page.Jobs, jobResponse(j))
	}
	if len(jobs) > 0 && len(jobs) == ingestion.PageSize(limit) {
		last := jobs[len(jobs)-1]
		page.NextAfter = strconv.FormatInt(last.CreatedAt.UnixNano(), 10) + ":" + last.ID
	}
	writeJSON(w, nethttp.StatusOK, page)
}

func (h *IngestionHandler) getJob(w nethttp.ResponseWriter, r *nethttp.Request) {
	job, err := h.service.GetJob(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, jobResponse(job))
}

// submitManifest reads a JSON Lines manifest from the request body. An
// optional X-Manifest-SHA256 header declares its checksum.
func (h *IngestionHandler) submitManifest(w nethttp.ResponseWriter, r *nethttp.Request) {
	defer r.Body.Close()
	report, err := h.service.SubmitInlineManifest(r.Context(), principalOf(r), r.PathValue("id"), r.Body,
		r.Header.Get("X-Manifest-SHA256"))
	if err != nil && !errors.Is(err, ingestion.ErrManifestTooLarge) && !errors.Is(err, ingestion.ErrTooManyLines) {
		h.fail(w, err)
		return
	}
	if err != nil {
		writeJSON(w, nethttp.StatusRequestEntityTooLarge, map[string]any{"error": err.Error(), "report": reportResponse(report)})
		return
	}
	writeJSON(w, nethttp.StatusOK, reportResponse(report))
}

func (h *IngestionHandler) stageManifest(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.StageManifestRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	staged, err := h.service.StageManifest(r.Context(), principalOf(r), r.PathValue("id"), body.SizeBytes)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, response.StagedManifestResponse{UploadURL: staged.UploadURL, ExpiresAt: staged.ExpiresAt})
}

func (h *IngestionHandler) submitStagedManifest(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.SubmitStagedManifestRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	job, err := h.service.SubmitStagedManifest(r.Context(), principalOf(r), r.PathValue("id"), body.SHA256)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, jobResponse(job))
}

func (h *IngestionHandler) finalize(w nethttp.ResponseWriter, r *nethttp.Request) {
	job, err := h.service.Finalize(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, jobResponse(job))
}

func (h *IngestionHandler) claim(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.ClaimEntriesRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	result, err := h.service.Claim(r.Context(), principalOf(r), r.PathValue("id"), body.Max)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.ClaimResponse{
		Grants: make([]response.GrantResponse, 0, len(result.Grants)), LimitedBy: result.LimitedBy,
		RetryAfterSeconds: int(result.RetryAfter.Seconds()), Failed: result.Failed,
	}
	for _, g := range result.Grants {
		out.Grants = append(out.Grants, grantResponse(g))
	}
	if out.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(out.RetryAfterSeconds))
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *IngestionHandler) listEntries(w nethttp.ResponseWriter, r *nethttp.Request) {
	var states []model.EntryState
	for _, s := range strings.Split(r.URL.Query().Get("state"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			states = append(states, model.EntryState(strings.ToUpper(s)))
		}
	}
	h.page(w, r, states)
}

func (h *IngestionHandler) listFailures(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.page(w, r, ingestion.FailedStates)
}

func (h *IngestionHandler) page(w nethttp.ResponseWriter, r *nethttp.Request, states []model.EntryState) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := h.service.ListEntries(r.Context(), principalOf(r), r.PathValue("id"), states, after, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	page := response.EntryPageResponse{Entries: make([]response.IngestionEntryResponse, 0, len(entries))}
	for _, e := range entries {
		page.Entries = append(page.Entries, entryResponse(e))
	}
	// A full page may have more after it; a short page is the last.
	if n := len(entries); n > 0 && n == ingestion.PageSize(limit) {
		next := entries[n-1].Position
		page.NextAfter = &next
	}
	writeJSON(w, nethttp.StatusOK, page)
}

func (h *IngestionHandler) position(w nethttp.ResponseWriter, r *nethttp.Request) (int64, bool) {
	pos, err := strconv.ParseInt(r.PathValue("position"), 10, 64)
	if err != nil || pos <= 0 {
		writeJSON(w, nethttp.StatusBadRequest, map[string]string{"error": "position must be a positive integer"})
		return 0, false
	}
	return pos, true
}

func (h *IngestionHandler) getEntry(w nethttp.ResponseWriter, r *nethttp.Request) {
	pos, ok := h.position(w, r)
	if !ok {
		return
	}
	e, err := h.service.GetEntry(r.Context(), principalOf(r), r.PathValue("id"), pos)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, entryResponse(e))
}

func (h *IngestionHandler) refreshUpload(w nethttp.ResponseWriter, r *nethttp.Request) {
	pos, ok := h.position(w, r)
	if !ok {
		return
	}
	grant, err := h.service.RefreshUpload(r.Context(), principalOf(r), r.PathValue("id"), pos)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, grantResponse(grant))
}

func (h *IngestionHandler) partURLs(w nethttp.ResponseWriter, r *nethttp.Request) {
	pos, ok := h.position(w, r)
	if !ok {
		return
	}
	var body request.PresignPartsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	urls, err := h.service.PartURLs(r.Context(), principalOf(r), r.PathValue("id"), pos, body.PartNumbers)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.PresignPartsResponse{Parts: make([]response.PresignedPartURL, 0, len(urls))}
	for _, u := range urls {
		out.Parts = append(out.Parts, response.PresignedPartURL{PartNumber: u.PartNumber, URL: u.URL, ExpiresAt: u.ExpiresAt})
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *IngestionHandler) confirmParts(w nethttp.ResponseWriter, r *nethttp.Request) {
	pos, ok := h.position(w, r)
	if !ok {
		return
	}
	var body request.RecordPartsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	parts := make([]upload.RecordPartInput, 0, len(body.Parts))
	for _, p := range body.Parts {
		parts = append(parts, upload.RecordPartInput{PartNumber: p.PartNumber, ETag: p.ETag, SizeBytes: p.SizeBytes, ChecksumSHA256: p.ChecksumSHA256})
	}
	if err := h.service.ConfirmParts(r.Context(), principalOf(r), r.PathValue("id"), pos, parts); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *IngestionHandler) heartbeat(w nethttp.ResponseWriter, r *nethttp.Request) {
	pos, ok := h.position(w, r)
	if !ok {
		return
	}
	e, err := h.service.Heartbeat(r.Context(), principalOf(r), r.PathValue("id"), pos)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, entryResponse(e))
}

func (h *IngestionHandler) complete(w nethttp.ResponseWriter, r *nethttp.Request) {
	pos, ok := h.position(w, r)
	if !ok {
		return
	}
	var body request.CompleteMultipartUploadRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	parts := make([]upload.CompletedPartInput, 0, len(body.Parts))
	for _, p := range body.Parts {
		parts = append(parts, upload.CompletedPartInput{PartNumber: p.PartNumber, ETag: p.ETag})
	}
	e, err := h.service.CompleteEntry(r.Context(), principalOf(r), r.PathValue("id"), pos, parts)
	if errors.Is(err, ingestion.ErrCompletionPending) {
		writeJSON(w, nethttp.StatusAccepted, entryResponse(e))
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, entryResponse(e))
}

func (h *IngestionHandler) retry(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.RetryEntriesRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := h.service.Retry(r.Context(), principalOf(r), r.PathValue("id"), body.Positions, body.All)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, response.RetryResponse{Retried: result.Retried, Requeued: result.Requeued})
}

func (h *IngestionHandler) cancel(w nethttp.ResponseWriter, r *nethttp.Request) {
	job, err := h.service.Cancel(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, jobResponse(job))
}

// fail maps service errors to responses. Messages never carry URLs,
// credentials or file content; unexpected errors are logged sanitized and
// answered generically.
func (h *IngestionHandler) fail(w nethttp.ResponseWriter, err error) {
	status := nethttp.StatusInternalServerError
	var quotaErr *model.QuotaError
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.As(err, &quotaErr):
		writeQuotaError(w, quotaErr)
		return
	case errors.Is(err, model.ErrForbidden), errors.Is(err, model.ErrTenantRequired), errors.Is(err, model.ErrRateLimited):
		writeSecurityError(w, h.log, err)
		return
	case errors.Is(err, ingestion.ErrInvalidInput), errors.Is(err, ingestion.ErrStrategyMismatch),
		errors.Is(err, upload.ErrInvalidCompletion), errors.Is(err, upload.ErrInvalidPart),
		errors.Is(err, upload.ErrInvalidRequest):
		status = nethttp.StatusBadRequest
	case errors.Is(err, model.ErrDatasetNotFound), errors.Is(err, model.ErrIngestionNotFound),
		errors.Is(err, model.ErrEntryNotFound), errors.Is(err, upload.ErrMultipartUploadNotFound):
		status = nethttp.StatusNotFound
	case errors.Is(err, model.ErrQuotaExceeded):
		status = nethttp.StatusForbidden
	case errors.Is(err, ingestion.ErrManifestTooLarge):
		status = nethttp.StatusRequestEntityTooLarge
	case errors.Is(err, ingestion.ErrManifestChecksum):
		status = nethttp.StatusUnprocessableEntity
	case errors.Is(err, model.ErrDatasetConflict), errors.Is(err, model.ErrIngestionConflict),
		errors.Is(err, model.ErrManifestConflict), errors.Is(err, model.ErrInvalidJobState),
		errors.Is(err, model.ErrInvalidEntryState), errors.Is(err, model.ErrManifestIncomplete),
		errors.Is(err, ingestion.ErrManifestNotStaged), errors.Is(err, ingestion.ErrUploadNotAttached),
		errors.Is(err, upload.ErrObjectNotUploaded), errors.Is(err, upload.ErrMultipartNotActive),
		errors.Is(err, upload.ErrMultipartNotStarted):
		status = nethttp.StatusConflict
	}
	message := err.Error()
	if status == nethttp.StatusInternalServerError {
		h.log.Error("ingestion request failed", "error", retry.Sanitize(err))
		message = "ingestion request failed"
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func datasetResponse(d model.Dataset) response.DatasetResponse {
	return response.DatasetResponse{
		ID: d.ID, Name: d.Name, CompletionPolicy: string(d.CompletionPolicy), Status: string(d.Status),
		MaxBytes: d.MaxBytes, ReservedBytes: d.ReservedBytes, StoredBytes: d.StoredBytes,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, ReadyAt: d.ReadyAt,
	}
}

func jobResponse(j model.IngestionJob) response.IngestionJobResponse {
	c := j.Counters
	progress := response.IngestionProgress{
		Total: c.Total, Valid: c.Valid(), Invalid: c.Invalid, Pending: c.Pending, Scheduled: c.Scheduled,
		Uploading: c.Uploading, Uploaded: c.Uploaded, Processing: c.Processing, Published: c.Published,
		FailedRetryable: c.Retryable, Failed: c.Failed, Cancelled: c.Cancelled, RequiredFailed: c.RequiredFailed,
		ExpectedBytes: c.ExpectedBytes, StoredBytes: c.StoredBytes,
	}
	if v := c.Valid(); v > 0 {
		progress.PercentPublished = float64(c.Published) * 100 / float64(v)
	}
	return response.IngestionJobResponse{
		ID: j.ID, DatasetID: j.DatasetID, State: string(j.State), MaxConcurrency: j.MaxConcurrency,
		ManifestSource: j.ManifestSource, ManifestBytes: j.ManifestBytes, ManifestSHA256: j.ManifestSHA256,
		ManifestComplete: j.ManifestComplete, ParsedLines: j.ParsedLines, Progress: progress, Error: j.Error,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, FinalizedAt: j.FinalizedAt, CompletedAt: j.CompletedAt,
		CancelledAt: j.CancelledAt,
	}
}

func entryResponse(e model.IngestionEntry) response.IngestionEntryResponse {
	return response.IngestionEntryResponse{
		Position: e.Position, Path: e.Path, SizeBytes: e.SizeBytes, ChecksumSHA256: e.ChecksumSHA256,
		ContentType: e.ContentType, Metadata: e.Metadata, Required: e.Required, Strategy: e.Strategy,
		State: string(e.State), UploadID: e.UploadID, AssetID: e.AssetID, Attempts: e.Attempts,
		ErrorCode: e.ErrorCode, ErrorDetail: e.ErrorDetail, LeaseExpiresAt: e.LeaseExpiresAt,
		NextAttemptAt: e.NextAttemptAt, UpdatedAt: e.UpdatedAt,
	}
}

func grantResponse(g ingestion.Grant) response.GrantResponse {
	out := response.GrantResponse{Entry: entryResponse(g.Entry), UploadURL: g.UploadURL, UploadHeaders: g.UploadHeaders}
	if !g.URLExpiresAt.IsZero() {
		expires := g.URLExpiresAt
		out.URLExpiresAt = &expires
	}
	return out
}

func reportResponse(r ingestion.ManifestReport) response.ManifestReportResponse {
	out := response.ManifestReportResponse{
		Lines: r.Lines, Accepted: r.Accepted, Invalid: r.Invalid, Duplicates: r.Duplicates,
		Replayed: r.Replayed, Complete: r.Complete, SHA256: r.SHA256,
	}
	for _, e := range r.Samples {
		out.Samples = append(out.Samples, entryResponse(e))
	}
	return out
}
