package http

import (
	"errors"
	nethttp "net/http"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/asset"
)

// Governance routes (docs/governance.md): templates, governance settings,
// approvals, bulk deletes, hold proofs and disposition certificates.

func (h *GovernanceHandler) governanceRoutes(routes map[string]nethttp.HandlerFunc) {
	routes["GET /api/v1/retention/templates"] = h.templates
	routes["GET /api/v1/governance"] = h.getGovernance
	routes["PUT /api/v1/governance"] = h.putGovernance
	routes["GET /api/v1/approvals"] = h.approvals
	routes["POST /api/v1/approvals/{id}/approve"] = h.decide(true)
	routes["POST /api/v1/approvals/{id}/reject"] = h.decide(false)
	routes["POST /api/v1/assets/bulk-delete"] = h.bulkDelete
	routes["GET /api/v1/legal-holds/{id}/proof"] = h.holdProof
	routes["GET /api/v1/assets/{id}/certificates"] = h.certificates
	routes["GET /api/v1/deletions/{id}/certificate"] = h.deletionCertificate
	routes["GET /api/v1/certificates/public-key"] = h.publicKey
}

func (h *GovernanceHandler) templates(w nethttp.ResponseWriter, _ *nethttp.Request) {
	out := make([]map[string]any, 0, len(model.RetentionTemplates))
	for _, t := range asset.Templates() {
		out = append(out, map[string]any{"id": t.ID, "name": t.Name, "citation": t.Citation,
			"min_retention": t.MinRetention.String(), "retain_from_key": t.RetainFromKey, "lock_mode": t.LockMode, "note": t.Note})
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"templates": out})
}

func governanceBody(g model.GovernanceSettings) map[string]any {
	body := map[string]any{"profile": g.Profile, "two_person": g.TwoPerson, "approve_deletes": g.ApproveDeletes,
		"retention_floor": g.RetentionFloor.String()}
	if !g.UpdatedAt.IsZero() {
		body["updated_by"], body["updated_at"] = g.UpdatedBy, g.UpdatedAt
	}
	return body
}

func (h *GovernanceHandler) getGovernance(w nethttp.ResponseWriter, r *nethttp.Request) {
	g, err := h.lifecycle.GovernanceFor(r.Context(), principalOf(r))
	if err != nil {
		h.governanceError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, governanceBody(g))
}

func (h *GovernanceHandler) putGovernance(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		Profile        string `json:"profile"`
		TwoPerson      bool   `json:"two_person"`
		ApproveDeletes bool   `json:"approve_deletes"`
		RetentionFloor string `json:"retention_floor"`
		Reason         string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	next := model.GovernanceSettings{Profile: strings.ToUpper(body.Profile), TwoPerson: body.TwoPerson, ApproveDeletes: body.ApproveDeletes}
	if next.Profile == "" {
		next.Profile = model.ProfileStandard
	}
	if body.RetentionFloor != "" {
		floor, err := time.ParseDuration(body.RetentionFloor)
		if err != nil {
			writeJSON(w, nethttp.StatusBadRequest, problem{Error: "retention_floor must be like 52560h", Code: "invalid_request"})
			return
		}
		next.RetentionFloor = floor
	}
	g, err := h.lifecycle.SetGovernanceFor(r.Context(), principalOf(r), next, body.Reason)
	if err != nil {
		h.governanceError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, governanceBody(g))
}

func (h *GovernanceHandler) approvals(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, err := h.lifecycle.ApprovalsFor(r.Context(), principalOf(r), strings.ToUpper(r.URL.Query().Get("state")))
	if err != nil {
		h.governanceError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, map[string]any{"approvals": append([]model.Approval{}, list...)})
}

func (h *GovernanceHandler) decide(approve bool) nethttp.HandlerFunc {
	return func(w nethttp.ResponseWriter, r *nethttp.Request) {
		var body struct {
			Note string `json:"note"`
		}
		if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
			return
		}
		a, err := h.lifecycle.DecideFor(r.Context(), principalOf(r), r.PathValue("id"), approve, body.Note)
		if err != nil && a.ID == "" {
			h.governanceError(w, err)
			return
		}
		// A failed execution is recorded on the approval and answered with it.
		writeJSON(w, nethttp.StatusOK, a)
	}
}

