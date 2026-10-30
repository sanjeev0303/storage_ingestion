package http

import (
	"context"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/events"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
	"github.com/sanjeev0303/object-storage-platform/internal/service/processing"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
)

// AdminHandler serves platform administration. Every route needs a
// platform role, which the services check.
type AdminHandler struct {
	identity    *identity.Service
	quotas      *quota.Service
	audit       *audit.Service
	deadLetters *events.GuardedAdmin
	log         *slog.Logger
}

func NewAdminHandler(identitySvc *identity.Service, quotas *quota.Service, auditSvc *audit.Service, deadLetters *events.GuardedAdmin, log *slog.Logger) *AdminHandler {
	return &AdminHandler{identity: identitySvc, quotas: quotas, audit: auditSvc, deadLetters: deadLetters, log: log}
}

func (h *AdminHandler) Routes() map[string]nethttp.HandlerFunc {
	routes := map[string]nethttp.HandlerFunc{
		"GET /api/v1/admin/users":                               h.listUsers,
		"POST /api/v1/admin/users":                              h.createUser,
		"POST /api/v1/admin/users/{id}/deactivate":              h.deactivateUser,
		"POST /api/v1/admin/users/{id}/reactivate":              h.reactivateUser,
		"PUT /api/v1/admin/users/{id}/platform-role":            h.setPlatformRole,
		"POST /api/v1/admin/users/{id}/password-reset":          h.issueReset,
		"GET /api/v1/admin/organizations":                       h.listOrganizations,
		"POST /api/v1/admin/organizations":                      h.createOrganization,
		"PUT /api/v1/admin/organizations/{id}/status":           h.setOrganizationStatus,
		"DELETE /api/v1/admin/organizations/{id}":               h.deleteOrganization,
		"GET /api/v1/admin/organizations/{id}/quota":            h.getQuota,
		"PUT /api/v1/admin/organizations/{id}/quota":            h.setQuota,
		"POST /api/v1/admin/organizations/{id}/quota/reconcile": h.reconcileQuota,
		"GET /api/v1/admin/audit":                               h.listAudit,
		"GET /api/v1/admin/audit/verify":                        h.verifyAudit,
	}
	if h.deadLetters != nil {
		routes["GET /api/v1/admin/dead-letters"] = h.listDeadLetters
		routes["GET /api/v1/admin/dead-letters/{id}"] = h.inspectDeadLetter
		routes["POST /api/v1/admin/dead-letters/{id}/retry"] = h.retryDeadLetter
		routes["POST /api/v1/admin/dead-letters/{id}/replay"] = h.replayDeadLetter
		routes["POST /api/v1/admin/dead-letters/{id}/resolve"] = h.resolveDeadLetter
		routes["POST /api/v1/admin/dead-letters/{id}/discard"] = h.discardDeadLetter
	}
	return routes
}

func credentialResponse(t identity.CredentialToken) response.CredentialTokenResponse {
	return response.CredentialTokenResponse{UserID: t.UserID, Token: t.Token, ExpiresAt: t.ExpiresAt}
}

func (h *AdminHandler) listUsers(w nethttp.ResponseWriter, r *nethttp.Request) {
	users, err := h.identity.ListUsers(r.Context(), principalOf(r), r.URL.Query().Get("after"), queryInt(r, "limit"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := make([]response.UserResponse, 0, len(users))
	for _, u := range users {
		out = append(out, userResponse(u))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"users": out})
}

func (h *AdminHandler) createUser(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CreateUserRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	user, token, err := h.identity.CreateUser(r.Context(), principalOf(r), body.Email, body.DisplayName,
		model.PlatformRole(strings.ToUpper(body.PlatformRole)))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusCreated, response.CreatedUserResponse{User: userResponse(user), Activation: credentialResponse(token)})
}

func (h *AdminHandler) deactivateUser(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.setUserStatus(w, r, false)
}

func (h *AdminHandler) reactivateUser(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.setUserStatus(w, r, true)
}

func (h *AdminHandler) setUserStatus(w nethttp.ResponseWriter, r *nethttp.Request, active bool) {
	user, err := h.identity.SetUserStatus(r.Context(), principalOf(r), r.PathValue("id"), active)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, userResponse(user))
}

