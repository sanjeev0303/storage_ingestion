package http

import (
	"errors"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
)

// CatalogHandler serves /api/v1/catalog: objects by ID and path, their
// versions, listings, and the changes clients may make.
type CatalogHandler struct {
	service *catalog.Service
	// capabilities reports the delivery bucket's provider capabilities;
	// nil leaves the route out.
	capabilities func(*nethttp.Request) (interfaces.StorageCapabilities, error)
	log          *slog.Logger
}

func NewCatalogHandler(service *catalog.Service, capabilities func(*nethttp.Request) (interfaces.StorageCapabilities, error), log *slog.Logger) *CatalogHandler {
	return &CatalogHandler{service: service, capabilities: capabilities, log: log}
}

// Routes maps patterns to handlers; reads are the first map, changes the
// second (they count against different rate limits).
func (h *CatalogHandler) Routes() (reads, changes map[string]nethttp.HandlerFunc) {
	reads = map[string]nethttp.HandlerFunc{
		"GET /api/v1/catalog/objects":                         h.list,
		"GET /api/v1/catalog/objects/{id}":                    h.get,
		"GET /api/v1/catalog/lookup":                          h.lookup,
		"GET /api/v1/catalog/objects/{id}/versions":           h.versions,
		"GET /api/v1/catalog/objects/{id}/versions/{version}": h.version,
	}
	changes = map[string]nethttp.HandlerFunc{
		"PATCH /api/v1/catalog/objects/{id}":                           h.updateMetadata,
		"POST /api/v1/catalog/objects/{id}/rename":                     h.rename,
		"PUT /api/v1/catalog/objects/{id}/versioning":                  h.setVersioning,
		"DELETE /api/v1/catalog/objects/{id}":                          h.deleteObject,
		"POST /api/v1/catalog/objects/{id}/versions/{version}/promote": h.promote,
		"DELETE /api/v1/catalog/objects/{id}/versions/{version}":       h.deleteVersion,
	}
	if h.capabilities != nil {
		reads["GET /api/v1/catalog/capabilities"] = h.storageCapabilities
	}
	return reads, changes
}

func catalogObjectResponse(o model.CatalogObject) response.CatalogObjectResponse {
	out := response.CatalogObjectResponse{
		ID: o.ID, Dataset: o.Dataset, Path: o.Path, Versioning: string(o.Versioning), Revision: o.Revision,
		Metadata: o.Metadata, Versions: o.Versions, DeletedAt: o.DeletedAt, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	if o.Published() {
		out.CurrentVersion = &response.CurrentVersionResponse{
			Version: o.CurrentVersion, AssetID: o.CurrentAssetID, SizeBytes: o.SizeBytes, ContentType: o.ContentType,
			ChecksumSHA256: o.ChecksumSHA256, PublishedAt: o.PublishedAt, StorageConnectionID: o.StorageConnectionID,
		}
	}
	return out
}

func versionResponse(a model.Asset, currentAssetID string) response.ObjectVersionResponse {
	out := response.ObjectVersionResponse{
		Version: a.Version, AssetID: a.ID, Status: string(a.Status), Current: a.ID == currentAssetID, FileName: a.FileName,
		SizeBytes: a.SizeBytes, ContentType: a.ContentType, ChecksumSHA256: a.ChecksumSHA256, ProviderETag: a.StorageETag,
		ProviderVersionID: a.StorageVersionID, Metadata: a.Metadata, PublishedAt: a.PublishedAt, CreatedAt: a.CreatedAt,
		StorageTier: string(a.StorageTier), ReadableUntil: a.ReadableUntil,
	}
	if a.ChecksumSHA256 != "" {
		out.ChecksumAlgorithm = "sha256"
	}
	return out
}

// writeObject answers an object, with its revision as the ETag clients
// send back in If-Match.
func writeObject(w nethttp.ResponseWriter, status int, o model.CatalogObject) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(o.Revision, 10)))
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, catalogObjectResponse(o))
}

