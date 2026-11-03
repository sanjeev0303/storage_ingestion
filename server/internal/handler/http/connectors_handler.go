package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	nethttp "net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/connectors"
	"github.com/sanjeev0303/object-storage-platform/internal/service/delivery"
)

// ConnectorsHandler serves chat shares, collections, Git LFS and DICOMweb
// (docs/connectors.md).
type ConnectorsHandler struct {
	service *connectors.Service
	log     *slog.Logger
}

func NewConnectorsHandler(service *connectors.Service, log *slog.Logger) *ConnectorsHandler {
	return &ConnectorsHandler{service: service, log: log}
}

func (h *ConnectorsHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/chat-integrations":          h.integrations,
		"POST /api/v1/chat-integrations":         h.addIntegration,
		"DELETE /api/v1/chat-integrations/{id}":  h.deleteIntegration,
		"GET /api/v1/chat-shares":                h.shares,
		"POST /api/v1/chat-shares":               h.share,
		"GET /api/v1/chat-shares/{id}":           h.getShare,
		"POST /api/v1/chat-shares/{id}/open":     h.openShare,
		"DELETE /api/v1/chat-shares/{id}":        h.revokeShare,
		"GET /api/v1/datasets/{id}/collections":  h.collections,
		"POST /api/v1/datasets/{id}/collections": h.addCollection,
	}
}

// LFSRoutes follow the Git LFS Batch and File Locking APIs; lfs.url is
// <api>/lfs/<dataset namespace>.
func (h *ConnectorsHandler) LFSRoutes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"POST /lfs/{dataset}/objects/batch":     h.lfsBatch,
		"POST /lfs/{dataset}/verify":            h.lfsVerify,
		"POST /lfs/{dataset}/locks":             h.lfsLock,
		"GET /lfs/{dataset}/locks":              h.lfsLocks,
		"POST /lfs/{dataset}/locks/verify":      h.lfsLocksVerify,
		"POST /lfs/{dataset}/locks/{id}/unlock": h.lfsUnlock,
	}
}

// DICOMwebRoutes are read-only: QIDO-RS searches and WADO-RS retrieval over
// one snapshot, with the platform's sign-in.
func (h *ConnectorsHandler) DICOMwebRoutes() map[string]nethttp.HandlerFunc {
	base := "GET /dicomweb/{id}/{versionID}/studies"
	return map[string]nethttp.HandlerFunc{
		base:                                                   h.qidoStudies,
		base + "/{study}":                                      h.wado,
		base + "/{study}/metadata":                             h.wadoMetadata,
		base + "/{study}/series":                               h.qidoSeries,
		base + "/{study}/series/{series}":                      h.wado,
		base + "/{study}/series/{series}/metadata":             h.wadoMetadata,
		base + "/{study}/series/{series}/instances":            h.qidoInstances,
		base + "/{study}/series/{series}/instances/{instance}": h.wado,
		base + "/{study}/series/{series}/instances/{instance}/metadata": h.wadoMetadata,
	}
}

func (h *ConnectorsHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidConn):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrChatNotFound), errors.Is(err, model.ErrDatasetNotFound), errors.Is(err, model.ErrDatasetVersionNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrChatExpired):
		writeJSON(w, nethttp.StatusGone, problem{Error: err.Error(), Code: "link_expired"})
	case errors.Is(err, model.ErrChatDelivery):
		writeJSON(w, nethttp.StatusBadGateway, problem{Error: err.Error(), Code: "chat_delivery"})
	case errors.Is(err, model.ErrVersionSuperseded):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "version_superseded"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

// ─── Chat ─────────────────────────────────────────────────────────────

func (h *ConnectorsHandler) integrations(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Integrations(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"integrations": out})
}

func (h *ConnectorsHandler) addIntegration(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	out, err := h.service.AddIntegration(r.Context(), principalOf(r), body.Kind, body.Name, body.URL)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *ConnectorsHandler) deleteIntegration(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.DeleteIntegration(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *ConnectorsHandler) shares(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Shares(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"shares": out})
}

