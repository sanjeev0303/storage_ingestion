package http

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/adoption"
)

// AdoptionHandler serves adoption jobs (docs/adoption.md) and the MHL and
// BagIt exports of dataset versions.
type AdoptionHandler struct {
	service  *adoption.Service
	exporter adoption.Exporter
	log      *slog.Logger
}

func NewAdoptionHandler(service *adoption.Service, exporter adoption.Exporter, log *slog.Logger) *AdoptionHandler {
	return &AdoptionHandler{service: service, exporter: exporter, log: log}
}

func (h *AdoptionHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"POST /api/v1/adoptions":                                             h.create,
		"GET /api/v1/adoptions":                                              h.list,
		"GET /api/v1/adoptions/{id}":                                         h.get,
		"GET /api/v1/adoptions/{id}/results":                                 h.results,
		"POST /api/v1/adoptions/{id}/cancel":                                 h.cancel,
		"GET /api/v1/datasets/{id}/versions/{versionID}/mhl":                 h.mhl,
		"GET /api/v1/datasets/{id}/versions/{versionID}/manifest-sha256.txt": h.bag,
	}
}

func (h *AdoptionHandler) error(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrAdoptionNotFound), errors.Is(err, model.ErrVersionNotFound), errors.Is(err, model.ErrDatasetNotFound),
		errors.Is(err, model.ErrDatasetVersionNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrInvalidAdoption):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_adoption"})
	case errors.Is(err, model.ErrConnectionNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "storage connection not found", Code: "not_found"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

func (h *AdoptionHandler) create(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		ConnectionID  string `json:"connection_id"`
		Dataset       string `json:"dataset"`
		Source        string `json:"source"`
		SourceRef     string `json:"source_ref"`
		Inline        string `json:"inline"`
		Mode          string `json:"mode"`
		RequireDigest *bool  `json:"require_digest"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	require := body.RequireDigest == nil || *body.RequireDigest
	job, err := h.service.CreateFor(r.Context(), principalOf(r), adoption.CreateInput{
		ConnectionID: body.ConnectionID, Dataset: body.Dataset, Source: body.Source, SourceRef: body.SourceRef,
		Inline: body.Inline, Mode: body.Mode, RequireDigest: require,
	})
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, job)
}

func (h *AdoptionHandler) list(w nethttp.ResponseWriter, r *nethttp.Request) {
	jobs, err := h.service.JobsFor(r.Context(), principalOf(r))
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"adoptions": append([]model.AdoptionJob{}, jobs...)})
}

func (h *AdoptionHandler) get(w nethttp.ResponseWriter, r *nethttp.Request) {
	job, err := h.service.JobFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, job)
}

func (h *AdoptionHandler) cancel(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.CancelFor(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		h.error(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

// results answers a job's per-entry results: a JSON page (?after=&limit=),
// or the whole file with ?format=csv or ?format=jsonl.
func (h *AdoptionHandler) results(w nethttp.ResponseWriter, r *nethttp.Request) {
	ctx, p, id := r.Context(), principalOf(r), r.PathValue("id")
	format := r.URL.Query().Get("format")
	if format == "" {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = 500
		}
		entries, err := h.service.EntriesFor(ctx, p, id, after, limit)
		if err != nil {
			h.error(w, err)
			return
		}
		next := int64(0)
		if len(entries) == limit {
			next = entries[len(entries)-1].Line
		}
		writeJSON(w, nethttp.StatusOK, map[string]any{"entries": append([]model.AdoptionEntry{}, entries...), "next_after": next})
		return
	}
	if format != "csv" && format != "jsonl" {
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: "format is csv or jsonl", Code: "invalid_request"})
		return
	}
	first, err := h.service.EntriesFor(ctx, p, id, 0, 5000)
	if err != nil {
		h.error(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="adoption-%s.%s"`, id, format))
	var out *csv.Writer
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		out = csv.NewWriter(w)
		_ = out.Write([]string{"line", "object_key", "url", "path", "state", "code", "detail", "asset_id", "upload_id", "sha256", "size_bytes"})
		out.Flush()
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	for entries := first; len(entries) > 0; {
		for _, e := range entries {
			if out != nil {
				_ = out.Write([]string{strconv.FormatInt(e.Line, 10), e.ObjectKey, e.URL, e.Path, e.State, e.Code, e.Detail,
					e.AssetID, e.UploadID, e.SHA256, strconv.FormatInt(e.SizeBytes, 10)})
			} else {
				line, _ := jsonLine(e)
				_, _ = w.Write(line)
			}
		}
		if out != nil {
			out.Flush()
		}
		if len(entries) < 5000 {
			break
		}
		if entries, err = h.service.EntriesFor(ctx, p, id, entries[len(entries)-1].Line, 5000); err != nil {
			h.log.Warn("adoption results cut short", "job_id", id, "error", err)
			return
		}
	}
}

func (h *AdoptionHandler) mhl(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Disposition", `attachment; filename="`+safeName(r.PathValue("versionID"))+`.mhl"`)
	if err := h.exporter.MHL(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), w); err != nil {
		h.error(w, err)
	}
}

func (h *AdoptionHandler) bag(w nethttp.ResponseWriter, r *nethttp.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := h.exporter.BagManifest(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), w); err != nil {
		h.error(w, err)
	}
}

func jsonLine(v any) ([]byte, error) {
	line, err := json.Marshal(v)
	return append(line, '\n'), err
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