func (h *GovernanceHandler) bulkDelete(w nethttp.ResponseWriter, r *nethttp.Request) {
	var body struct {
		AssetIDs   []string          `json:"asset_ids"`
		Dataset    string            `json:"dataset"`
		PathPrefix string            `json:"path_prefix"`
		Metadata   map[string]string `json:"metadata"`
		Reason     string            `json:"reason"`
		DryRun     bool              `json:"dry_run"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	plan, err := h.lifecycle.BulkDeleteFor(r.Context(), principalOf(r), asset.BulkDeleteInput{
		AssetIDs: body.AssetIDs, Filter: model.HoldQuery{Dataset: body.Dataset, PathPrefix: body.PathPrefix, Metadata: body.Metadata},
		Reason: body.Reason, DryRun: body.DryRun,
	})
	if err != nil {
		h.governanceError(w, err)
		return
	}
	status := nethttp.StatusOK
	if plan.ApprovalID != "" || plan.Scheduled > 0 {
		status = nethttp.StatusAccepted
	}
	writeJSON(w, status, plan)
}

func (h *GovernanceHandler) holdProof(w nethttp.ResponseWriter, r *nethttp.Request) {
	proof, err := h.lifecycle.HoldProofFor(r.Context(), principalOf(r), r.PathValue("id"), r.URL.Query().Get("verify") == "true")
	if err != nil {
		h.governanceError(w, err)
		return
	}
	if proof.Assets == nil {
		proof.Assets = []asset.HeldAsset{}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, nethttp.StatusOK, map[string]any{"hold": holdBody(proof.Hold), "assets": proof.Assets,
		"truncated": proof.Truncated, "generated_at": proof.At})
}

func (h *GovernanceHandler) certificates(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, err := h.lifecycle.CertificatesFor(r.Context(), principalOf(r), r.PathValue("id"), "")
	if err != nil {
		h.governanceError(w, err)
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"certificates": append([]model.DispositionCertificate{}, list...)})
}

func (h *GovernanceHandler) deletionCertificate(w nethttp.ResponseWriter, r *nethttp.Request) {
	list, err := h.lifecycle.CertificatesFor(r.Context(), principalOf(r), "", r.PathValue("id"))
	if err != nil {
		h.governanceError(w, err)
		return
	}
	if len(list) == 0 {
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "no certificate: the deletion has not completed", Code: "not_found"})
		return
	}
	writeJSON(w, nethttp.StatusOK, list[0])
}

func (h *GovernanceHandler) publicKey(w nethttp.ResponseWriter, _ *nethttp.Request) {
	id, key := h.lifecycle.PublicKey()
	if key == "" {
		writeJSON(w, nethttp.StatusNotFound, problem{Error: "certificates are not signed on this platform", Code: "not_configured"})
		return
	}
	writeJSON(w, nethttp.StatusOK, map[string]any{"key_id": id, "algorithm": "Ed25519", "public_key": key})
}

func holdBody(hold model.LegalHold) map[string]any {
	return map[string]any{
		"id": hold.ID, "scope": hold.Scope, "scope_key": hold.ScopeKey, "reason": hold.Reason, "query": hold.Query,
		"matter": hold.Matter, "custodians": append([]string{}, hold.Custodians...),
		"placed_by": hold.PlacedBy, "placed_at": hold.PlacedAt, "released_by": hold.ReleasedBy, "released_at": hold.ReleasedAt,
	}
}

// writeApproval answers a change that now waits for a second member.
func writeApproval(w nethttp.ResponseWriter, err error) bool {
	var pending *model.ApprovalRequiredError
	if !errors.As(err, &pending) {
		return false
	}
	writeJSON(w, nethttp.StatusAccepted, map[string]any{"status": "PENDING_APPROVAL", "approval_id": pending.ID,
		"message": "a second member with retention.manage must approve this change"})
	return true
}
