package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/storageconn"
)

// StorageConnectionHandler serves the provider catalog and an
// organization's storage connections. Credentials are accepted on create
// and rotate only and are never written to a response or a log.
type StorageConnectionHandler struct {
	service *storageconn.Service
	log     *slog.Logger
}

func NewStorageConnectionHandler(service *storageconn.Service, log *slog.Logger) *StorageConnectionHandler {
	return &StorageConnectionHandler{service: service, log: log}
}

func (h *StorageConnectionHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"GET /api/v1/storage-providers":                            h.providers,
		"GET /api/v1/storage-providers/capabilities":               h.capabilities,
		"POST /api/v1/storage-connections":                         h.create,
		"GET /api/v1/storage-connections":                          h.list,
		"GET /api/v1/storage-connections/{id}":                     h.get,
		"PATCH /api/v1/storage-connections/{id}":                   h.update,
		"POST /api/v1/storage-connections/{id}/test":               h.test,
		"POST /api/v1/storage-connections/{id}/rotate-credentials": h.rotate,
		"POST /api/v1/storage-connections/{id}/set-default":        h.setDefault,
		"POST /api/v1/storage-connections/{id}/disable":            h.disable,
		"POST /api/v1/storage-connections/{id}/enable":             h.enable,
		"DELETE /api/v1/storage-connections/{id}":                  h.delete,
	}
}

// ─── Responses ────────────────────────────────────────────────────────

func capabilitiesResponse(c model.ProviderCapabilities) response.StorageCapabilities {
	algorithms := c.ChecksumAlgorithms
	if algorithms == nil {
		algorithms = []string{}
	}
	return response.StorageCapabilities{
		PresignedUpload: c.PresignedUpload, MaxSingleUploadBytes: c.MaxSingleUploadBytes,
		Multipart: c.Multipart, MultipartKind: c.MultipartKind, MaxParts: c.MaxParts, MinPartBytes: c.MinPartBytes,
		PartValidator: c.PartValidator, ListIncompleteUploads: c.ListIncompleteUploads, ServerSideCopy: c.ServerSideCopy,
		RangeReads: c.RangeReads, ConditionalReads: c.ConditionalReads, PresignedDownload: c.PresignedDownload,
		ListObjects: c.ListObjects, ProviderVersioning: c.ProviderVersioning, VersioningStatus: c.VersioningStatus,
		VersionIDKind: c.VersionIDKind, ChecksumAlgorithms: algorithms,
	}
}

func connectionResponse(c model.StorageConnection, defaultID string) response.StorageConnectionResponse {
	out := response.StorageConnectionResponse{
		ID: c.ID, Name: c.Name, Provider: string(c.Provider), Status: string(c.Status), AuthMethod: string(c.AuthMethod),
		Bucket: c.Bucket, Region: c.Region, Endpoint: c.Endpoint, AccountID: c.AccountID, ForcePathStyle: c.ForcePathStyle,
		RoleARN: c.RoleARN, ExternalID: c.ExternalID, StagingPrefix: c.StagingPrefix, ReadyPrefix: c.ReadyPrefix,
		StagingMode: string(c.StagingMode), IsDefault: c.ID == defaultID,
		HasCredentials: c.HasCredential(), CredentialRotation: c.CredentialVersion, CredentialRotatedAt: c.CredentialRotatedAt,
		CapabilitiesCheckedAt: c.CapabilitiesCheckedAt, LastTestedAt: c.LastTestedAt, LastTestOKAt: c.LastTestOKAt,
		LastErrorCategory: c.LastErrorCategory, Revision: c.Revision, CreatedBy: c.CreatedBy, UpdatedBy: c.UpdatedBy,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, DisabledAt: c.DisabledAt, DeletedAt: c.DeletedAt,
	}
	if c.Capabilities != nil {
		caps := capabilitiesResponse(*c.Capabilities)
		out.Capabilities = &caps
	}
	return out
}

func testResponse(result storageconn.TestResult) response.StorageTestResponse {
	out := response.StorageTestResponse{
		OK: result.OK, Category: result.Category, Message: result.Message, TestedAt: result.TestedAt,
		Checks: []response.StorageTestCheck{},
	}
	for _, c := range result.Checks {
		out.Checks = append(out.Checks, response.StorageTestCheck{Name: c.Name, OK: c.OK, Category: c.Category})
	}
	if result.Capabilities != nil {
		caps := capabilitiesResponse(*result.Capabilities)
		out.Capabilities = &caps
	}
	return out
}

// writeConnection answers a connection with its revision as the ETag that
// changes send back in If-Match.
func (h *StorageConnectionHandler) writeConnection(w nethttp.ResponseWriter, r *nethttp.Request, status int, c model.StorageConnection) {
	defaultID, err := h.service.DefaultConnection(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(c.Revision, 10)))
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, connectionResponse(c, defaultID))
}

func secretsOf(c request.StorageCredentials) model.StorageSecrets {
	return model.StorageSecrets{
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken,
		ServiceAccountJSON: c.ServiceAccountJSON, AccountKey: c.AccountKey, SASToken: c.SASToken,
	}
}

// ─── Providers ────────────────────────────────────────────────────────