func (h *ConnectorsHandler) share(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		AssetID       string `json:"asset_id"`
		IntegrationID string `json:"integration_id"`
		Note          string `json:"note"`
		ExpiresIn     string `json:"expires_in"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	var life time.Duration
	if body.ExpiresIn != "" {
		var err error
		if life, err = time.ParseDuration(body.ExpiresIn); err != nil {
			writeJSON(w, nethttp.StatusBadRequest, problem{Error: "expires_in is a duration, such as 24h", Code: "invalid_request"})
			return
		}
	}
	out, err := h.service.ShareToChat(r.Context(), principalOf(r), body.AssetID, body.IntegrationID, body.Note, life)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *ConnectorsHandler) getShare(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Share(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *ConnectorsHandler) openShare(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.OpenShare(r.Context(), principalOf(r), r.PathValue("id"), middleware.RequestIDFrom(r.Context()))
	if errors.Is(err, model.ErrChatNotFound) || errors.Is(err, model.ErrChatExpired) {
		h.fail(w, err)
		return
	}
	if err != nil {
		writeDeliveryError(w, h.log, err, 0)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusOK, map[string]any{"url": out.URL, "expires_at": out.ExpiresAt})
}

func (h *ConnectorsHandler) revokeShare(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.RevokeShare(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

// ─── Collections ──────────────────────────────────────────────────────

func (h *ConnectorsHandler) collections(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.service.Collections(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"collections": out})
}

func (h *ConnectorsHandler) addCollection(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body model.CollectionImport
	if !decodeJSON(w, r, &body) {
		return
	}
	body.DatasetID = r.PathValue("id")
	out, err := h.service.RecordCollection(r.Context(), principalOf(r), body)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, out)
}

// ─── Git LFS ──────────────────────────────────────────────────────────

const lfsType = "application/vnd.git-lfs+json"

func lfsJSON(w nethttp.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", lfsType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *ConnectorsHandler) lfsFail(w nethttp.ResponseWriter, err error) {
	status := nethttp.StatusInternalServerError
	message := "the request failed"
	switch {
	case errors.Is(err, model.ErrForbidden):
		status, message = nethttp.StatusForbidden, "you may not do this"
	case errors.Is(err, model.ErrInvalidConn):
		status, message = nethttp.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, model.ErrChatNotFound):
		status, message = nethttp.StatusNotFound, "not found"
	case errors.Is(err, model.ErrLockHeld):
		status, message = nethttp.StatusForbidden, err.Error()
	default:
		h.log.Warn("git lfs request failed", "error", err)
	}
	lfsJSON(w, status, map[string]string{"message": message})
}

func lfsDecode(w nethttp.ResponseWriter, r *nethttp.Request, into any) bool {
	r.Body = nethttp.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		lfsJSON(w, nethttp.StatusUnprocessableEntity, map[string]string{"message": "invalid JSON"})
		return false
	}
	return true
}

// baseURL is how the client reached this endpoint, for the verify action.
func baseURL(r *nethttp.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	return scheme + "://" + r.Host + "/lfs/" + r.PathValue("dataset")
}

func (h *ConnectorsHandler) lfsBatch(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Operation string                 `json:"operation"`
		Transfers []string               `json:"transfers"`
		Objects   []connectors.LFSObject `json:"objects"`
	}
	if !lfsDecode(w, r, &body) {
		return
	}
	if len(body.Transfers) > 0 && !slices.Contains(body.Transfers, "basic") {
		lfsJSON(w, nethttp.StatusUnprocessableEntity, map[string]string{"message": "only the basic transfer adapter is supported"})
		return
	}
	objects, err := h.service.LFSBatch(r.Context(), principalOf(r), r.PathValue("dataset"), body.Operation, baseURL(r), r.Header.Get("Authorization"),
		middleware.RequestIDFrom(r.Context()), body.Objects)
	if err != nil {
		h.lfsFail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	lfsJSON(w, nethttp.StatusOK, map[string]any{"transfer": "basic", "objects": objects, "hash_algo": "sha256"})
}

func (h *ConnectorsHandler) lfsVerify(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		OID string `json:"oid"`
	}
	if !lfsDecode(w, r, &body) {
		return
	}
	if err := h.service.LFSVerify(r.Context(), principalOf(r), r.PathValue("dataset"), r.URL.Query().Get("upload"), body.OID); err != nil {
		h.lfsFail(w, err)
		return
	}
	lfsJSON(w, nethttp.StatusOK, map[string]string{})
}

func lfsLockJSON(l model.LFSLock) map[string]any {
	return map[string]any{"id": l.ID, "path": l.Path, "locked_at": l.LockedAt.Format(time.RFC3339), "owner": map[string]string{"name": l.OwnerID}}
}

func (h *ConnectorsHandler) lfsLock(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !lfsDecode(w, r, &body) {
		return
	}
	l, taken, err := h.service.LFSLock(r.Context(), principalOf(r), r.PathValue("dataset"), body.Path)
	if err != nil {
		h.lfsFail(w, err)
		return
	}
	if !taken {
		lfsJSON(w, nethttp.StatusConflict, map[string]any{"lock": lfsLockJSON(l), "message": "already locked"})
		return
	}
	lfsJSON(w, nethttp.StatusCreated, map[string]any{"lock": lfsLockJSON(l)})
}

func (h *ConnectorsHandler) lfsLocks(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	locks, err := h.service.LFSLocks(r.Context(), principalOf(r), r.PathValue("dataset"), q.Get("path"), q.Get("id"))
	if err != nil {
		h.lfsFail(w, err)
		return
	}
	out := make([]map[string]any, len(locks))
	for i, l := range locks {
		out[i] = lfsLockJSON(l)
	}
	lfsJSON(w, nethttp.StatusOK, map[string]any{"locks": out})
}

// lfsLocksVerify sorts the locks into the caller's own and others', so a
// push can refuse to overwrite a file somebody else holds.
func (h *ConnectorsHandler) lfsLocksVerify(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !lfsDecode(w, r, &struct{}{}) {
		return
	}
	p := principalOf(r)
	locks, err := h.service.LFSLocks(r.Context(), p, r.PathValue("dataset"), "", "")
	if err != nil {
		h.lfsFail(w, err)
		return
	}
	ours, theirs := []map[string]any{}, []map[string]any{}
	for _, l := range locks {
		if l.OwnerID == p.UserID {
			ours = append(ours, lfsLockJSON(l))
		} else {
			theirs = append(theirs, lfsLockJSON(l))
		}
	}
	lfsJSON(w, nethttp.StatusOK, map[string]any{"ours": ours, "theirs": theirs})
}

func (h *ConnectorsHandler) lfsUnlock(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	if !lfsDecode(w, r, &body) {
		return
	}
	l, err := h.service.LFSUnlock(r.Context(), principalOf(r), r.PathValue("dataset"), r.PathValue("id"), body.Force)
	if err != nil {
		h.lfsFail(w, err)
		return
	}
	lfsJSON(w, nethttp.StatusOK, map[string]any{"lock": lfsLockJSON(l)})
}

// ─── DICOMweb ─────────────────────────────────────────────────────────

const dicomJSON = "application/dicom+json"

func (h *ConnectorsHandler) instances(w nethttp.ResponseWriter, r *nethttp.Request, f model.DicomFilter) ([]model.DicomInstance, bool) {
	out, err := h.service.DicomInstances(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), f)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	return out, true
}

// A QIDO parameter is named by keyword or by tag.
func qido(r *nethttp.Request, keyword, tag string) string {
	if v := r.URL.Query().Get(keyword); v != "" {
		return v
	}
	return r.URL.Query().Get(tag)
}

func writeDicom(w nethttp.ResponseWriter, r *nethttp.Request, rows []map[string]map[string]any) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if offset > 0 {
		rows = rows[min(offset, len(rows)):]
	}
	if limit > 0 {
		rows = rows[:min(limit, len(rows))]
	}
	if len(rows) == 0 {
		w.WriteHeader(nethttp.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", dicomJSON)
	_ = json.NewEncoder(w).Encode(rows)
}

func withCount(tags map[string]map[string]any, tag, vr string, value any) {
	tags[tag] = map[string]any{"vr": vr, "Value": []any{value}}
}

// group makes one row per key from instances in order: the first instance's
// attributes, with what the caller adds.
func group(in []model.DicomInstance, key func(model.DicomInstance) string, keep map[string]bool, finish func(row map[string]map[string]any, members []model.DicomInstance)) []map[string]map[string]any {
	order := []string{}
	byKey := map[string][]model.DicomInstance{}
	for _, i := range in {
		k := key(i)
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], i)
	}
	out := make([]map[string]map[string]any, 0, len(order))
	for _, k := range order {
		members := byKey[k]
		row := map[string]map[string]any{}
		for tag, attr := range members[0].Tags {
			if keep[tag] {
				row[tag] = attr
			}
		}
		finish(row, members)
		out = append(out, row)
	}
	return out
}

var studyTags = map[string]bool{"00080020": true, "00080030": true, "00080050": true, "00081030": true, "00100010": true, "00100020": true,
	"00100030": true, "00100040": true, "0020000D": true, "00200010": true}
var seriesTags = map[string]bool{"00080060": true, "0008103E": true, "0020000E": true, "00200011": true}

func (h *ConnectorsHandler) qidoStudies(w nethttp.ResponseWriter, r *nethttp.Request) {
	in, ok := h.instances(w, r, model.DicomFilter{Study: qido(r, "StudyInstanceUID", "0020000D"), PatientID: qido(r, "PatientID", "00100020"),
		StudyDate: qido(r, "StudyDate", "00080020")})
	if !ok {
		return
	}
	wantModality := qido(r, "ModalitiesInStudy", "00080061")
	rows := group(in, func(i model.DicomInstance) string { return i.StudyUID }, studyTags, func(row map[string]map[string]any, members []model.DicomInstance) {
		modalities, series := []any{}, map[string]bool{}
		for _, m := range members {
			series[m.SeriesUID] = true
			if m.Modality != "" && !slices.Contains(modalities, any(m.Modality)) {
				modalities = append(modalities, m.Modality)
			}
		}
		row["00080061"] = map[string]any{"vr": "CS", "Value": modalities}
		withCount(row, "00201206", "IS", len(series))
		withCount(row, "00201208", "IS", len(members))
	})
	if wantModality != "" {
		kept := rows[:0]
		for _, row := range rows {
			if slices.Contains(row["00080061"]["Value"].([]any), any(wantModality)) {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	writeDicom(w, r, rows)
}

func (h *ConnectorsHandler) qidoSeries(w nethttp.ResponseWriter, r *nethttp.Request) {
	in, ok := h.instances(w, r, model.DicomFilter{Study: r.PathValue("study"), Series: qido(r, "SeriesInstanceUID", "0020000E"), Modality: qido(r, "Modality", "00080060")})
	if !ok {
		return
	}
	writeDicom(w, r, group(in, func(i model.DicomInstance) string { return i.SeriesUID }, seriesTags, func(row map[string]map[string]any, members []model.DicomInstance) {
		withCount(row, "00201209", "IS", len(members))
	}))
}

func (h *ConnectorsHandler) qidoInstances(w nethttp.ResponseWriter, r *nethttp.Request) {
	in, ok := h.instances(w, r, model.DicomFilter{Study: r.PathValue("study"), Series: r.PathValue("series"), Instance: qido(r, "SOPInstanceUID", "00080018")})
	if !ok {
		return
	}
	rows := make([]map[string]map[string]any, len(in))
	for i, inst := range in {
		rows[i] = inst.Tags
	}
	writeDicom(w, r, rows)
}

func pathFilter(r *nethttp.Request) model.DicomFilter {
	return model.DicomFilter{Study: r.PathValue("study"), Series: r.PathValue("series"), Instance: r.PathValue("instance")}
}

func (h *ConnectorsHandler) wadoMetadata(w nethttp.ResponseWriter, r *nethttp.Request) {
	in, ok := h.instances(w, r, pathFilter(r))
	if !ok {
		return
	}
	if len(in) == 0 {
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
		return
	}
	rows := make([]map[string]map[string]any, len(in))
	for i, inst := range in {
		rows[i] = inst.Tags
	}
	w.Header().Set("Content-Type", dicomJSON)
	_ = json.NewEncoder(w).Encode(rows)
}

// wado answers multipart/related with one application/dicom part for each
// instance, as WADO-RS asks. Only the Part 10 files as stored are served.
func (h *ConnectorsHandler) wado(w nethttp.ResponseWriter, r *nethttp.Request) {
	if accept := r.Header.Get("Accept"); accept != "" && !strings.Contains(accept, "application/dicom") && !strings.Contains(accept, "*/*") {
		writeJSON(w, nethttp.StatusNotAcceptable, problem{Error: "only multipart/related; type=\"application/dicom\" is served", Code: "not_acceptable"})
		return
	}
	in, ok := h.instances(w, r, pathFilter(r))
	if !ok {
		return
	}
	if len(in) == 0 {
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
		return
	}
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", fmt.Sprintf(`multipart/related; type="application/dicom"; boundary=%s`, mw.Boundary()))
	err := h.service.DicomRead(r.Context(), principalOf(r), in, middleware.RequestIDFrom(r.Context()), func(f delivery.ShardFile) error {
		part, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/dicom"}, "Content-Length": {strconv.FormatInt(f.Size, 10)}})
		if err != nil {
			return err
		}
		_, err = io.Copy(part, f.Body)
		return err
	})
	if err != nil {
		// The headers are gone: cut the response so the client sees it failed.
		h.log.Warn("wado-rs stream failed", "error", err)
		panic(nethttp.ErrAbortHandler)
	}
	_ = mw.Close()
}

// LFSCredentials lets Git's credential helpers sign in: the API token is the
// Basic password. It also tells Git LFS how to ask, on a 401.
func LFSCredentials(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if _, password, ok := r.BasicAuth(); ok {
			r.Header.Set("Authorization", "Bearer "+password)
		}
		w.Header().Set("LFS-Authenticate", `Basic realm="object storage platform"`)
		next.ServeHTTP(w, r)
	})
}