func (h *AdminHandler) setPlatformRole(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.PlatformRoleRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	err := h.identity.SetPlatformRole(r.Context(), principalOf(r), r.PathValue("id"), model.PlatformRole(strings.ToUpper(body.PlatformRole)))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *AdminHandler) issueReset(w nethttp.ResponseWriter, r *nethttp.Request) {
	token, err := h.identity.IssueReset(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusCreated, credentialResponse(token))
}

func (h *AdminHandler) listOrganizations(w nethttp.ResponseWriter, r *nethttp.Request) {
	orgs, err := h.identity.ListOrganizations(r.Context(), principalOf(r), r.URL.Query().Get("after"), queryInt(r, "limit"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := make([]response.OrganizationResponse, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, orgResponse(o))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"organizations": out})
}

func (h *AdminHandler) createOrganization(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.CreateOrganizationRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	org, err := h.identity.CreateOrganization(r.Context(), principalOf(r), body.Name, body.Slug, body.OwnerUserID)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, orgResponse(org))
}

func (h *AdminHandler) setOrganizationStatus(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.OrganizationStatusRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	org, err := h.identity.SetOrganizationStatus(r.Context(), principalOf(r), r.PathValue("id"),
		model.OrgStatus(strings.ToUpper(body.Status)))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, orgResponse(org))
}

func (h *AdminHandler) deleteOrganization(w nethttp.ResponseWriter, r *nethttp.Request) {
	org, err := h.identity.DeleteOrganization(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, orgResponse(org))
}

func (h *AdminHandler) getQuota(w nethttp.ResponseWriter, r *nethttp.Request) {
	report, err := h.quotas.GetFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, quotaResponse(report))
}

func (h *AdminHandler) setQuota(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.QuotaRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	_, err := h.quotas.Set(r.Context(), principalOf(r), r.PathValue("id"), model.TenantQuota{
		MaxStorageBytes: body.MaxStorageBytes, MaxAssets: body.MaxAssets, MaxObjectBytes: body.MaxObjectBytes,
		MaxActiveUploads: body.MaxActiveUploads, MaxActiveMultipart: body.MaxActiveMultipart, MaxActiveJobs: body.MaxActiveJobs,
		MaxPendingProcessing: body.MaxPendingProcessing, MaxPendingEntries: body.MaxPendingEntries,
		MaxManifestEntries: body.MaxManifestEntries, MaxManifestBytes: body.MaxManifestBytes,
		MaxRequestsPerMinute: body.MaxRequestsPerMinute,
	})
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	h.getQuota(w, r)
}

func (h *AdminHandler) reconcileQuota(w nethttp.ResponseWriter, r *nethttp.Request) {
	result, err := h.quotas.AdminReconcile(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{
		"tenant_id": result.TenantID, "settled_reservations": result.Settled, "repaired": result.Repaired,
		"reserved_bytes_before": result.Before.ReservedBytes, "reserved_bytes_after": result.After.ReservedBytes,
		"stored_bytes_before": result.Before.StoredBytes, "stored_bytes_after": result.After.StoredBytes,
	})
}

func (h *AdminHandler) listAudit(w nethttp.ResponseWriter, r *nethttp.Request) {
	f := auditFilter(r)
	chain := r.URL.Query().Get("chain")
	if chain == "" {
		chain = model.PlatformChain
	}
	events, err := h.audit.PlatformList(r.Context(), principalOf(r), chain, f)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, auditPage(events, f.Limit))
}

func (h *AdminHandler) verifyAudit(w nethttp.ResponseWriter, r *nethttp.Request) {
	chain := r.URL.Query().Get("chain")
	if chain == "" {
		chain = model.PlatformChain
	}
	v, err := h.audit.Verify(r.Context(), principalOf(r), chain)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, verificationResponse(v))
}

func deadLetterResponse(dl model.DeadLetter) response.DeadLetterResponse {
	return response.DeadLetterResponse{
		ID: dl.ID, EventID: dl.EventID, EventType: dl.EventType, TenantID: dl.TenantID, Consumer: dl.Consumer,
		Status: dl.Status, Category: dl.FailureCategory, LastError: dl.LastError, CreatedAt: dl.DeadLetteredAt,
	}
}