func (h *StorageConnectionHandler) providers(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := principalOf(r).Require(model.PermStorageRead); err != nil {
		h.fail(w, err)
		return
	}
	out := response.StorageProvidersResponse{BYOSEnabled: h.service.Enabled()}
	for _, p := range storageconn.Providers() {
		info := response.StorageProvider{Provider: string(p.Provider), Name: p.Name, RequiredFields: p.RequiredFields, Notes: p.Notes}
		if info.RequiredFields == nil {
			info.RequiredFields = []string{}
		}
		for _, m := range p.AuthMethods {
			fields := m.SecretFields
			if fields == nil {
				fields = []string{}
			}
			info.AuthMethods = append(info.AuthMethods, response.StorageAuthMethod{Method: string(m.Method), SecretFields: fields})
		}
		out.Providers = append(out.Providers, info)
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *StorageConnectionHandler) capabilities(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := principalOf(r).Require(model.PermStorageRead); err != nil {
		h.fail(w, err)
		return
	}
	var out response.ProviderCapabilitiesResponse
	for _, p := range storageconn.Providers() {
		out.Providers = append(out.Providers, response.StorageProviderCapabilities{
			Provider: string(p.Provider), Capabilities: capabilitiesResponse(p.Capabilities),
		})
	}
	writeJSON(w, nethttp.StatusOK, out)
}

// ─── Connections ──────────────────────────────────────────────────────

func (h *StorageConnectionHandler) create(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CreateStorageConnectionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	in := storageconn.CreateInput{
		Name: body.Name, Provider: model.StorageProviderType(body.Provider), AuthMethod: model.StorageAuthMethod(body.AuthMethod),
		Bucket: body.Bucket, Region: body.Region, Endpoint: body.Endpoint, AccountID: body.AccountID,
		ForcePathStyle: body.ForcePathStyle, RoleARN: body.RoleARN, StagingPrefix: body.StagingPrefix,
		ReadyPrefix: body.ReadyPrefix, StagingMode: model.StagingMode(body.StagingMode),
	}
	if body.Credentials != nil {
		in.Secrets = secretsOf(*body.Credentials)
	}
	c, err := h.service.Create(r.Context(), principalOf(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeConnection(w, r, nethttp.StatusCreated, c)
}

func (h *StorageConnectionHandler) list(w nethttp.ResponseWriter, r *nethttp.Request) {
	connections, defaultID, err := h.service.List(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.StorageConnectionsResponse{Connections: []response.StorageConnectionResponse{}, DefaultConnectionID: defaultID}
	for _, c := range connections {
		out.Connections = append(out.Connections, connectionResponse(c, defaultID))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *StorageConnectionHandler) get(w nethttp.ResponseWriter, r *nethttp.Request) {
	c, err := h.service.Get(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeConnection(w, r, nethttp.StatusOK, c)
}

func (h *StorageConnectionHandler) update(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.UpdateStorageConnectionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	revision, err := revisionOf(r, body.Revision)
	if err != nil {
		h.fail(w, err)
		return
	}
	in := storageconn.UpdateInput{Name: body.Name, Region: body.Region, Endpoint: body.Endpoint, ForcePathStyle: body.ForcePathStyle}
	if body.StagingMode != nil {
		mode := model.StagingMode(*body.StagingMode)
		in.StagingMode = &mode
	}
	c, err := h.service.Update(r.Context(), principalOf(r), r.PathValue("id"), revision, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeConnection(w, r, nethttp.StatusOK, c)
}

func (h *StorageConnectionHandler) test(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.TestStorageConnectionRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	c, result, err := h.service.Test(r.Context(), principalOf(r), r.PathValue("id"), body.WriteProbe)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeTest(w, r, c, result)
}

func (h *StorageConnectionHandler) writeTest(w nethttp.ResponseWriter, r *nethttp.Request, c model.StorageConnection, result storageconn.TestResult) {
	out := testResponse(result)
	if c.ID != "" {
		defaultID, err := h.service.DefaultConnection(r.Context(), principalOf(r))
		if err != nil {
			h.fail(w, err)
			return
		}
		connection := connectionResponse(c, defaultID)
		out.Connection = &connection
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *StorageConnectionHandler) rotate(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.RotateStorageCredentialsRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	revision, err := revisionOf(r, body.Revision)
	if err != nil {
		h.fail(w, err)
		return
	}
	c, result, err := h.service.RotateCredentials(r.Context(), principalOf(r), r.PathValue("id"), revision, secretsOf(body.Credentials))
	if errors.Is(err, storageconn.ErrTestFailed) {
		// The new credentials were not stored; the old ones still work.
		out := testResponse(result)
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, nethttp.StatusUnprocessableEntity, out)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeTest(w, r, c, result)
}

func (h *StorageConnectionHandler) setDefault(w nethttp.ResponseWriter, r *nethttp.Request) {
	c, err := h.service.SetDefault(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeConnection(w, r, nethttp.StatusOK, c)
}

func (h *StorageConnectionHandler) disable(w nethttp.ResponseWriter, r *nethttp.Request) {
	c, err := h.service.Disable(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeConnection(w, r, nethttp.StatusOK, c)
}

func (h *StorageConnectionHandler) enable(w nethttp.ResponseWriter, r *nethttp.Request) {
	c, result, err := h.service.Enable(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeTest(w, r, c, result)
}

func (h *StorageConnectionHandler) delete(w nethttp.ResponseWriter, r *nethttp.Request) {
	_, dependents, err := h.service.Delete(r.Context(), principalOf(r), r.PathValue("id"))
	if errors.Is(err, model.ErrConnectionInUse) && dependents.Any() {
		writeJSON(w, nethttp.StatusConflict, response.StorageDependentsResponse{
			Error: err.Error(), Code: "connection_in_use", ActiveUploads: dependents.ActiveUploads,
			LiveAssets: dependents.LiveAssets, Datasets: dependents.Datasets, IsDefault: dependents.IsDefault,
		})
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *StorageConnectionHandler) fail(w nethttp.ResponseWriter, err error) {
	if errors.Is(err, storageconn.ErrInvalidRequest) {
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
		return
	}
	writeSecurityError(w, h.log, err)
}
