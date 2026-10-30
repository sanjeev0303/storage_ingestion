package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/evidence"
)

// EvidenceHandler serves anchors, audit exports and reviews, provenance,
// evidence packs, snapshot holds, model training summaries and control
// mappings (docs/evidence.md).
type EvidenceHandler struct {
	service *evidence.Service
	log     *slog.Logger
}

func NewEvidenceHandler(service *evidence.Service, log *slog.Logger) *EvidenceHandler {
	return &EvidenceHandler{service: service, log: log}
}

func (h *EvidenceHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/evidence-settings":                                 h.settings,
		"PUT /api/v1/evidence-settings":                                 h.putSettings,
		"GET /api/v1/organization/audit/anchors":                        h.anchors,
		"POST /api/v1/organization/audit/anchors":                       h.anchorNow,
		"GET /api/v1/organization/audit/export":                         h.export,
		"POST /api/v1/audit-reviews":                                    h.createReview,
		"GET /api/v1/audit-reviews":                                     h.reviews,
		"GET /api/v1/audit-reviews/{reviewID}":                          h.review,
		"POST /api/v1/audit-reviews/{reviewID}/sign":                    h.sign,
		"GET /api/v1/audit-reviews/{reviewID}/export":                   h.reviewExport,
		"GET /api/v1/datasets/{id}/provenance":                          h.provenance,
		"PUT /api/v1/datasets/{id}/provenance":                          h.putProvenance,
		"GET /api/v1/training-runs/{runID}/provenance-order":            h.runOrder,
		"GET /api/v1/datasets/{id}/versions/{versionID}/evidence-pack":  h.pack,
		"POST /api/v1/datasets/{id}/versions/{versionID}/holds":         h.hold,
		"GET /api/v1/datasets/{id}/versions/{versionID}/holds":          h.holds,
		"POST /api/v1/datasets/{id}/versions/{versionID}/holds/release": h.release,
		"GET /api/v1/models/{modelID}/training-summary":                 h.summary,
		"POST /api/v1/models/{modelID}/training-summary/publish":        h.publish,
		"GET /api/v1/compliance/frameworks":                             h.frameworks,
		"GET /api/v1/compliance/{framework}":                            h.compliance,
		"GET /api/v1/compliance/{framework}/evidence":                   h.complianceEvidence,
		"GET /api/v1/compliance/sec-17a-4/undertaking":                  h.undertaking,
		"POST /api/v1/compliance/regulator-access":                      h.regulatorAccess,
		"GET /api/v1/catalog/as-of":                                     h.asOf,
	}
}

func (h *EvidenceHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidEvidence), errors.Is(err, model.ErrInvalidAPIToken):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrReviewNotFound), errors.Is(err, model.ErrModelNotFound), errors.Is(err, model.ErrUnknownFramework),
		errors.Is(err, model.ErrDatasetNotFound), errors.Is(err, model.ErrDatasetVersionNotFound), errors.Is(err, model.ErrTrainingRunNotFound),
		errors.Is(err, model.ErrHoldNotFound), errors.Is(err, model.ErrConnectionNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrReviewSigned):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "already_signed"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

func timeParam(r *nethttp.Request, name string) (time.Time, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, v)
	return t, err == nil
}