func (h *AdminHandler) listDeadLetters(w nethttp.ResponseWriter, r *nethttp.Request) {
	letters, err := h.deadLetters.List(r.Context(), principalOf(r), r.URL.Query().Get("status"), queryInt(r, "limit"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := make([]response.DeadLetterResponse, 0, len(letters))
	for _, dl := range letters {
		out = append(out, deadLetterResponse(dl))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"dead_letters": out})
}

func (h *AdminHandler) inspectDeadLetter(w nethttp.ResponseWriter, r *nethttp.Request) {
	detail, err := h.deadLetters.Inspect(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := deadLetterResponse(detail.DeadLetter)
	out.Stale = detail.Stale
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *AdminHandler) retryDeadLetter(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.ReasonRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := h.deadLetters.Retry(r.Context(), principalOf(r), r.PathValue("id"), body.Reason); err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *AdminHandler) replayDeadLetter(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.ReasonRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	replayID, err := h.deadLetters.Replay(r.Context(), principalOf(r), r.PathValue("id"), body.Reason)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]string{"replay_event_id": replayID})
}

func (h *AdminHandler) resolveDeadLetter(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.closeDeadLetter(w, r, h.deadLetters.Resolve)
}

func (h *AdminHandler) discardDeadLetter(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.closeDeadLetter(w, r, h.deadLetters.Discard)
}

func (h *AdminHandler) closeDeadLetter(w nethttp.ResponseWriter, r *nethttp.Request,
	fn func(ctx context.Context, p model.Principal, id, reason string) error) {
	var body request.ReasonRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := fn(r.Context(), principalOf(r), r.PathValue("id"), body.Reason); err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

// ─── Tenant quarantine and retention ──────────────────────────────────

// GovernanceHandler serves a tenant's quarantine records, retention
// policies and legal holds.
type GovernanceHandler struct {
	quarantine *processing.Quarantine
	lifecycle  *asset.Service
	log        *slog.Logger
}

func NewGovernanceHandler(quarantine *processing.Quarantine, lifecycle *asset.Service, log *slog.Logger) *GovernanceHandler {
	return &GovernanceHandler{quarantine: quarantine, lifecycle: lifecycle, log: log}
}

func (h *GovernanceHandler) Routes() map[string]nethttp.HandlerFunc {
	routes := map[string]nethttp.HandlerFunc{}
	if h.quarantine != nil {
		routes["GET /api/v1/assets/{id}/quarantine"] = h.inspect
		routes["POST /api/v1/assets/{id}/quarantine/release"] = h.release
		routes["GET /api/v1/assets/{id}/findings"] = h.findings
		routes["GET /api/v1/assets/{id}/blast-radius"] = h.blastRadius
		routes["GET /api/v1/content-policy"] = h.contentPolicy
		routes["PUT /api/v1/content-policy"] = h.setContentPolicy
	}
	if h.lifecycle != nil {
		routes["PUT /api/v1/retention/policies"] = h.setPolicy
		routes["GET /api/v1/retention/policies"] = h.listPolicies
		routes["POST /api/v1/legal-holds"] = h.placeHold
		routes["GET /api/v1/legal-holds"] = h.listHolds
		routes["DELETE /api/v1/legal-holds/{id}"] = h.releaseHold
		h.governanceRoutes(routes)
	}
	return routes
}

func (h *GovernanceHandler) inspect(w nethttp.ResponseWriter, r *nethttp.Request) {
	inspection, err := h.quarantine.InspectFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := response.ProcessingRecordResponse{
		AssetID: inspection.Asset.ID, AssetStatus: string(inspection.Asset.Status), JobState: string(inspection.Job.State),
		JobStage: string(inspection.Job.Stage), Attempts: inspection.Job.AttemptCount, LastError: inspection.Job.LastError,
		Quarantine: []map[string]any{},
	}
	for _, q := range inspection.Quarantine {
		out.Quarantine = append(out.Quarantine, map[string]any{
			"id": q.ID, "category": q.Category, "reason": q.Reason, "scan_result": q.ScanResult,
			"status": q.Status, "created_at": q.CreatedAt, "retain_until": q.RetainUntil,
		})
	}
	if job := inspection.Job; job.ScanResult != "" {
		out.Scan = &response.ScanResponse{
			Scanner: job.Scanner, Result: job.ScanResult, ScannerVersion: job.ScannerVersion,
			ReusedFrom: job.ScanReusedFrom, ScannedAt: job.ScannedAt,
		}
	}
	out.Events = []map[string]any{}
	for _, e := range inspection.Events {
		out.Events = append(out.Events, map[string]any{"event": e.Event, "detail": e.Detail, "actor": e.Actor, "at": e.CreatedAt})
	}
	out.Findings = append([]model.ContentFinding{}, inspection.Findings...)
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *GovernanceHandler) findings(w nethttp.ResponseWriter, r *nethttp.Request) {
	findings, err := h.quarantine.FindingsFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"findings": append([]model.ContentFinding{}, findings...)})
}

