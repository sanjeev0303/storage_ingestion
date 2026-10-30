package http

import (
	"errors"
	"log/slog"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/access"
)

// AccessHandler serves API tokens, the access policy, share links, egress,
// classification and security alerts (docs/access.md).
type AccessHandler struct {
	service *access.Service
	log     *slog.Logger
}

func NewAccessHandler(service *access.Service, log *slog.Logger) *AccessHandler {
	return &AccessHandler{service: service, log: log}
}

func (h *AccessHandler) Routes() map[string]nethttp.HandlerFunc {
	return map[string]nethttp.HandlerFunc{
		"POST /api/v1/api-tokens":                       h.createToken,
		"GET /api/v1/api-tokens":                        h.tokens,
		"DELETE /api/v1/api-tokens/{id}":                h.revokeToken,
		"GET /api/v1/access-policy":                     h.policy,
		"PUT /api/v1/access-policy":                     h.setPolicy,
		"GET /api/v1/links":                             h.links,
		"POST /api/v1/links/revoke":                     h.revokeLinks,
		"GET /api/v1/egress":                            h.egress,
		"PUT /api/v1/assets/{id}/classification":        h.classify,
		"GET /api/v1/security/alerts":                   h.alerts,
		"POST /api/v1/security/alerts/{id}/acknowledge": h.acknowledge,
	}
}

func (h *AccessHandler) error(w nethttp.ResponseWriter, err error) {
	switch {
	case errors.Is(err, model.ErrInvalidAPIToken), errors.Is(err, model.ErrInvalidPolicyBody):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	case errors.Is(err, model.ErrAPITokenNotFound), errors.Is(err, model.ErrAlertNotFound), errors.Is(err, model.ErrAssetNotFound):
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "not found", Code: "not_found"})
	case err != nil && strings.Contains(err.Error(), "choose links to revoke"):
		writeJSON(w, nethttp.StatusBadRequest, problem{Error: err.Error(), Code: "invalid_request"})
	default:
		writeSecurityError(w, h.log, err)
	}
}

func tokenBody(t model.APIToken) map[string]any {
	return map[string]any{"id": t.ID, "name": t.Name, "user_id": t.UserID, "hint": t.Hint, "permissions": t.Permissions.Names(),
		"datasets": append([]string{}, t.Datasets...), "networks": append([]string{}, t.Networks...), "allow_links": t.AllowLinks,
		"expires_at": t.ExpiresAt, "created_at": t.CreatedAt, "last_used_at": t.LastUsedAt, "last_used_ip": t.LastUsedIP,
		"revoked_at": t.RevokedAt, "revoked_by": t.RevokedBy}
}

