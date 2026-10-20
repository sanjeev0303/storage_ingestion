package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	nethttp "net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
	"github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// DatasetHandler serves managed datasets: the datasets themselves, their
// members, manifest imports, versions, validations, reports, readiness
// assessments, lineage and validation profiles. Every route runs behind
// authentication; the service scopes every call to the caller's tenant
// and checks the caller's permissions.
type DatasetHandler struct {
	service *dataset.Service
	log     *slog.Logger
}

func NewDatasetHandler(service *dataset.Service, log *slog.Logger) *DatasetHandler {
	return &DatasetHandler{service: service, log: log}
}

// Routes maps patterns to handlers. It includes POST /api/v1/datasets and
// GET /api/v1/datasets/{id}, which take over from the ingestion handler's
// and accept the same requests.
func (h *DatasetHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"POST /api/v1/datasets":                                  h.create,
		"GET /api/v1/datasets":                                   h.list,
		"GET /api/v1/datasets/{id}":                              h.get,
		"PATCH /api/v1/datasets/{id}":                            h.update,
		"POST /api/v1/datasets/{id}/archive":                     h.archive,
		"GET /api/v1/datasets/{id}/members":                      h.members,
		"POST /api/v1/datasets/{id}/members":                     h.addMembers,
		"DELETE /api/v1/datasets/{id}/members/{memberID}":        h.removeMember,
		"POST /api/v1/datasets/{id}/manifests":                   h.importManifest,
		"GET /api/v1/datasets/{id}/manifests/{jobID}":            h.manifestImport,
		"GET /api/v1/datasets/{id}/manifests/{jobID}/errors":     h.importErrors,
		"POST /api/v1/datasets/{id}/manifests/{jobID}/submit":    h.submitManifest,
		"POST /api/v1/datasets/{id}/manifests/{jobID}/cancel":    h.cancelImport,
		"POST /api/v1/datasets/{id}/snapshots":                   h.snapshot,
		"GET /api/v1/datasets/{id}/versions":                     h.versions,
		"GET /api/v1/datasets/{id}/versions/{versionID}":         h.version,
		"GET /api/v1/datasets/{id}/versions/{versionID}/members": h.versionMembers,
		"POST /api/v1/datasets/{id}/validations":                 h.validate,
		"GET /api/v1/datasets/{id}/validations":                  h.validations,
		"GET /api/v1/validation-jobs/{jobID}":                    h.validationJob,
		"POST /api/v1/validation-jobs/{jobID}/cancel":            h.cancelValidation,
		"GET /api/v1/validation-reports/{reportID}":              h.report,
		"GET /api/v1/validation-reports/{reportID}/findings":     h.findings,
		"POST /api/v1/datasets/{id}/readiness-assessments":       h.assessReadiness,
		"GET /api/v1/datasets/{id}/readiness-reports":            h.readinessReports,
		"GET /api/v1/datasets/{id}/lineage":                      h.datasetLineage,
		"GET /api/v1/dataset-versions/{versionID}/lineage":       h.versionLineage,
		"GET /api/v1/validation-profiles":                        h.profiles,
		"GET /api/v1/validation-profiles/{profileID}":            h.profile,
		"POST /api/v1/validation-profiles":                       h.createProfile,
	}
}

// ─── Responses ────────────────────────────────────────────────────────

func derivationInfo(d *model.Derivation) *response.DerivationInfo {
	if d == nil {
		return nil
	}
	out := &response.DerivationInfo{VersionIDs: d.SourceVersionIDs, Splits: d.Splits, Labels: d.Labels}
	if t := d.Transformation; t != nil {
		out.Transformation = &response.TransformationInfo{Name: t.Name, Version: t.Version, ConfigSHA256: t.ConfigSHA256}
	}
	return out
}