func (h *GovernanceHandler) blastRadius(w nethttp.ResponseWriter, r *nethttp.Request) {
	out, err := h.quarantine.BlastRadiusFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out.Assets = append([]model.BlastAsset{}, out.Assets...)
	out.Snapshots = append([]model.BlastSnapshot{}, out.Snapshots...)
	out.Downloads = append([]model.BlastDownload{}, out.Downloads...)
	writeJSON(w, nethttp.StatusOK, out)
}

// contentPolicyBody is a tenant's content policy on the wire.
type contentPolicyBody struct {
	AllowedTypes      []string          `json:"allowed_types"`
	DeniedTypes       []string          `json:"denied_types"`
	MaxSizeBytes      int64             `json:"max_size_bytes"`
	UnknownTypeAction string            `json:"unknown_type_action"`
	FindingActions    map[string]string `json:"finding_actions"`
}

func policyBody(p model.ContentPolicy) contentPolicyBody {
	body := contentPolicyBody{AllowedTypes: append([]string{}, p.AllowedTypes...), DeniedTypes: append([]string{}, p.DeniedTypes...),
		MaxSizeBytes: p.MaxSizeBytes, UnknownTypeAction: p.UnknownTypeAction, FindingActions: p.FindingActions}
	if body.FindingActions == nil {
		body.FindingActions = map[string]string{}
	}
	return body
}

func (h *GovernanceHandler) contentPolicy(w nethttp.ResponseWriter, r *nethttp.Request) {
	policy, err := h.quarantine.ContentPolicyFor(r.Context(), principalOf(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"policy": policyBody(policy), "finding_families": model.FindingFamilies})
}

func (h *GovernanceHandler) setContentPolicy(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body contentPolicyBody
	if !decodeJSON(w, r, &body) {
		return
	}
	policy, err := h.quarantine.SetContentPolicyFor(r.Context(), principalOf(r), model.ContentPolicy{
		AllowedTypes: body.AllowedTypes, DeniedTypes: body.DeniedTypes, MaxSizeBytes: body.MaxSizeBytes,
		UnknownTypeAction: body.UnknownTypeAction, FindingActions: body.FindingActions,
	})
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"policy": policyBody(policy)})
}

func (h *GovernanceHandler) release(w nethttp.ResponseWriter, r *nethttp.Request) {
	job, err := h.quarantine.RequeueFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusAccepted, map[string]any{"asset_id": job.AssetID, "job_state": job.State, "stage": job.Stage})
}

func (h *GovernanceHandler) setPolicy(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.RetentionPolicyRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	policy := model.RetentionPolicy{Scope: strings.ToUpper(body.Scope), ScopeKey: body.ScopeKey, Reason: body.Reason,
		LockMode: strings.ToUpper(body.LockMode), RetainFromKey: body.RetainFromKey, Template: body.Template}
	for _, d := range []struct {
		value  string
		target **time.Duration
	}{
		{body.MinRetention, &policy.MinRetention}, {body.MaxAge, &policy.MaxAge},
		{body.DeletionGrace, &policy.DeletionGrace}, {body.QuarantineRetention, &policy.QuarantineRetention},
	} {
		if d.value == "" {
			continue
		}
		parsed, err := time.ParseDuration(d.value)
		if err != nil || parsed < 0 {
			writeJSON(w, nethttp.StatusBadRequest, problem{Error: "durations must be like 720h", Code: "invalid_request"})
			return
		}
		*d.target = &parsed
	}
	stored, impact, err := h.lifecycle.SetPolicyGoverned(r.Context(), principalOf(r), asset.PolicyInput{
		Policy: policy, ConfirmCompliance: body.ConfirmCompliance, DryRun: body.DryRun})
	if body.DryRun && (err == nil || errors.Is(err, model.ErrComplianceLocked)) {
		writeJSON(w, nethttp.StatusOK, map[string]any{"dry_run": true, "impact": impact})
		return
	}
	if writeApproval(w, err) {
		return
	}
	if err != nil {
		h.governanceError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"id": stored.ID, "scope": stored.Scope, "scope_key": stored.ScopeKey,
		"version": stored.Version, "effective_from": stored.EffectiveFrom, "lock_mode": stored.LockMode,
		"retain_from_key": stored.RetainFromKey, "template": stored.Template, "impact": impact})
}