func (h *AccessHandler) createToken(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
		Datasets    []string `json:"datasets"`
		Networks    []string `json:"networks"`
		ExpiresIn   string   `json:"expires_in"`
		AllowLinks  bool     `json:"allow_links"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	var expires time.Duration
	if body.ExpiresIn != "" {
		d, err := time.ParseDuration(body.ExpiresIn)
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, problem{Error: "expires_in must be like 720h", Code: "invalid_request"})
			return
		}
		expires = d
	}
	t, raw, err := h.service.CreateTokenFor(r.Context(), principalOf(r), access.TokenInput{Name: body.Name,
		Permissions: body.Permissions, Datasets: body.Datasets, Networks: body.Networks, ExpiresIn: expires, AllowLinks: body.AllowLinks})
	if err != nil {
		h.error(w, err)
		return
	}
	out := tokenBody(t)
	out["token"] = raw
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, nethttp.StatusCreated, out)
}

func (h *AccessHandler) tokens(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, err := h.service.TokensFor(r.Context(), principalOf(r), r.URL.Query().Get("all") == "true")
	if err != nil {
		h.error(w, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, tokenBody(t))
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"tokens": out})
}

func (h *AccessHandler) revokeToken(w nethttp.ResponseWriter, r *nethttp.Request) {
	if err := h.service.RevokeTokenFor(r.Context(), principalOf(r), r.PathValue("id")); err != nil {
		h.error(w, err)
		return
	}
	w.WriteHeader(nethttp.StatusNoContent)
}

func policyJSON(p model.AccessPolicy) map[string]any {
	out := map[string]any{"links_enabled": p.LinksEnabled, "max_link_ttl": p.MaxLinkTTL.String(),
		"egress_monthly_bytes": p.EgressMonthlyBytes, "egress_daily_member_bytes": p.EgressDailyMemberBytes,
		"egress_action": p.EgressAction, "mass_link_threshold": p.MassLinkThreshold, "anomaly_action": p.AnomalyAction}
	if !p.UpdatedAt.IsZero() {
		out["updated_by"], out["updated_at"] = p.UpdatedBy, p.UpdatedAt
	}
	return out
}

func (h *AccessHandler) policy(w nethttp.ResponseWriter, r *nethttp.Request) {
	p, err := h.service.PolicyFor(r.Context(), principalOf(r))
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, policyJSON(p))
}

func (h *AccessHandler) setPolicy(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		LinksEnabled           *bool  `json:"links_enabled"`
		MaxLinkTTL             string `json:"max_link_ttl"`
		EgressMonthlyBytes     int64  `json:"egress_monthly_bytes"`
		EgressDailyMemberBytes int64  `json:"egress_daily_member_bytes"`
		EgressAction           string `json:"egress_action"`
		MassLinkThreshold      int    `json:"mass_link_threshold"`
		AnomalyAction          string `json:"anomaly_action"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	p := model.DefaultAccessPolicy("")
	if body.LinksEnabled != nil {
		p.LinksEnabled = *body.LinksEnabled
	}
	if body.MaxLinkTTL != "" {
		d, err := time.ParseDuration(body.MaxLinkTTL)
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, problem{Error: "max_link_ttl must be like 1h", Code: "invalid_request"})
			return
		}
		p.MaxLinkTTL = d
	}
	p.EgressMonthlyBytes, p.EgressDailyMemberBytes = body.EgressMonthlyBytes, body.EgressDailyMemberBytes
	if body.EgressAction != "" {
		p.EgressAction = strings.ToUpper(body.EgressAction)
	}
	if body.MassLinkThreshold != 0 {
		p.MassLinkThreshold = body.MassLinkThreshold
	}
	if body.AnomalyAction != "" {
		p.AnomalyAction = strings.ToUpper(body.AnomalyAction)
	}
	stored, err := h.service.SetPolicyFor(r.Context(), principalOf(r), p)
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, policyJSON(stored))
}

func (h *AccessHandler) links(w nethttp.ResponseWriter, r *nethttp.Request) {
	q := r.URL.Query()
	list, err := h.service.LinksFor(r.Context(), principalOf(r), q.Get("all") == "true", q.Get("active") != "false")
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"links": append([]model.ShareLink{}, list...)})
}

func (h *AccessHandler) revokeLinks(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		LinkIDs     []string `json:"link_ids"`
		AssetIDs    []string `json:"asset_ids"`
		PrincipalID string   `json:"principal_id"`
		All         bool     `json:"all"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	n, err := h.service.RevokeLinksFor(r.Context(), principalOf(r), postgres.LinkFilter{IDs: body.LinkIDs, AssetIDs: body.AssetIDs,
		PrincipalID: body.PrincipalID, All: body.All})
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"revoked": n})
}

func (h *AccessHandler) egress(w nethttp.ResponseWriter, r *nethttp.Request) {
	report, err := h.service.EgressFor(r.Context(), principalOf(r))
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, report)
}

func (h *AccessHandler) classify(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Classification string `json:"classification"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	class := strings.ToUpper(body.Classification)
	if err := h.service.ClassifyFor(r.Context(), principalOf(r), r.PathValue("id"), class); err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"asset_id": r.PathValue("id"), "classification": class})
}

func (h *AccessHandler) alerts(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, err := h.service.AlertsFor(r.Context(), principalOf(r), strings.ToUpper(r.URL.Query().Get("state")))
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"alerts": append([]model.SecurityAlert{}, list...)})
}

func (h *AccessHandler) acknowledge(w nethttp.ResponseWriter, r *nethttp.Request) {
	a, err := h.service.AcknowledgeFor(r.Context(), principalOf(r), r.PathValue("id"))
	if err != nil {
		h.error(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, a)
}
