package http

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	nethttp "net/http"
	"strconv"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/request"
	"github.com/sanjeev0303/object-storage-platform/internal/dto/response"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
	"github.com/sanjeev0303/object-storage-platform/internal/service/identity"
	"github.com/sanjeev0303/object-storage-platform/internal/service/quota"
)

// IdentityHandler serves sign-in, the caller's account, and the caller's
// organization: its members, invitations, roles, quota and audit log.
type IdentityHandler struct {
	identity *identity.Service
	quotas   *quota.Service
	audit    *audit.Service
	resolve  middleware.Resolver
	log      *slog.Logger
}

func NewIdentityHandler(identitySvc *identity.Service, quotas *quota.Service, auditSvc *audit.Service, resolve middleware.Resolver, log *slog.Logger) *IdentityHandler {
	return &IdentityHandler{identity: identitySvc, quotas: quotas, audit: auditSvc, resolve: resolve, log: log}
}

// Routes groups the handler's endpoints by what they require, so the
// router wraps each group in the right middleware.
type IdentityRoutes struct {
	Public  map[string]nethttp.HandlerFunc
	Account map[string]nethttp.HandlerFunc
	Tenant  map[string]nethttp.HandlerFunc
}

func (h *IdentityHandler) Routes() IdentityRoutes {
	return IdentityRoutes{
		Public: map[string]nethttp.HandlerFunc{
			"POST /api/v1/auth/login":          h.login,
			"POST /api/v1/auth/register":       h.register,
			"POST /api/v1/auth/refresh":        h.refresh,
			"POST /api/v1/auth/activate":       h.activate,
			"POST /api/v1/auth/password/reset": h.resetPassword,
			"POST /api/v1/invitations/accept":  h.acceptInvitation,
		},
		Account: map[string]nethttp.HandlerFunc{
			"POST /api/v1/auth/logout":          h.logout,
			"GET /api/v1/auth/me":               h.me,
			"GET /api/v1/auth/sessions":         h.sessions,
			"DELETE /api/v1/auth/sessions/{id}": h.revokeSession,
			"POST /api/v1/auth/password":        h.changePassword,
		},
		Tenant: map[string]nethttp.HandlerFunc{
			"GET /api/v1/organization":                               h.getOrganization,
			"PATCH /api/v1/organization":                             h.updateOrganization,
			"POST /api/v1/organization/deactivate":                   h.deactivateOrganization,
			"GET /api/v1/organization/members":                       h.listMembers,
			"PUT /api/v1/organization/members/{user_id}/role":        h.changeRole,
			"POST /api/v1/organization/members/{user_id}/suspend":    h.suspendMember,
			"POST /api/v1/organization/members/{user_id}/reactivate": h.reactivateMember,
			"DELETE /api/v1/organization/members/{user_id}":          h.removeMember,
			"GET /api/v1/organization/invitations":                   h.listInvitations,
			"POST /api/v1/organization/invitations":                  h.invite,
			"DELETE /api/v1/organization/invitations/{id}":           h.revokeInvitation,
			"GET /api/v1/organization/roles":                         h.listRoles,
			"POST /api/v1/organization/roles":                        h.createRole,
			"PUT /api/v1/organization/roles/{id}":                    h.updateRole,
			"DELETE /api/v1/organization/roles/{id}":                 h.deleteRole,
			"GET /api/v1/organization/quota":                         h.getQuota,
			"GET /api/v1/organization/audit":                         h.listAudit,
			"GET /api/v1/organization/audit/verify":                  h.verifyAudit,
		},
	}
}

func client(r *nethttp.Request) identity.Client {
	agent := r.UserAgent()
	if len(agent) > 200 {
		agent = agent[:200]
	}
	return identity.Client{IP: correlation.Source(r.Context()), UserAgent: agent}
}

func tokensResponse(t identity.Tokens) response.TokensResponse {
	return response.TokensResponse{
		TokenType: "Bearer", AccessToken: t.AccessToken, AccessExpiresAt: t.AccessExpiresAt,
		RefreshToken: t.RefreshToken, RefreshExpiresAt: t.RefreshExpiresAt, SessionID: t.SessionID, User: userResponse(t.User),
	}
}

func userResponse(u model.User) response.UserResponse {
	return response.UserResponse{
		ID: u.ID, Email: u.Email, DisplayName: u.DisplayName, Status: string(u.Status),
		PlatformRole: string(u.PlatformRole), LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt,
	}
}

