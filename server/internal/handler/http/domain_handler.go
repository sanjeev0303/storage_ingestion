package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/domain"
)

// DomainHandler serves STAC, recordings, segments, embedding indexes,
// upstream sources and publications (docs/domain.md).
type DomainHandler struct {
	service *domain.Service
	log     *slog.Logger
}

func NewDomainHandler(service *domain.Service, log *slog.Logger) *DomainHandler {
	return &DomainHandler{service: service, log: log}
}

func (h *DomainHandler) Routes() map[string]nethttp.HandlerFunc {
	const version = "/api/v1/datasets/{id}/versions/{versionID}"
	return map[string]nethttp.HandlerFunc{
		"GET " + version + "/stac/collection.json":    h.stacCollection,
		"GET " + version + "/stac/items.json":         h.stacItems,
		"GET " + version + "/recordings":              h.recordings,
		"POST " + version + "/segments":               h.addSegments,
		"GET " + version + "/segments":                h.segments,
		"DELETE " + version + "/segments/{segmentID}": h.deleteSegment,
		"POST " + version + "/embedding-indexes":      h.createIndex,
		"GET " + version + "/embedding-indexes":       h.indexes,
		"DELETE /api/v1/embedding-indexes/{id}":       h.deleteIndex,
		"POST /api/v1/embedding-indexes/{id}/vectors": h.putVectors,
		"POST /api/v1/embedding-indexes/{id}/query":   h.query,
		"GET /api/v1/upstream-sources":                h.upstream,
		"POST " + version + "/publish":                h.publish,
		"GET " + version + "/publication":             h.publication,
		"POST " + version + "/publication/doi":        h.mintDOI,
		"POST " + version + "/publication/withdraw":   h.withdraw,
		"GET /api/v1/datasets/{id}/publications":      h.publications,
		"GET " + version + "/retention-cost":          h.retentionCost,
	}
}

func (h *DomainHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidDomain), errors.Is(err, dataset.ErrInvalidRequest):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrDomainNotFound), errors.Is(err, model.ErrDatasetNotFound), errors.Is(err, model.ErrVersionNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrAlreadyPublished):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "already_published"})
	case errors.Is(err, model.ErrPublishingOff):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "publishing_not_configured"})
	case errors.Is(err, model.ErrDOIOff):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "doi_not_configured"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

func (h *DomainHandler) stacCollection(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.STACCollection(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

// countingWriter tells whether a stream has started: after that, a failure
// can no longer be answered with a status.
type countingWriter struct {
	nethttp.ResponseWriter
	wrote bool
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.wrote = true
		c.Header().Set("Content-Type", "application/geo+json")
	}
	return c.ResponseWriter.Write(b)
}

func (h *DomainHandler) stacItems(w nethttp.ResponseWriter, r *nethttp.Request) {
	out := &countingWriter{ResponseWriter: w}
	err := h.service.WriteSTACItems(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), out)
	if err == nil {
		return
	}
	if out.wrote {
		h.log.Warn("STAC export stopped", "error", err)
		panic(nethttp.ErrAbortHandler) // a cut stream must not look complete
	}
	h.fail(w, err)
}

func queryNS(r *nethttp.Request, name string) (*int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, errors.Join(model.ErrInvalidDomain, errors.New(name+" is a number of nanoseconds"))
	}
	return &v, nil
}

func (h *DomainHandler) recordings(w nethttp.ResponseWriter, r *nethttp.Request) {
	from, err := queryNS(r, "from_ns")
	if err != nil {
		h.fail(w, err)
		return
	}
	to, err := queryNS(r, "to_ns")
	if err != nil {
		h.fail(w, err)
		return
	}
	out, err := h.service.Recordings(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), r.URL.Query().Get("topic"), from, to)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"recordings": out})
}

func (h *DomainHandler) addSegments(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Segments []model.Segment `json:"segments"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.AddSegments(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), body.Segments)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, map[string]any{"segments": out})
}

func (h *DomainHandler) segments(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	from, err := queryNS(r, "from_ns")
	if err != nil {
		h.fail(w, err)
		return
	}
	to, err := queryNS(r, "to_ns")
	if err != nil {
		h.fail(w, err)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	out, err := h.service.Segments(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), model.SegmentFilter{
		Path: q.Get("path"), Label: q.Get("label"), FromNS: from, ToNS: to, AfterID: q.Get("after"), Limit: limit})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"segments": out})
}

func (h *DomainHandler) deleteSegment(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.DeleteSegment(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), r.PathValue("segmentID")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *DomainHandler) createIndex(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Name       string `json:"name"`
		Model      string `json:"model"`
		Dimensions int    `json:"dimensions"`
		Metric     string `json:"metric"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.CreateIndex(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"),
		model.EmbeddingIndex{Name: body.Name, Model: body.Model, Dimensions: body.Dimensions, Metric: body.Metric})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *DomainHandler) indexes(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Indexes(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"indexes": out})
}

func (h *DomainHandler) deleteIndex(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.DeleteIndex(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *DomainHandler) putVectors(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Vectors []model.Embedding `json:"vectors"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	n, err := h.service.PutVectors(r.Context(), principalOf(r), r.PathValue("id"), body.Vectors)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"written": n})
}

func (h *DomainHandler) query(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Vector []float32 `json:"vector"`
		Top    int       `json:"top"`
		Path   string    `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.Query(r.Context(), principalOf(r), r.PathValue("id"), body.Vector, body.Top, body.Path)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"matches": out})
}

func (h *DomainHandler) upstream(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Upstream(r.Context(), principalOf(r), r.URL.Query().Get("dataset_id"), r.URL.Query().Get("state"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"sources": out})
}

func (h *DomainHandler) publish(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		model.PublicationMeta
		MintDOI bool `json:"mint_doi"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.Publish(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), body.PublicationMeta, body.MintDOI)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, h.withLanding(out))
}

func (h *DomainHandler) withLanding(p model.Publication) map[string]any {
	return map[string]any{"publication": p, "landing_url": h.service.LandingURL(p.Slug)}
}

func (h *DomainHandler) publication(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Publication(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, h.withLanding(out))
}

func (h *DomainHandler) mintDOI(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.MintDOI(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, h.withLanding(out))
}

func (h *DomainHandler) withdraw(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.Withdraw(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), body.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, h.withLanding(out))
}

func (h *DomainHandler) publications(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Publications(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"publications": out})
}

func (h *DomainHandler) retentionCost(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	years := 10
	if raw := q.Get("years"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			h.fail(w, errors.Join(model.ErrInvalidDomain, errors.New("years is a whole number")))
			return
		}
		years = v
	}
	price := 0.0
	if raw := q.Get("price_per_gb_month"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			h.fail(w, errors.Join(model.ErrInvalidDomain, errors.New("price_per_gb_month is a number")))
			return
		}
		price = v
	}
	out, err := h.service.RetentionCost(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), years, price)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

// Public answers a publication's public record: no credential, limited by
// address.
func (h *DomainHandler) Public(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Public(r.Context(), r.PathValue("slug"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}