func managedDatasetResponse(d model.Dataset) response.DatasetResponse {
	out := response.DatasetResponse{
		ID: d.ID, Name: d.Name, CompletionPolicy: string(d.CompletionPolicy), Status: string(d.Status),
		MaxBytes: d.MaxBytes, ReservedBytes: d.ReservedBytes, StoredBytes: d.StoredBytes,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, ReadyAt: d.ReadyAt,
		Description: d.Description, Tags: d.Tags, Metadata: d.Metadata, LifecycleStatus: string(d.Lifecycle),
		Revision: d.Revision, MemberCount: d.MemberCount, CurrentVersionID: d.CurrentVersionID,
		ProfileID: d.ProfileID, ProfileVersion: d.ProfileVersion, LatestReportID: d.LatestReportID,
		Derivation: derivationInfo(d.Derivation), CreatedBy: d.CreatedBy, UpdatedBy: d.UpdatedBy, ArchivedAt: d.ArchivedAt,
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if out.Metadata == nil {
		out.Metadata = map[string]string{}
	}
	return out
}

// writeDataset answers a dataset, with its revision as the ETag clients
// send back in If-Match.
func writeDataset(w nethttp.ResponseWriter, status int, d model.Dataset) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(d.Revision, 10)))
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, managedDatasetResponse(d))
}

func importResponse(imp model.ManifestImport) response.ManifestImportResponse {
	return response.ManifestImportResponse{
		ID: imp.ID, DatasetID: imp.DatasetID, State: string(imp.State), Source: imp.Source,
		DuplicatePolicy: string(imp.DuplicatePolicy), ManifestBytes: imp.ManifestBytes, ManifestSHA256: imp.ManifestSHA256,
		Progress: response.ManifestImportProgress{
			LinesRead: imp.LinesRead, MembersAdded: imp.MembersAdded, MembersUpdated: imp.MembersUpdated,
			MembersSkipped: imp.MembersSkipped, LinesRejected: imp.LinesRejected,
		},
		Attempts: imp.Attempts, CancelRequested: imp.CancelRequested, LastError: imp.LastError,
		CreatedAt: imp.CreatedAt, StartedAt: imp.StartedAt, CompletedAt: imp.CompletedAt,
	}
}

func datasetVersionResponse(v model.DatasetVersion) response.DatasetVersionResponse {
	out := response.DatasetVersionResponse{
		ID: v.ID, DatasetID: v.DatasetID, Version: v.Version, Status: v.Status, ValidationStatus: v.ValidationStatus,
		MemberCount: v.MemberCount, TotalBytes: v.TotalBytes, ManifestSHA256: v.ManifestSHA256, ManifestFormat: v.ManifestFormat,
		ProfileID: v.ProfileID, ProfileVersion: v.ProfileVersion, Config: v.Config, ParentVersionID: v.ParentVersionID,
		Derivation: derivationInfo(v.Derivation), LatestReportID: v.LatestReportID, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt,
	}
	if out.Config == nil {
		out.Config = map[string]any{}
	}
	return out
}

func validationJobResponse(j model.ValidationJob) response.ValidationJobResponse {
	return response.ValidationJobResponse{
		ID: j.ID, DatasetID: j.DatasetID, VersionID: j.DatasetVersionID, ProfileID: j.ProfileID, ProfileVersion: j.ProfileVersion,
		Mode: string(j.Mode), SampleSize: j.SampleSize, Readiness: j.Readiness, State: string(j.State),
		Progress: response.ValidationProgress{
			Total: j.TotalItems, Processed: j.ProcessedItems, Passed: j.PassedItems, Warnings: j.WarningItems, Failed: j.FailedItems,
			ContentExamined: j.ContentExamined, ContentNotExamined: j.ContentSkipped, BytesTotal: j.BytesTotal, BytesRead: j.BytesRead,
		},
		Attempts: j.Attempts, MaxAttempts: j.MaxAttempts, CancelRequested: j.CancelRequested, LastError: j.LastError,
		ReportID: j.ReportID, CreatedBy: j.CreatedBy, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt,
	}
}