func memberResponse(m model.Membership, org *model.Organization) response.MembershipResponse {
	out := response.MembershipResponse{
		OrganizationID: m.OrgID, UserID: m.UserID, Email: m.Email, DisplayName: m.DisplayName,
		RoleID: m.RoleID, Status: string(m.Status), CreatedAt: m.CreatedAt,
	}
	if org != nil {
		out.OrganizationName, out.OrganizationSlug = org.Name, org.Slug
	}
	return out
}

func orgResponse(o model.Organization) response.OrganizationResponse {
	return response.OrganizationResponse{
		ID: o.ID, Name: o.Name, Slug: o.Slug, Status: string(o.Status), AllowedEmailDomains: o.Settings.AllowedEmailDomains,
		InvitationTTLHours: o.Settings.InvitationTTLHours, CreatedAt: o.CreatedAt, DeactivatedAt: o.DeactivatedAt,
	}
}

func roleResponse(r model.Role) response.RoleResponse {
	return response.RoleResponse{ID: r.ID, Name: r.Name, Description: r.Description, Permissions: r.Permissions.Names(), BuiltIn: r.BuiltIn}
}

func invitationResponse(i model.Invitation, token string) response.InvitationResponse {
	return response.InvitationResponse{
		ID: i.ID, Email: i.Email, RoleID: i.RoleID, Status: string(i.Status), CreatedBy: i.CreatedBy,
		CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt, Accepted: i.AcceptedAt, Token: token,
	}
}

func auditResponse(e model.AuditEvent) response.AuditEventResponse {
	return response.AuditEventResponse{
		ID: e.ID, Seq: e.Seq, TenantID: e.TenantID, ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		ResourceType: e.ResourceType, ResourceID: e.ResourceID, Outcome: e.Outcome, Reason: e.Reason,
		CorrelationID: e.CorrelationID, SourceIP: e.SourceIP, Details: e.Details, CreatedAt: e.CreatedAt,
		Hash: hex.EncodeToString(e.Hash),
	}
}