func (h *EvidenceHandler) settings(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Settings(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EvidenceHandler) putSettings(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in model.EvidenceSettings
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.PutSettings(r.Context(), principalOf(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EvidenceHandler) anchors(w nethttp.ResponseWriter, r *nethttp.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := h.service.Anchors(r.Context(), principalOf(r), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"anchors": out})
}

func (h *EvidenceHandler) anchorNow(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.AnchorNow(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

// export answers the chain as JSON lines (the default) or CEF lines.
func (h *EvidenceHandler) export(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after_seq"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	from, ok1 := timeParam(r, "from")
	to, ok2 := timeParam(r, "to")
	if !ok1 || !ok2 {
		h.fail(w, model.ErrInvalidEvidence)
		return
	}
	events, err := h.service.Export(r.Context(), principalOf(r), after, from, to, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	var buf bytes.Buffer
	for _, e := range events {
		if q.Get("format") == "cef" {
			buf.WriteString(evidence.CEF(e) + "\n")
			continue
		}
		line, _ := json.Marshal(e)
		buf.Write(append(line, '\n'))
	}
	if len(events) > 0 {
		w.Header().Set("X-Next-After-Seq", strconv.FormatInt(events[len(events)-1].Seq, 10))
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(nethttp.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (h *EvidenceHandler) createReview(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.CreateReview(r.Context(), principalOf(r), in.From, in.To)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *EvidenceHandler) reviews(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Reviews(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"reviews": out})
}

func (h *EvidenceHandler) review(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Review(r.Context(), principalOf(r), r.PathValue("reviewID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EvidenceHandler) sign(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in struct {
		Note string `json:"note"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.Sign(r.Context(), principalOf(r), r.PathValue("reviewID"), in.Note)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func writeZip(w nethttp.ResponseWriter, name string, data []byte) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(nethttp.StatusOK)
	_, _ = w.Write(data)
}

func (h *EvidenceHandler) reviewExport(w nethttp.ResponseWriter, r *nethttp.Request) {
	data, err := h.service.ReviewExport(r.Context(), principalOf(r), r.PathValue("reviewID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeZip(w, "audit-review-"+r.PathValue("reviewID")+".zip", data)
}

func (h *EvidenceHandler) provenance(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Provenance(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EvidenceHandler) putProvenance(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in model.Provenance
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.PutProvenance(r.Context(), principalOf(r), r.PathValue("id"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EvidenceHandler) runOrder(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.RunOrder(r.Context(), principalOf(r), r.PathValue("runID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

// pack streams the evidence pack. Errors before the first byte are
// answered; after it, the connection is cut so the zip is incomplete.
func (h *EvidenceHandler) pack(w nethttp.ResponseWriter, r *nethttp.Request) {
	opts := evidence.PackOptions{Digests: r.URL.Query().Get("digests") == "true"}
	opts.Reads, _ = strconv.Atoi(r.URL.Query().Get("reads"))
	var buf bytes.Buffer
	if err := h.service.WritePack(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), opts, &buf); err != nil {
		h.fail(w, err)
		return
	}
	writeZip(w, "evidence-"+r.PathValue("versionID")+".zip", buf.Bytes())
}

func (h *EvidenceHandler) hold(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in struct {
		Kind      string `json:"kind"`
		Reference string `json:"reference"`
		Reason    string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, err := h.service.HoldSnapshot(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), in.Kind, in.Reference, in.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *EvidenceHandler) holds(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.SnapshotHolds(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"holds": out})
}

func (h *EvidenceHandler) release(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in struct {
		Matter string `json:"matter"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	n, err := h.service.ReleaseSnapshotHold(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), in.Matter, in.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"released": n})
}

func (h *EvidenceHandler) summary(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.ModelSummary(r.Context(), principalOf(r), r.PathValue("modelID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if r.URL.Query().Get("format") == "md" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(nethttp.StatusOK)
		_, _ = w.Write([]byte(out.Markdown()))
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EvidenceHandler) publish(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.PublishSummary(r.Context(), principalOf(r), r.PathValue("modelID")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *EvidenceHandler) frameworks(w nethttp.ResponseWriter, _ *nethttp.Request) {
	writeJSON(w, nethttp.StatusOK, map[string]any{"frameworks": evidence.Frameworks})
}

func (h *EvidenceHandler) compliance(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Compliance(r.Context(), principalOf(r), r.PathValue("framework"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *EvidenceHandler) complianceEvidence(w nethttp.ResponseWriter, r *nethttp.Request) {
	var buf bytes.Buffer
	if err := h.service.WriteComplianceEvidence(r.Context(), principalOf(r), r.PathValue("framework"), &buf); err != nil {
		h.fail(w, err)
		return
	}
	writeZip(w, r.PathValue("framework")+"-evidence.zip", buf.Bytes())
}

func (h *EvidenceHandler) undertaking(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	text, err := h.service.Undertaking(r.Context(), principalOf(r), q.Get("role"), q.Get("name"), q.Get("title"), q.Get("firm"))
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(nethttp.StatusOK)
	_, _ = w.Write([]byte(text))
}

func (h *EvidenceHandler) regulatorAccess(w nethttp.ResponseWriter, r *nethttp.Request) {
	var in struct {
		Name     string   `json:"name"`
		Days     int      `json:"days"`
		Datasets []string `json:"datasets"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	t, secret, err := h.service.RegulatorAccess(r.Context(), principalOf(r), in.Name, in.Days, in.Datasets)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusCreated, map[string]any{"id": t.ID, "name": t.Name, "expires_at": t.ExpiresAt, "token": secret})
}

func (h *EvidenceHandler) asOf(w nethttp.ResponseWriter, r *nethttp.Request) {
	at, ok := timeParam(r, "at")
	if !ok {
		h.fail(w, model.ErrInvalidEvidence)
		return
	}
	out, err := h.service.AsOf(r.Context(), principalOf(r), r.URL.Query().Get("dataset"), at)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"at": at, "objects": out})
}