func (h *CatalogHandler) list(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	req := catalog.ListRequest{
		Dataset: q.Get("dataset"), AnyDataset: !q.Has("dataset"), Prefix: q.Get("prefix"), ObjectID: q.Get("object_id"),
		AssetID: q.Get("asset_id"), ContentType: q.Get("content_type"), Order: q.Get("order"), Cursor: q.Get("cursor"),
		IncludeUnpublished: q.Get("include") == "unpublished",
	}
	var err error
	if req.Limit, err = optionalInt(q.Get("limit")); err != nil {
		h.fail(w, err)
		return
	}
	for _, size := range []struct {
		name   string
		target *int64
	}{{"min_size", &req.MinSize}, {"max_size", &req.MaxSize}} {
		v, err := optionalInt(q.Get(size.name))
		if err != nil {
			h.fail(w, err)
			return
		}
		*size.target = int64(v)
	}
	for _, t := range []struct {
		name   string
		target *time.Time
	}{
		{"created_after", &req.CreatedAfter}, {"created_before", &req.CreatedBefore},
		{"updated_after", &req.UpdatedAfter}, {"updated_before", &req.UpdatedBefore},
	} {
		if raw := q.Get(t.name); raw != "" {
			if *t.target, err = time.Parse(time.RFC3339, raw); err != nil {
				h.fail(w, fmt.Errorf("%w: %s must be an RFC 3339 time", catalog.ErrInvalidRequest, t.name))
				return
			}
		}
	}
	// metadata.<key>=<value> filters on user metadata, exactly.
	for name, values := range q {
		if key, ok := strings.CutPrefix(name, "metadata."); ok {
			if req.Metadata == nil {
				req.Metadata = map[string]string{}
			}
			req.Metadata[key] = values[0]
		}
	}

	result, err := h.service.List(r.Context(), principalOf(r), req)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.CatalogPageResponse{Objects: make([]response.CatalogObjectResponse, 0, len(result.Objects)), NextCursor: result.NextCursor}
	for _, o := range result.Objects {
		out.Objects = append(out.Objects, catalogObjectResponse(o))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, out)
}

func optionalInt(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a number", catalog.ErrInvalidRequest, raw)
	}
	return n, nil
}