// listPolicies answers the newest version of every policy, or with
// ?scope= (and scope_key=) every version of that one, newest first.
func (h *GovernanceHandler) listPolicies(w nethttp.ResponseWriter, r *nethttp.Request) {
	scope := strings.ToUpper(r.URL.Query().Get("scope"))
	var policies []model.RetentionPolicy
	var err error
	if scope == "" {
		policies, err = h.lifecycle.PoliciesFor(r.Context(), principalOf(r))
	} else {
		policies, err = h.lifecycle.PolicyHistoryFor(r.Context(), principalOf(r), scope, r.URL.Query().Get("scope_key"))
	}
	if err != nil {
		h.governanceError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(policies))
	for _, p := range policies {
		out = append(out, map[string]any{
			"id": p.ID, "scope": p.Scope, "scope_key": p.ScopeKey, "version": p.Version,
			"min_retention": durationText(p.MinRetention), "max_age": durationText(p.MaxAge),
			"deletion_grace": durationText(p.DeletionGrace), "quarantine_retention": durationText(p.QuarantineRetention),
			"effective_from": p.EffectiveFrom, "created_by": p.CreatedBy, "reason": p.Reason, "created_at": p.CreatedAt,
			"lock_mode": p.LockMode, "retain_from_key": p.RetainFromKey, "template": p.Template,
		})
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, map[string]any{"policies": out})
}

// listHolds answers the active legal holds, or all of them with
// ?include_released=true.
func (h *GovernanceHandler) listHolds(w nethttp.ResponseWriter, r *nethttp.Request) {
	holds, err := h.lifecycle.HoldsFor(r.Context(), principalOf(r), r.URL.Query().Get("include_released") == "true")
	if err != nil {
		h.governanceError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(holds))
	for _, hold := range holds {
		out = append(out, holdBody(hold))
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, map[string]any{"holds": out})
}

// durationText writes a policy duration the way it is set ("720h0m0s"),
// or nil when the policy leaves it unset.
func durationText(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return d.String()
}

func (h *GovernanceHandler) placeHold(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.LegalHoldRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	hold, err := h.lifecycle.PlaceHoldFor(r.Context(), principalOf(r), model.LegalHold{
		Scope: strings.ToUpper(body.Scope), ScopeKey: body.ScopeKey, Reason: body.Reason,
		Query: body.Query, Matter: body.Matter, Custodians: body.Custodians,
	})
	if err != nil {
		h.governanceError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, holdBody(hold))
}

// releaseHold releases a hold, or answers 202 with the approval it waits
// for under the two-person rule. ?reason= says why.
func (h *GovernanceHandler) releaseHold(w nethttp.ResponseWriter, r *nethttp.Request) {
	err := h.lifecycle.ReleaseHoldGoverned(r.Context(), principalOf(r), r.PathValue("id"), r.URL.Query().Get("reason"))
	if writeApproval(w, err) {
		return
	}
	if err != nil {
		h.governanceError(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

// governanceError adds the lifecycle's own validation errors to the
// security mapping.
func (h *GovernanceHandler) governanceError(w nethttp.ResponseWriter, err error) {
	if _, _, ok := securityStatus(err); ok {
		writeSecurityError(w, h.log, err)
		return
	}
	switch {
	case isNotFound(err), errors.Is(err, model.ErrApprovalNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case errors.Is(err, model.ErrComplianceLocked):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "compliance_locked"})
	case errors.Is(err, model.ErrApprovalDecided):
		writeJSON(w, nethttp.StatusConflict, problem{Error: err.Error(), Code: "approval_decided"})
	case errors.Is(err, model.ErrSelfApproval):
		writeJSON(w, nethttp.StatusForbidden, problem{Error: err.Error(), Code: "self_approval"})
	default:
		// Policy and hold validation: the service's message is safe.
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, asset.ErrNotFound) || errors.Is(err, model.ErrHoldNotFound)
}