func validationReportResponse(r model.ValidationReport, withBody bool) response.ValidationReportResponse {
	out := response.ValidationReportResponse{
		ID: r.ID, JobID: r.JobID, DatasetID: r.DatasetID, VersionID: r.DatasetVersionID, ProfileID: r.ProfileID,
		ProfileVersion: r.ProfileVersion, Mode: string(r.Mode), Status: string(r.Status), Readiness: r.Readiness,
		SchemaVersion: r.SchemaVersion, Summary: r.Summary, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt,
	}
	if withBody {
		out.Report = r.Body
	}
	return out
}

func profileResponse(p model.ValidationProfile) response.ValidationProfileResponse {
	definition, _ := json.Marshal(p.Definition)
	out := response.ValidationProfileResponse{
		ID: p.ID, Version: p.Version, Name: p.Name, Description: p.Description, BuiltIn: p.BuiltIn, Definition: definition,
	}
	if !p.CreatedAt.IsZero() {
		at := p.CreatedAt
		out.CreatedAt = &at
	}
	return out
}

// ─── Datasets ─────────────────────────────────────────────────────────

func (h *DatasetHandler) create(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CreateDatasetRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	in := dataset.CreateInput{
		Name: body.Name, Description: body.Description, Tags: body.Tags, Metadata: body.Metadata,
		ProfileID: body.ProfileID, ProfileVersion: body.ProfileVersion,
		CompletionPolicy: model.CompletionPolicy(strings.ToUpper(body.CompletionPolicy)), MaxBytes: body.MaxBytes,
	}
	if from := body.DerivedFrom; from != nil {
		in.SourceVersionIDs, in.Splits, in.Labels = from.VersionIDs, from.Splits, from.Labels
		if from.VersionIDs == nil {
			in.SourceVersionIDs = []string{}
		}
		if t := from.Transformation; t != nil {
			in.Transformation = &dataset.TransformationInput{Name: t.Name, Version: t.Version, Config: t.Config}
		}
		if len(in.SourceVersionIDs) == 0 {
			h.fail(w, fmt.Errorf("%w: derived_from needs version_ids", dataset.ErrInvalidRequest))
			return
		}
	}
	d, created, err := h.service.Create(r.Context(), principalOf(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	status := nethttp.StatusOK
	if created {
		status = nethttp.StatusCreated
	}
	writeDataset(w, status, d)
}

func (h *DatasetHandler) list(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	limit, err := datasetInt(q.Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	page, err := h.service.List(r.Context(), principalOf(r), q.Get("status"), q.Get("cursor"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.DatasetPageResponse{Datasets: make([]response.DatasetResponse, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, d := range page.Items {
		out.Datasets = append(out.Datasets, managedDatasetResponse(d))
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) get(w nethttp.ResponseWriter, r *nethttp.Request) {
	d, err := h.service.Get(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeDataset(w, nethttp.StatusOK, d)
}

func (h *DatasetHandler) update(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.UpdateDatasetRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	revision, err := revisionOf(r, body.Revision)
	if err != nil {
		h.fail(w, err)
		return
	}
	d, err := h.service.Update(r.Context(), principalOf(r), r.PathValue("id"), revision, dataset.UpdateInput{
		Description: body.Description, Tags: body.Tags, Metadata: body.Metadata,
		ProfileID: body.ProfileID, ProfileVersion: body.ProfileVersion,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeDataset(w, nethttp.StatusOK, d)
}

func (h *DatasetHandler) archive(w nethttp.ResponseWriter, r *nethttp.Request) {
	d, err := h.service.Archive(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeDataset(w, nethttp.StatusOK, d)
}

// ─── Members ──────────────────────────────────────────────────────────

func (h *DatasetHandler) members(w nethttp.ResponseWriter, r *nethttp.Request) {
	limit, err := datasetInt(r.URL.Query().Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	page, err := h.service.Members(r.Context(), principalOf(r), r.PathValue("id"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.DatasetMemberPageResponse{Members: make([]response.DatasetMemberResponse, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, m := range page.Items {
		metadata := m.Metadata
		if metadata == nil {
			metadata = map[string]string{}
		}
		out.Members = append(out.Members, response.DatasetMemberResponse{
			ID: m.ID, ObjectID: m.ObjectID, Namespace: m.Namespace, Path: m.Path, VersionID: m.AssetID, Label: m.Label,
			Split: m.Split, Metadata: metadata, Source: m.Source, AddedBy: m.AddedBy, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) addMembers(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.AddMembersRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	inputs := make([]dataset.MemberInput, len(body.Members))
	for i, m := range body.Members {
		inputs[i] = dataset.MemberInput{ObjectID: m.ObjectID, Namespace: m.Namespace, Path: m.Path, VersionID: m.VersionID,
			Version: m.Version, Label: m.Label, Split: m.Split, Metadata: m.Metadata}
	}
	result, err := h.service.AddMembers(r.Context(), principalOf(r), r.PathValue("id"), inputs,
		model.DuplicatePolicy(strings.ToUpper(body.OnDuplicate)))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.AddMembersResponse{Added: result.Added, Updated: result.Updated, Skipped: result.Skipped,
		Rejected: result.Rejected, Dataset: managedDatasetResponse(result.Dataset)}
	for _, o := range result.Outcomes {
		out.Results = append(out.Results, response.MemberResultResponse{Index: o.Index, Outcome: o.Outcome,
			ObjectID: o.ObjectID, VersionID: o.AssetID, Code: o.Code, Error: o.Error})
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) removeMember(w nethttp.ResponseWriter, r *nethttp.Request) {
	d, err := h.service.RemoveMember(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("memberID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeDataset(w, nethttp.StatusOK, d)
}

// ─── Manifests ────────────────────────────────────────────────────────

// importManifest starts an import. A JSON Lines body (application/x-ndjson
// or application/jsonl) is the manifest itself; a JSON body asks for a
// staged upload of a larger one. Idempotency-Key is required either way.
func (h *DatasetHandler) importManifest(w nethttp.ResponseWriter, r *nethttp.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	p, datasetID := principalOf(r), r.PathValue("id")
	switch mediaType {
	case "application/x-ndjson", "application/jsonl", "application/jsonlines":
		in := dataset.ImportInput{
			IdempotencyKey:  r.Header.Get("Idempotency-Key"),
			DuplicatePolicy: model.DuplicatePolicy(strings.ToUpper(r.URL.Query().Get("duplicate_policy"))),
			SHA256:          r.URL.Query().Get("sha256"),
		}
		defer r.Body.Close()
		imp, created, err := h.service.ImportInline(r.Context(), p, datasetID, in, r.Body)
		if err != nil {
			h.fail(w, err)
			return
		}
		status := nethttp.StatusOK
		if created {
			status = nethttp.StatusAccepted
		}
		writeJSON(w, status, importResponse(imp))
	case "application/json":
		var body request.StageDatasetManifestRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		key := body.IdempotencyKey
		if key == "" {
			key = r.Header.Get("Idempotency-Key")
		}
		imp, upload, created, err := h.service.StageImport(r.Context(), p, datasetID, dataset.ImportInput{
			IdempotencyKey: key, DuplicatePolicy: model.DuplicatePolicy(strings.ToUpper(body.DuplicatePolicy)),
			SHA256: body.SHA256, SizeBytes: body.SizeBytes,
		})
		if err != nil {
			h.fail(w, err)
			return
		}
		out := importResponse(imp)
		if upload.URL != "" {
			out.UploadURL = upload.URL
			out.UploadExpiresAt = &upload.ExpiresAt
		}
		w.Header().Set("Cache-Control", "no-store")
		status := nethttp.StatusOK
		if created {
			status = nethttp.StatusCreated
		}
		writeJSON(w, status, out)
	default:
		writeJSON(w, nethttp.StatusUnsupportedMediaType, problem{
			Error: "send the manifest as application/x-ndjson, or application/json to stage a large one", Code: "unsupported_media_type"})
	}
}

func (h *DatasetHandler) submitManifest(w nethttp.ResponseWriter, r *nethttp.Request) {
	imp, err := h.service.SubmitImport(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("jobID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, importResponse(imp))
}

func (h *DatasetHandler) manifestImport(w nethttp.ResponseWriter, r *nethttp.Request) {
	imp, err := h.service.Import(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("jobID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, importResponse(imp))
}

func (h *DatasetHandler) importErrors(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	after, err := datasetInt(q.Get("after"))
	if err != nil {
		h.fail(w, err)
		return
	}
	limit, err := datasetInt(q.Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	errs, err := h.service.ImportErrors(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("jobID"), int64(after), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.ImportErrorPageResponse{Errors: make([]response.ImportErrorResponse, 0, len(errs))}
	for _, e := range errs {
		out.Errors = append(out.Errors, response.ImportErrorResponse{Line: e.Line, Code: e.Code, Detail: e.Detail})
	}
	if len(errs) > 0 && (limit == 0 || len(errs) == limit) {
		out.NextAfter = errs[len(errs)-1].Line
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) cancelImport(w nethttp.ResponseWriter, r *nethttp.Request) {
	imp, err := h.service.CancelImport(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("jobID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	status := nethttp.StatusOK
	if !imp.State.Terminal() {
		status = nethttp.StatusAccepted // the worker stops after its batch
	}
	writeJSON(w, status, importResponse(imp))
}

// ─── Versions ─────────────────────────────────────────────────────────

func (h *DatasetHandler) snapshot(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.SnapshotRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	key := body.IdempotencyKey
	if key == "" {
		key = r.Header.Get("Idempotency-Key")
	}
	v, created, err := h.service.Snapshot(r.Context(), principalOf(r), r.PathValue("id"), key)
	if err != nil {
		h.fail(w, err)
		return
	}
	status := nethttp.StatusOK
	if created {
		status = nethttp.StatusCreated
	}
	writeJSON(w, status, datasetVersionResponse(v))
}

func (h *DatasetHandler) versions(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	before, err := datasetInt(q.Get("before"))
	if err != nil {
		h.fail(w, err)
		return
	}
	limit, err := datasetInt(q.Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	versions, err := h.service.Versions(r.Context(), principalOf(r), r.PathValue("id"), before, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.DatasetVersionPageResponse{Versions: make([]response.DatasetVersionResponse, 0, len(versions))}
	for _, v := range versions {
		out.Versions = append(out.Versions, datasetVersionResponse(v))
	}
	if n := len(versions); n > 0 && versions[n-1].Version > 1 && (limit == 0 || n == limit) {
		out.NextBefore = versions[n-1].Version
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) version(w nethttp.ResponseWriter, r *nethttp.Request) {
	v, err := h.service.Version(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, datasetVersionResponse(v))
}

func (h *DatasetHandler) versionMembers(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	after, err := datasetInt(q.Get("after"))
	if err != nil {
		h.fail(w, err)
		return
	}
	limit, err := datasetInt(q.Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	members, err := h.service.VersionMembers(r.Context(), principalOf(r), r.PathValue("id"), r.PathValue("versionID"), int64(after), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.VersionMemberPageResponse{Members: make([]response.VersionMemberResponse, 0, len(members))}
	for _, m := range members {
		metadata := m.Metadata
		if metadata == nil {
			metadata = map[string]string{}
		}
		out.Members = append(out.Members, response.VersionMemberResponse{
			Ordinal: m.Ordinal, ObjectID: m.ObjectID, VersionID: m.AssetID, ObjectVersion: m.ObjectVersion, Path: m.Path,
			Label: m.Label, Split: m.Split, SizeBytes: m.SizeBytes, ContentType: m.ContentType, ChecksumSHA256: m.ChecksumSHA256,
			Metadata: metadata,
		})
	}
	if n := len(members); n > 0 && (limit == 0 || n == limit) {
		out.NextAfter = members[n-1].Ordinal
	}
	writeJSON(w, nethttp.StatusOK, out)
}

// ─── Validation ───────────────────────────────────────────────────────

func validationInput(r *nethttp.Request, body request.ValidationRequest) dataset.ValidationInput {
	key := body.IdempotencyKey
	if key == "" {
		key = r.Header.Get("Idempotency-Key")
	}
	return dataset.ValidationInput{
		VersionID: body.VersionID, ProfileID: body.ProfileID, ProfileVersion: body.ProfileVersion,
		Mode: model.ValidationMode(strings.ToUpper(body.Mode)), SampleSize: body.SampleSize, IdempotencyKey: key,
	}
}

func (h *DatasetHandler) startJob(w nethttp.ResponseWriter, r *nethttp.Request, readiness bool) {
	var body request.ValidationRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	start := h.service.StartValidation
	if readiness {
		start = h.service.StartReadiness
	}
	job, created, err := start(r.Context(), principalOf(r), r.PathValue("id"), validationInput(r, body))
	if err != nil {
		h.fail(w, err)
		return
	}
	status := nethttp.StatusOK
	if created {
		status = nethttp.StatusAccepted
		w.Header().Set("Location", "/api/v1/validation-jobs/"+job.ID)
	}
	writeJSON(w, status, validationJobResponse(job))
}

func (h *DatasetHandler) validate(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.startJob(w, r, false)
}

func (h *DatasetHandler) assessReadiness(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.startJob(w, r, true)
}

func (h *DatasetHandler) validations(w nethttp.ResponseWriter, r *nethttp.Request) {
	limit, err := datasetInt(r.URL.Query().Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	page, err := h.service.Validations(r.Context(), principalOf(r), r.PathValue("id"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.ValidationJobPageResponse{Jobs: make([]response.ValidationJobResponse, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, j := range page.Items {
		out.Jobs = append(out.Jobs, validationJobResponse(j))
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) validationJob(w nethttp.ResponseWriter, r *nethttp.Request) {
	job, err := h.service.Validation(r.Context(), principalOf(r), r.PathValue("jobID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, validationJobResponse(job))
}

func (h *DatasetHandler) cancelValidation(w nethttp.ResponseWriter, r *nethttp.Request) {
	job, err := h.service.CancelValidation(r.Context(), principalOf(r), r.PathValue("jobID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	status := nethttp.StatusOK
	if !job.State.Terminal() {
		status = nethttp.StatusAccepted
	}
	writeJSON(w, status, validationJobResponse(job))
}

func (h *DatasetHandler) report(w nethttp.ResponseWriter, r *nethttp.Request) {
	rep, err := h.service.Report(r.Context(), principalOf(r), r.PathValue("reportID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, validationReportResponse(rep, true))
}

func (h *DatasetHandler) findings(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	limit, err := datasetInt(q.Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	page, err := h.service.Findings(r.Context(), principalOf(r), r.PathValue("reportID"), q.Get("severity"), q.Get("rule_id"), q.Get("cursor"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.FindingPageResponse{Findings: make([]response.FindingResponse, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, f := range page.Items {
		out.Findings = append(out.Findings, response.FindingResponse{
			ID: f.ID, RuleID: f.RuleID, RuleVersion: f.RuleVersion, Severity: string(f.Severity), Code: f.Code,
			ObjectID: f.ObjectID, VersionID: f.AssetID, Message: f.Message, Expected: f.Expected, Observed: f.Observed,
			Evidence: f.Evidence, Remediation: f.Remediation, CreatedAt: f.CreatedAt,
		})
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) readinessReports(w nethttp.ResponseWriter, r *nethttp.Request) {
	limit, err := datasetInt(r.URL.Query().Get("limit"))
	if err != nil {
		h.fail(w, err)
		return
	}
	page, err := h.service.Reports(r.Context(), principalOf(r), r.PathValue("id"), r.URL.Query().Get("all") != "true",
		r.URL.Query().Get("cursor"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.ValidationReportPageResponse{Reports: make([]response.ValidationReportResponse, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, rep := range page.Items {
		out.Reports = append(out.Reports, validationReportResponse(rep, false))
	}
	writeJSON(w, nethttp.StatusOK, out)
}

// ─── Lineage ──────────────────────────────────────────────────────────

func lineageRequest(r *nethttp.Request) (dataset.LineageRequest, error) {
	q := r.URL.Query()
	depth, err := datasetInt(q.Get("depth"))
	if err != nil {
		return dataset.LineageRequest{}, err
	}
	limit, err := datasetInt(q.Get("limit"))
	if err != nil {
		return dataset.LineageRequest{}, err
	}
	return dataset.LineageRequest{Direction: q.Get("direction"), Depth: depth, Cursor: q.Get("cursor"), Limit: limit}, nil
}

func lineageResponse(start model.LineageRef, direction string, page model.LineagePage) response.LineageResponse {
	if direction == "" {
		direction = "upstream"
	}
	out := response.LineageResponse{Start: response.LineageRefResponse{Kind: string(start.Kind), ID: start.RefID}, Direction: direction,
		Edges: make([]response.LineageEdgeResponse, 0, len(page.Edges)), NextCursor: page.After, Truncated: page.Truncated}
	for _, e := range page.Edges {
		out.Edges = append(out.Edges, response.LineageEdgeResponse{
			From: response.LineageRefResponse{Kind: string(e.From.Kind), ID: e.From.RefID},
			To:   response.LineageRefResponse{Kind: string(e.To.Kind), ID: e.To.RefID}, Relation: string(e.Relation),
			Depth: e.Depth, Actor: e.Actor, JobID: e.JobID, Details: e.Details, CreatedAt: e.CreatedAt,
		})
	}
	return out
}

func (h *DatasetHandler) datasetLineage(w nethttp.ResponseWriter, r *nethttp.Request) {
	req, err := lineageRequest(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	id := r.PathValue("id")
	page, err := h.service.DatasetLineage(r.Context(), principalOf(r), id, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, lineageResponse(model.LineageRef{Kind: model.NodeDataset, RefID: id}, req.Direction, page))
}

func (h *DatasetHandler) versionLineage(w nethttp.ResponseWriter, r *nethttp.Request) {
	req, err := lineageRequest(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	id := r.PathValue("versionID")
	page, err := h.service.VersionLineage(r.Context(), principalOf(r), id, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, lineageResponse(model.LineageRef{Kind: model.NodeDatasetVersion, RefID: id}, req.Direction, page))
}

// ─── Profiles ─────────────────────────────────────────────────────────

func (h *DatasetHandler) profiles(w nethttp.ResponseWriter, r *nethttp.Request) {
	profiles, err := h.service.Profiles(r.Context(), principalOf(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := response.ValidationProfileListResponse{RuleTypes: dataset.RuleTypes()}
	for _, p := range profiles {
		out.Profiles = append(out.Profiles, profileResponse(p))
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *DatasetHandler) profile(w nethttp.ResponseWriter, r *nethttp.Request) {
	version, err := datasetInt(r.URL.Query().Get("version"))
	if err != nil {
		h.fail(w, err)
		return
	}
	p, err := h.service.Profile(r.Context(), principalOf(r), r.PathValue("profileID"), version)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, profileResponse(p))
}

func (h *DatasetHandler) createProfile(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.ValidationProfileRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	def := model.ProfileDefinition{Kind: body.Kind, Thresholds: model.Thresholds{MaxErrors: body.Thresholds.MaxErrors, MaxWarnings: body.Thresholds.MaxWarnings}}
	for _, rule := range body.Rules {
		enabled := rule.Enabled == nil || *rule.Enabled
		def.Rules = append(def.Rules, model.Rule{ID: rule.ID, Version: rule.Version, Type: rule.Type,
			Severity: model.Severity(strings.ToUpper(rule.Severity)), Enabled: enabled, Scope: model.RuleScope(strings.ToUpper(rule.Scope)),
			Config: rule.Config})
	}
	p, err := h.service.CreateProfile(r.Context(), principalOf(r), dataset.ProfileInput{Name: body.Name, Description: body.Description, Definition: def})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, profileResponse(p))
}

// ─── Errors ───────────────────────────────────────────────────────────

func datasetInt(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a number", dataset.ErrInvalidRequest, raw)
	}
	return n, nil
}

// fail maps service errors to responses with a stable code. Messages
// never carry storage keys, URLs or object content; anything unexpected
// is logged sanitized and answered generically.
func (h *DatasetHandler) fail(w nethttp.ResponseWriter, err error) {
	status, code := 0, ""
	switch {
	case errors.Is(err, dataset.ErrInvalidRequest), errors.Is(err, catalog.ErrInvalidRequest), errors.Is(err, model.ErrInvalidCursor),
		errors.Is(err, model.ErrInvalidProfile), errors.Is(err, model.ErrInvalidLineage):
		status, code = nethttp.StatusBadRequest, "invalid_request"
	case errors.Is(err, dataset.ErrRevisionRequired):
		status, code = nethttp.StatusPreconditionRequired, "revision_required"
	case errors.Is(err, model.ErrDatasetNotFound), errors.Is(err, model.ErrMemberNotFound), errors.Is(err, model.ErrDatasetVersionNotFound),
		errors.Is(err, model.ErrImportNotFound), errors.Is(err, model.ErrProfileNotFound), errors.Is(err, model.ErrValidationNotFound),
		errors.Is(err, model.ErrReportNotFound):
		status, code = nethttp.StatusNotFound, "not_found"
	case errors.Is(err, model.ErrDatasetArchived):
		status, code = nethttp.StatusConflict, "dataset_archived"
	case errors.Is(err, model.ErrDatasetTransition):
		status, code = nethttp.StatusConflict, "invalid_transition"
	case errors.Is(err, model.ErrNoMembers):
		status, code = nethttp.StatusConflict, "no_members"
	case errors.Is(err, model.ErrUnresolvedMembers):
		status, code = nethttp.StatusConflict, "unresolved_members"
	case errors.Is(err, dataset.ErrNoVersion):
		status, code = nethttp.StatusConflict, "no_version"
	case errors.Is(err, model.ErrDatasetConflict), errors.Is(err, model.ErrImportConflict), errors.Is(err, model.ErrValidationConflict):
		status, code = nethttp.StatusConflict, "idempotency_conflict"
	case errors.Is(err, model.ErrImportState), errors.Is(err, model.ErrValidationState):
		status, code = nethttp.StatusConflict, "invalid_state"
	case errors.Is(err, dataset.ErrManifestNotStaged):
		status, code = nethttp.StatusConflict, "manifest_not_uploaded"
	case errors.Is(err, model.ErrLineageCycle):
		status, code = nethttp.StatusConflict, "lineage_cycle"
	case errors.Is(err, dataset.ErrManifestTooLarge):
		status, code = nethttp.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, model.ErrSnapshotTooLarge):
		status, code = nethttp.StatusUnprocessableEntity, "snapshot_too_large"
	case errors.Is(err, dataset.ErrManifestChecksum):
		status, code = nethttp.StatusUnprocessableEntity, "checksum_mismatch"
	case errors.Is(err, model.ErrTooManyJobs):
		w.Header().Set("Retry-After", strconv.Itoa(int(time.Minute/time.Second)))
		status, code = nethttp.StatusTooManyRequests, "too_many_jobs"
	}
	if status == 0 {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, status, problem{Error: err.Error(), Code: code})
}