func (h *CatalogHandler) get(w nethttp.ResponseWriter, r *nethttp.Request) {
	o, err := h.service.Get(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeObject(w, nethttp.StatusOK, o)
}

func (h *CatalogHandler) lookup(w nethttp.ResponseWriter, r *nethttp.Request) {
	o, err := h.service.Lookup(r.Context(), principalOf(r), r.URL.Query().Get("dataset"), r.URL.Query().Get("path"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeObject(w, nethttp.StatusOK, o)
}

func (h *CatalogHandler) versions(w nethttp.ResponseWriter, r *nethttp.Request) {
	before, err := optionalInt(r.URL.Query().Get("before"))
	if err != nil {
		h.fail(w, err)
		return
	}
	limit, err := optionalInt(r.URL.Query().Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	p := principalOf(r)
	page, err := h.service.Versions(r.Context(), p, r.PathValue("id"), before, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	o, err := h.service.Get(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.VersionPageResponse{Versions: make([]response.ObjectVersionResponse, 0, len(page.Versions)), NextBefore: page.NextBefore}
	for _, v := range page.Versions {
		out.Versions = append(out.Versions, versionResponse(v, o.CurrentAssetID))
	}
	writeJSON(w, nethttp.StatusOK, out)
}

// version answers one version; "current" names the current one.
func (h *CatalogHandler) version(w nethttp.ResponseWriter, r *nethttp.Request) {
	number := 0
	if raw := r.PathValue("version"); raw != "current" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			h.fail(w, fmt.Errorf("%w: a version is a positive number or current", catalog.ErrInvalidRequest))
			return
		}
		number = n
	}
	p := principalOf(r)
	v, err := h.service.Version(r.Context(), p, r.PathValue("id"), number)
	if err != nil {
		h.fail(w, err)
		return
	}
	o, err := h.service.Get(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, versionResponse(v, o.CurrentAssetID))
}

// revisionOf is the revision a change is conditional on: If-Match, or
// the body's revision.
func revisionOf(r *nethttp.Request, body int64) (int64, error) {
	match := strings.TrimSpace(r.Header.Get("If-Match"))
	if match == "" {
		return body, nil
	}
	n, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(match, "W/"), `"`), 10, 64)
	if err != nil || (body != 0 && body != n) {
		return 0, fmt.Errorf("%w: If-Match must be the object's revision", catalog.ErrInvalidRequest)
	}
	return n, nil
}

func (h *CatalogHandler) updateMetadata(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CatalogMetadataRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	revision, err := revisionOf(r, body.Revision)
	if err != nil {
		h.fail(w, err)
		return
	}
	o, err := h.service.UpdateMetadata(r.Context(), principalOf(r), r.PathValue("id"), revision, body.Metadata)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeObject(w, nethttp.StatusOK, o)
}

func (h *CatalogHandler) rename(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CatalogRenameRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	revision, err := revisionOf(r, body.Revision)
	if err != nil {
		h.fail(w, err)
		return
	}
	o, err := h.service.Rename(r.Context(), principalOf(r), r.PathValue("id"), revision, body.Path)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeObject(w, nethttp.StatusOK, o)
}

func (h *CatalogHandler) setVersioning(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CatalogVersioningRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	revision, err := revisionOf(r, body.Revision)
	if err != nil {
		h.fail(w, err)
		return
	}
	o, err := h.service.SetVersioning(r.Context(), principalOf(r), r.PathValue("id"), revision,
		model.Versioning(strings.ToUpper(body.Versioning)))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeObject(w, nethttp.StatusOK, o)
}

func (h *CatalogHandler) promote(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CatalogRevisionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	revision, err := revisionOf(r, body.Revision)
	if err != nil {
		h.fail(w, err)
		return
	}
	number, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || number <= 0 {
		h.fail(w, fmt.Errorf("%w: a version is a positive number", catalog.ErrInvalidRequest))
		return
	}
	o, err := h.service.Promote(r.Context(), principalOf(r), r.PathValue("id"), number, revision)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeObject(w, nethttp.StatusOK, o)
}

func (h *CatalogHandler) deleteVersion(w nethttp.ResponseWriter, r *nethttp.Request) {
	number, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || number <= 0 {
		h.fail(w, fmt.Errorf("%w: a version is a positive number", catalog.ErrInvalidRequest))
		return
	}
	v, err := h.service.DeleteVersion(r.Context(), principalOf(r), r.PathValue("id"), number)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, versionResponse(v, ""))
}

func (h *CatalogHandler) deleteObject(w nethttp.ResponseWriter, r *nethttp.Request) {
	o, inFlight, err := h.service.DeleteObject(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, map[string]any{"object": catalogObjectResponse(o), "versions_in_processing": inFlight})
}

func (h *CatalogHandler) storageCapabilities(w nethttp.ResponseWriter, r *nethttp.Request) {
	if !principalOf(r).Can(model.PermAssetsRead) {
		writeSecurityError(w, h.log, model.ErrForbidden)
		return
	}
	c, err := h.capabilities(r)
	if err != nil {
		h.log.Error("storage capabilities", "error", err)
		writeJSON(w, nethttp.StatusServiceUnavailable, problem{Error: "the storage provider did not answer", Code: "storage_unavailable"})
		return
	}
	writeJSON(w, nethttp.StatusOK, response.StorageCapabilitiesResponse{
		LogicalVersioning: true, ProviderVersioning: c.ProviderVersioning, VersioningStatus: c.VersioningStatus,
	})
}

func (h *CatalogHandler) fail(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrInvalidRequest), errors.Is(err, model.ErrInvalidCursor):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, catalog.ErrRevisionRequired):
		writeJSON(w, nethttp.StatusPreconditionRequired, problem{Error: err.Error(), Code: "revision_required"})
	case errors.Is(err, assetservice.ErrNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: model.ErrVersionNotFound.Error(), Code: "not_found"})
	case errors.Is(err, model.ErrDeletionBlocked):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "deletion_blocked"})
	default:
		writeSecurityError(w, h.log, err)
	}
}