func auditPage(events []model.AuditEvent, limit int) response.AuditPageResponse {
	page := response.AuditPageResponse{Events: make([]response.AuditEventResponse, 0, len(events))}
	for _, e := range events {
		page.Events = append(page.Events, auditResponse(e))
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if len(events) == limit {
		page.NextBefore = events[len(events)-1].Seq
	}
	return page
}

func verificationResponse(v interfaces.AuditVerification) response.AuditVerificationResponse {
	return response.AuditVerificationResponse{
		Chain: v.Chain, Checked: v.Checked, FirstSeq: v.FirstSeq, LastSeq: v.LastSeq,
		Intact: v.BrokenAt == 0, BrokenAt: v.BrokenAt, Problem: v.Problem,
	}
}

func quotaResponse(r quota.Report) response.QuotaResponse {
	p, u := r.Policy, r.Usage
	return response.QuotaResponse{
		Limits: map[string]int64{
			"max_storage_bytes": p.MaxStorageBytes, "max_assets": p.MaxAssets, "max_object_bytes": p.MaxObjectBytes,
			"max_active_uploads": int64(p.MaxActiveUploads), "max_active_multipart": int64(p.MaxActiveMultipart),
			"max_active_jobs": int64(p.MaxActiveJobs), "max_pending_processing": int64(p.MaxPendingProcessing),
			"max_pending_entries": p.MaxPendingEntries, "max_manifest_entries": p.MaxManifestEntries,
			"max_manifest_bytes": p.MaxManifestBytes, "max_requests_per_minute": int64(p.MaxRequestsPerMinute),
		},
		Usage: map[string]int64{
			"reserved_bytes": u.ReservedBytes, "reserved_uploads": u.ReservedUploads, "stored_bytes": u.StoredBytes,
			"asset_count": u.AssetCount, "pending_deletion_bytes": u.PendingDeletionBytes, "quarantined_bytes": u.QuarantinedBytes,
			"incomplete_multipart_bytes": u.IncompleteMultipartBytes, "active_uploads": u.ActiveUploads,
			"active_multipart": u.ActiveMultipart, "pending_processing": u.PendingProcessing,
			"remaining_storage_bytes": max(p.MaxStorageBytes-u.ReservedBytes-u.StoredBytes-u.PendingDeletionBytes, 0),
		},
	}
}

func queryInt(r *nethttp.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

// ─── Public ───────────────────────────────────────────────────────────

func (h *IdentityHandler) login(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.LoginRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	tokens, err := h.identity.Login(r.Context(), body.Email, body.Password, client(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusOK, tokensResponse(tokens))
}

func (h *IdentityHandler) register(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.RegisterRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	tokens, err := h.identity.Register(r.Context(), identity.Registration{
		Email: body.Email, Password: body.Password, DisplayName: body.DisplayName, OrganizationName: body.OrganizationName,
	}, client(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusCreated, tokensResponse(tokens))
}

func (h *IdentityHandler) refresh(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.RefreshRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	tokens, err := h.identity.Refresh(r.Context(), body.RefreshToken, client(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusOK, tokensResponse(tokens))
}

func (h *IdentityHandler) activate(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.consume(w, r, h.identity.Activate)
}

func (h *IdentityHandler) resetPassword(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.consume(w, r, h.identity.ResetPassword)
}

// consume spends an activation or reset token with a new password.
func (h *IdentityHandler) consume(w nethttp.ResponseWriter, r *nethttp.Request,
	fn func(ctx context.Context, token, pw string, c identity.Client) (model.User, error)) {
	var body request.CredentialTokenRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	user, err := fn(r.Context(), body.Token, body.Password, client(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, userResponse(user))
}

func (h *IdentityHandler) acceptInvitation(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.AcceptInvitationRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	// A signed-in caller joins with their account; the token alone never
	// signs anyone in.
	var caller *model.Principal
	if middleware.BearerToken(r) != "" {
		p, err := h.resolve(r)
		if err != nil && p.UserID == "" {
			writeSecurityError(w, h.log, model.ErrUnauthenticated)
			return
		}
		caller = &p
	}
	var account *identity.NewAccount
	if caller == nil {
		account = &identity.NewAccount{Password: body.Password, DisplayName: body.DisplayName}
	}
	member, err := h.identity.AcceptInvitation(r.Context(), body.Token, caller, account, client(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, memberResponse(member, nil))
}

// ─── Account ──────────────────────────────────────────────────────────

func (h *IdentityHandler) logout(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.identity.Logout(r.Context(), principalOf(r)); err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *IdentityHandler) me(w nethttp.ResponseWriter, r *nethttp.Request) {
	me, err := h.identity.Me(r.Context(), principalOf(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := response.MeResponse{User: userResponse(me.User), Memberships: []response.MembershipResponse{}}
	for i, m := range me.Memberships {
		out.Memberships = append(out.Memberships, memberResponse(m, &me.Organizations[i]))
	}
	writeJSON(w, nethttp.StatusOK, out)
}

func (h *IdentityHandler) sessions(w nethttp.ResponseWriter, r *nethttp.Request) {
	sessions, err := h.identity.Sessions(r.Context(), principalOf(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := make([]response.SessionResponse, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, response.SessionResponse{
			ID: s.ID, SourceIP: s.SourceIP, UserAgent: s.UserAgent, CreatedAt: s.CreatedAt, LastUsedAt: s.LastUsedAt,
			ExpiresAt: s.ExpiresAt, AccessExpiresAt: s.AccessExpiresAt,
		})
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"sessions": out})
}

func (h *IdentityHandler) revokeSession(w nethttp.ResponseWriter, r *nethttp.Request) {
	err := h.identity.RevokeSession(r.Context(), principalOf(r), r.PathValue("id"))
	if errors.Is(err, model.ErrSessionInvalid) {
		// Not one of the caller's live sessions.
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "session not found", Code: "not_found"})
		return
	}
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *IdentityHandler) changePassword(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.ChangePasswordRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := h.identity.ChangePassword(r.Context(), principalOf(r), body.CurrentPassword, body.NewPassword); err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

// ─── Organization ─────────────────────────────────────────────────────

func (h *IdentityHandler) getOrganization(w nethttp.ResponseWriter, r *nethttp.Request) {
	org, err := h.identity.GetOrganization(r.Context(), principalOf(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, orgResponse(org))
}

func (h *IdentityHandler) updateOrganization(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.UpdateOrganizationRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	org, err := h.identity.UpdateOrganization(r.Context(), principalOf(r), body.Name, model.OrgSettings{
		AllowedEmailDomains: body.AllowedEmailDomains, InvitationTTLHours: body.InvitationTTLHours,
	})
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, orgResponse(org))
}

func (h *IdentityHandler) deactivateOrganization(w nethttp.ResponseWriter, r *nethttp.Request) {
	org, err := h.identity.DeactivateOrganization(r.Context(), principalOf(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, orgResponse(org))
}

func (h *IdentityHandler) listMembers(w nethttp.ResponseWriter, r *nethttp.Request) {
	members, err := h.identity.ListMembers(r.Context(), principalOf(r), r.URL.Query().Get("after"), queryInt(r, "limit"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := make([]response.MembershipResponse, 0, len(members))
	for _, m := range members {
		out = append(out, memberResponse(m, nil))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"members": out})
}

func (h *IdentityHandler) changeRole(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.ChangeRoleRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	member, err := h.identity.ChangeMemberRole(r.Context(), principalOf(r), r.PathValue("user_id"), body.RoleID)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, memberResponse(member, nil))
}

func (h *IdentityHandler) suspendMember(w nethttp.ResponseWriter, r *nethttp.Request) {
	member, err := h.identity.SuspendMember(r.Context(), principalOf(r), r.PathValue("user_id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, memberResponse(member, nil))
}

func (h *IdentityHandler) reactivateMember(w nethttp.ResponseWriter, r *nethttp.Request) {
	member, err := h.identity.ReactivateMember(r.Context(), principalOf(r), r.PathValue("user_id"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, memberResponse(member, nil))
}

func (h *IdentityHandler) removeMember(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.identity.RemoveMember(r.Context(), principalOf(r), r.PathValue("user_id")); err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *IdentityHandler) listInvitations(w nethttp.ResponseWriter, r *nethttp.Request) {
	invitations, err := h.identity.ListInvitations(r.Context(), principalOf(r), r.URL.Query().Get("after"), queryInt(r, "limit"))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := make([]response.InvitationResponse, 0, len(invitations))
	for _, inv := range invitations {
		out = append(out, invitationResponse(inv, ""))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"invitations": out})
}

func (h *IdentityHandler) invite(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.InviteRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	inv, err := h.identity.Invite(r.Context(), principalOf(r), body.Email, body.RoleID)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusCreated, invitationResponse(inv.Invitation, inv.Token))
}

func (h *IdentityHandler) revokeInvitation(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.identity.RevokeInvitation(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *IdentityHandler) listRoles(w nethttp.ResponseWriter, r *nethttp.Request) {
	roles, err := h.identity.Roles(r.Context(), principalOf(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	out := make([]response.RoleResponse, 0, len(roles))
	for _, role := range roles {
		out = append(out, roleResponse(role))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"roles": out})
}

func (h *IdentityHandler) createRole(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.RoleRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	role, err := h.identity.CreateRole(r.Context(), principalOf(r), body.Name, body.Description, body.Permissions)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusCreated, roleResponse(role))
}

func (h *IdentityHandler) updateRole(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body request.RoleRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	role, err := h.identity.UpdateRole(r.Context(), principalOf(r), r.PathValue("id"), body.Name, body.Description, body.Permissions)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, roleResponse(role))
}

func (h *IdentityHandler) deleteRole(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.identity.DeleteRole(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func (h *IdentityHandler) getQuota(w nethttp.ResponseWriter, r *nethttp.Request) {
	report, err := h.quotas.Get(r.Context(), principalOf(r))
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, quotaResponse(report))
}

func (h *IdentityHandler) listAudit(w nethttp.ResponseWriter, r *nethttp.Request) {
	f := auditFilter(r)
	events, err := h.audit.List(r.Context(), principalOf(r), f)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, auditPage(events, f.Limit))
}

func (h *IdentityHandler) verifyAudit(w nethttp.ResponseWriter, r *nethttp.Request) {
	p := principalOf(r)
	v, err := h.audit.Verify(r.Context(), p, p.TenantID)
	if err != nil {
		writeSecurityError(w, h.log, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, verificationResponse(v))
}

// auditFilter reads the listing filters. Action filters are plain names,
// so nothing the caller sends reaches a LIKE pattern.
func auditFilter(r *nethttp.Request) audit.Filter {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	f := audit.Filter{BeforeSeq: before, Limit: queryInt(r, "limit"), Outcome: strings.ToUpper(q.Get("outcome")), ActorID: q.Get("actor")}
	action := q.Get("action")
	plain := true
	for _, c := range action {
		if !(c >= 'a' && c <= 'z' || c == '.' || c == '_') {
			plain = false
			break
		}
	}
	if plain {
		f.Action = action
	}
	return f
}
