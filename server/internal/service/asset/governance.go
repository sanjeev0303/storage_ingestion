package asset

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Governance (docs/governance.md): how an organization guards its
// records beyond retention periods and holds. A second member approves
// what loosens protection, COMPLIANCE policies can only get stricter,
// locks are read back from the provider to catch tampering, and every
// permanent deletion leaves a signed certificate.

// Governance is the store behind it (postgres.GovernanceRepository).
type Governance interface {
	GetSettings(ctx context.Context, tenantID string) (model.GovernanceSettings, error)
	PutSettings(ctx context.Context, g model.GovernanceSettings) error
	CreateApproval(ctx context.Context, a model.Approval) error
	GetApproval(ctx context.Context, tenantID, id string) (model.Approval, error)
	ListApprovals(ctx context.Context, tenantID, state string, now time.Time, limit int) ([]model.Approval, error)
	Decide(ctx context.Context, tenantID, id, state, by, note string, at time.Time) (model.Approval, error)
	Finish(ctx context.Context, id, state, result string) error
	PutCertificate(ctx context.Context, c model.DispositionCertificate) error
	Certificates(ctx context.Context, tenantID, assetID, deletionID string) ([]model.DispositionCertificate, error)
	QueueWebhook(ctx context.Context, tenantID, eventType string, payload map[string]any) error
	ScopeImpact(ctx context.Context, tenantID, scope, key string) (int64, int64, error)
	HoldAssets(ctx context.Context, hold model.LegalHold, afterID string, limit int) ([]model.HoldCoverage, error)
	FindAssets(ctx context.Context, tenantID string, q model.HoldQuery, ids []string, limit int) ([]model.Asset, error)
}

// Signer signs disposition certificates.
type Signer struct {
	KeyID string
	Key   ed25519.PrivateKey
}

const approvalTTL = 7 * 24 * time.Hour

// settings is the organization's governance with its profile applied.
func (s *Service) settings(ctx context.Context, tenantID string) (model.GovernanceSettings, error) {
	if s.deps.Governance == nil {
		return model.GovernanceSettings{TenantID: tenantID, Profile: model.ProfileStandard}, nil
	}
	g, err := s.deps.Governance.GetSettings(ctx, tenantID)
	return g.ApplyProfile(), err
}

// alert tells subscribed endpoints about an attempt to loosen protection,
// whether it is then made, refused or sent for approval.
func (s *Service) alert(ctx context.Context, tenantID, eventType string, payload map[string]any) {
	if s.deps.Governance == nil {
		return
	}
	if err := s.deps.Governance.QueueWebhook(ctx, tenantID, eventType, payload); err != nil {
		s.log.Warn("governance webhook not queued", "event", eventType, "tenant_id", tenantID, "error", err)
	}
}

// requestApproval records a change for a second member and returns the
// error that says so.
func (s *Service) requestApproval(ctx context.Context, p model.Principal, kind, summary, reason string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: a reason is required for a change that needs approval", model.ErrGovernanceInvalid)
	}
	now := s.now()
	a := model.Approval{ID: s.newID(), TenantID: p.TenantID, Kind: kind, Summary: summary, Payload: body, Reason: reason,
		RequestedBy: "user:" + p.UserID, RequestedAt: now, ExpiresAt: now.Add(approvalTTL)}
	if err := s.deps.Governance.CreateApproval(ctx, a); err != nil {
		return err
	}
	s.audit(ctx, p, "governance.approval_request", "approval", a.ID, map[string]any{"kind": kind, "summary": summary})
	s.alert(ctx, p.TenantID, "governance.approval_requested", map[string]any{"approval_id": a.ID, "kind": kind, "summary": summary,
		"requested_by": a.RequestedBy})
	return &model.ApprovalRequiredError{ID: a.ID}
}

// ─── Retention policies ───────────────────────────────────────────────

// PolicyInput is a policy change and how to make it.
type PolicyInput struct {
	Policy model.RetentionPolicy
	// ConfirmCompliance must be set to make a COMPLIANCE policy: nobody,
	// the platform included, can delete what it locks before it ends.
	ConfirmCompliance bool
	// DryRun returns the change's impact and stores nothing.
	DryRun bool
}

// PolicyImpact is what a policy change would cover.
type PolicyImpact struct {
	Assets      int64    `json:"assets"`
	Bytes       int64    `json:"bytes"`
	LockMode    string   `json:"lock_mode"`
	Loosens     bool     `json:"loosens"`
	NeedsReview bool     `json:"needs_approval"`
	Notes       []string `json:"notes"`
}

// applyTemplate fills a policy's unset settings from its template.
func applyTemplate(policy *model.RetentionPolicy) error {
	if policy.Template == "" {
		return nil
	}
	t, ok := model.TemplateByID(policy.Template)
	if !ok {
		return fmt.Errorf("%w: unknown template %q", model.ErrInvalidPolicy, policy.Template)
	}
	if policy.MinRetention == nil {
		d := t.MinRetention
		policy.MinRetention = &d
	}
	if policy.LockMode == "" {
		policy.LockMode = t.LockMode
	}
	if policy.RetainFromKey == "" {
		policy.RetainFromKey = t.RetainFromKey
	}
	return nil
}

// loosens reports whether next keeps records less long or less firmly
// than prev, the version it replaces.
func loosens(prev, next model.RetentionPolicy) bool {
	shorter := func(a, b *time.Duration) bool { return a != nil && (b == nil || *b < *a) }
	return shorter(prev.MinRetention, next.MinRetention) ||
		(prev.LockMode == model.LockCompliance && next.LockMode != model.LockCompliance) ||
		(prev.RetainFromKey != "" && next.RetainFromKey != prev.RetainFromKey)
}

// SetPolicyGoverned is SetPolicyFor with the governance rules: templates,
// the organization's retention floor, COMPLIANCE guardrails and
// approvals for loosening changes. It answers the impact on a dry run.
func (s *Service) SetPolicyGoverned(ctx context.Context, p model.Principal, in PolicyInput) (model.RetentionPolicy, PolicyImpact, error) {
	if err := audit.Require(ctx, s.security, p, model.PermRetentionManage, "retention.policy_set", "retention_policy", in.Policy.ScopeKey); err != nil {
		return model.RetentionPolicy{}, PolicyImpact{}, err
	}
	policy := in.Policy
	policy.TenantID, policy.CreatedBy = p.TenantID, "user:"+p.UserID
	if err := applyTemplate(&policy); err != nil {
		return model.RetentionPolicy{}, PolicyImpact{}, err
	}
	g, err := s.settings(ctx, p.TenantID)
	if err != nil {
		return model.RetentionPolicy{}, PolicyImpact{}, err
	}
	if g.RetentionFloor > 0 && policy.MinRetention != nil && *policy.MinRetention < g.RetentionFloor {
		return model.RetentionPolicy{}, PolicyImpact{}, fmt.Errorf("%w: the organization's %s profile keeps records at least %s",
			model.ErrInvalidPolicy, g.Profile, g.RetentionFloor)
	}
	if policy.LockMode == model.LockCompliance && !in.ConfirmCompliance && !in.DryRun {
		return model.RetentionPolicy{}, PolicyImpact{}, fmt.Errorf("%w: confirm_compliance must be set: a COMPLIANCE lock cannot be shortened or removed by anyone until it ends",
			model.ErrInvalidPolicy)
	}

	prev, found, err := s.deps.Policies.Effective(ctx, p.TenantID, policy.Scope, policy.ScopeKey, s.now())
	if err != nil {
		return model.RetentionPolicy{}, PolicyImpact{}, err
	}
	loose := found && loosens(prev, policy)
	impact := PolicyImpact{LockMode: policy.LockMode, Loosens: loose, NeedsReview: loose && g.TwoPerson}
	if impact.LockMode == "" {
		impact.LockMode = model.LockGovernance
	}
	if s.deps.Governance != nil {
		key := policy.ScopeKey
		if impact.Assets, impact.Bytes, err = s.deps.Governance.ScopeImpact(ctx, p.TenantID, policy.Scope, key); err != nil {
			return model.RetentionPolicy{}, impact, err
		}
	}
	if found && prev.LockMode == model.LockCompliance && loose {
		impact.Notes = append(impact.Notes, "refused: the policy it replaces is COMPLIANCE")
		if !in.DryRun {
			s.alert(ctx, p.TenantID, "retention.change_requested", map[string]any{"scope": policy.Scope, "scope_key": policy.ScopeKey,
				"by": policy.CreatedBy, "outcome": "refused", "reason": "COMPLIANCE"})
			return model.RetentionPolicy{}, impact, model.ErrComplianceLocked
		}
	}
	if policy.LockMode == model.LockCompliance {
		impact.Notes = append(impact.Notes, fmt.Sprintf("%d versions on buckets with Object Lock get COMPLIANCE locks; nobody can delete them early", impact.Assets))
	}
	if in.DryRun {
		return policy, impact, nil
	}
	if loose {
		s.alert(ctx, p.TenantID, "retention.change_requested", map[string]any{"scope": policy.Scope, "scope_key": policy.ScopeKey,
			"by": policy.CreatedBy, "outcome": map[bool]string{true: "approval_requested", false: "made"}[g.TwoPerson]})
		if g.TwoPerson {
			summary := fmt.Sprintf("loosen the %s %s retention policy", strings.ToLower(policy.Scope), policy.ScopeKey)
			return model.RetentionPolicy{}, impact, s.requestApproval(ctx, p, model.ApprovalRetentionChange, summary, policy.Reason, policy)
		}
	}
	stored, err := s.SetPolicy(ctx, policy)
	if err == nil {
		s.audit(ctx, p, "retention.policy_set", "retention_policy", stored.ID,
			map[string]any{"scope": stored.Scope, "scope_key": stored.ScopeKey, "version": stored.Version, "lock_mode": stored.LockMode})
	}
	return stored, impact, err
}

// ─── Legal holds ──────────────────────────────────────────────────────

// ReleaseHoldGoverned releases a hold, or asks for approval under the
// two-person rule. Every attempt is announced.
func (s *Service) ReleaseHoldGoverned(ctx context.Context, p model.Principal, holdID, reason string) error {
	if err := audit.Require(ctx, s.security, p, model.PermRetentionManage, "retention.hold_release", "legal_hold", holdID); err != nil {
		return err
	}
	g, err := s.settings(ctx, p.TenantID)
	if err != nil {
		return err
	}
	s.alert(ctx, p.TenantID, "retention.change_requested", map[string]any{"hold_id": holdID, "by": "user:" + p.UserID,
		"outcome": map[bool]string{true: "approval_requested", false: "made"}[g.TwoPerson]})
	if g.TwoPerson {
		return s.requestApproval(ctx, p, model.ApprovalHoldRelease, "release legal hold "+holdID, reason, map[string]string{"hold_id": holdID})
	}
	if err := s.ReleaseHold(ctx, p.TenantID, holdID, "user:"+p.UserID); err != nil {
		return err
	}
	s.audit(ctx, p, "retention.hold_release", "legal_hold", holdID, map[string]any{"reason": reason})
	return nil
}

// HoldProof is what a hold covers and the evidence that its locks took
// effect: when each was applied, and what the provider holds now when
// asked to read it back.
type HoldProof struct {
	Hold   model.LegalHold `json:"hold"`
	Assets []HeldAsset     `json:"assets"`
	// Truncated: more assets are covered than listed.
	Truncated bool      `json:"truncated"`
	At        time.Time `json:"generated_at"`
}

type HeldAsset struct {
	AssetID          string     `json:"asset_id"`
	FileName         string     `json:"file_name"`
	Status           string     `json:"status"`
	ChecksumSHA256   string     `json:"checksum_sha256"`
	StorageVersionID string     `json:"storage_version_id,omitempty"`
	LockApplied      *time.Time `json:"lock_applied_at,omitempty"`
	LockLegalHold    bool       `json:"lock_legal_hold"`
	LockMode         string     `json:"lock_mode,omitempty"`
	LockUntil        *time.Time `json:"lock_retain_until,omitempty"`
	// Provider is the read-back: "" when not asked, else MATCH or what
	// differs, or why it could not be read.
	Provider string `json:"provider,omitempty"`
}

const proofLimit, proofReadBacks = 1000, 100

// HoldProofFor lists a hold's assets and their locks. With verify, it
// reads up to proofReadBacks of them back from the provider.
func (s *Service) HoldProofFor(ctx context.Context, p model.Principal, holdID string, verify bool) (HoldProof, error) {
	if err := audit.Require(ctx, s.security, p, model.PermRetentionManage, "retention.hold_read", "legal_hold", holdID); err != nil {
		return HoldProof{}, err
	}
	if s.deps.Governance == nil {
		return HoldProof{}, errors.New("governance is not configured")
	}
	holds, err := s.deps.Holds.List(ctx, p.TenantID, true)
	if err != nil {
		return HoldProof{}, err
	}
	var hold *model.LegalHold
	for i := range holds {
		if holds[i].ID == holdID {
			hold = &holds[i]
		}
	}
	if hold == nil {
		return HoldProof{}, model.ErrHoldNotFound
	}
	covered, err := s.deps.Governance.HoldAssets(ctx, *hold, "", proofLimit+1)
	if err != nil {
		return HoldProof{}, err
	}
	out := HoldProof{Hold: *hold, At: s.now(), Truncated: len(covered) > proofLimit}
	read := 0
	for i, c := range covered {
		if i == proofLimit {
			break
		}
		h := HeldAsset{AssetID: c.Asset.ID, FileName: c.Asset.FileName, Status: string(c.Asset.Status),
			ChecksumSHA256: c.Asset.ChecksumSHA256, StorageVersionID: c.Asset.StorageVersionID}
		if c.Lock != nil {
			applied := c.Lock.AppliedAt
			h.LockApplied, h.LockLegalHold, h.LockMode, h.LockUntil = &applied, c.Lock.LegalHold, c.Lock.Mode, c.Lock.RetainUntil
		}
		if verify && c.Lock != nil && read < proofReadBacks {
			read++
			h.Provider = s.readBack(ctx, c.Asset, *c.Lock)
		}
		out.Assets = append(out.Assets, h)
	}
	s.audit(ctx, p, "retention.hold_proof", "legal_hold", holdID, map[string]any{"assets": len(out.Assets), "verified": read})
	return out, nil
}

// readBack compares a recorded lock with what the provider holds.
func (s *Service) readBack(ctx context.Context, asset model.Asset, recorded model.ObjectLock) string {
	locker, bucket, key, enabled, err := s.lockable(ctx, asset)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if !enabled {
		return "unreadable: the bucket has no Object Lock"
	}
	got, err := locker.ObjectLockStatus(ctx, bucket, key, asset.StorageVersionID)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	var diffs []string
	if recorded.LegalHold && !got.LegalHold {
		diffs = append(diffs, "legal hold is off at the provider")
	}
	if recorded.RetainUntil != nil && recorded.RetainUntil.After(s.now()) {
		if got.Until.IsZero() || got.Until.Before(recorded.RetainUntil.Add(-time.Second)) {
			diffs = append(diffs, fmt.Sprintf("retention ends %s at the provider, recorded %s", formatTime(got.Until), recorded.RetainUntil.Format(time.RFC3339)))
		}
		if recorded.Mode == model.LockCompliance && got.Mode != model.LockCompliance {
			diffs = append(diffs, "mode is "+got.Mode+" at the provider, recorded COMPLIANCE")
		}
	}
	if len(diffs) == 0 {
		return "MATCH"
	}
	return "MISMATCH: " + strings.Join(diffs, "; ")
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// lockVerifyBatch is how many locks a cycle reads back.
const lockVerifyBatch = 50

// VerifyLocks reads back the locks verified longest ago. One that the
// provider no longer holds as recorded (lifted with a bypass, or changed
// out of band) is recorded, counted and announced as object.lock_tampered.
func (s *Service) VerifyLocks(ctx context.Context, stats *CycleStats) error {
	if s.deps.Locks == nil || s.deps.Governance == nil || s.cfg.DryRun {
		return nil
	}
	due, err := s.deps.Locks.DueVerification(ctx, s.now(), lockVerifyBatch)
	if err != nil {
		return err
	}
	for _, lock := range due {
		asset, err := s.deps.Assets.GetByID(ctx, lock.TenantID, lock.AssetID)
		if err != nil {
			continue
		}
		result := s.readBack(ctx, *asset, lock)
		if err := s.deps.Locks.RecordVerification(ctx, lock.AssetID, result, s.now()); err != nil {
			return err
		}
		stats.LocksVerified++
		if strings.HasPrefix(result, "MISMATCH") {
			stats.LocksTampered++
			s.deps.Metrics.Add("object_locks_tampered", 1)
			s.log.Error("object lock differs at the provider", "asset_id", lock.AssetID, "tenant_id", lock.TenantID, "result", result)
			s.alert(ctx, lock.TenantID, "object.lock_tampered", map[string]any{"asset_id": lock.AssetID, "detail": result})
			s.record(ctx, s.newEvent(lock.TenantID, lock.AssetID, "", "lock_tampered", "MISMATCH", result, s.actor()))
		}
	}
	return nil
}

// ─── Governance settings and approvals ───────────────────────────────

// GovernanceFor returns the organization's governance settings.
func (s *Service) GovernanceFor(ctx context.Context, p model.Principal) (model.GovernanceSettings, error) {
	if err := audit.Require(ctx, s.security, p, model.PermRetentionManage, "governance.read", "organization", p.TenantID); err != nil {
		return model.GovernanceSettings{}, err
	}
	return s.settings(ctx, p.TenantID)
}

// SetGovernanceFor changes them. Loosening any rule under the two-person
// rule waits for approval.
func (s *Service) SetGovernanceFor(ctx context.Context, p model.Principal, next model.GovernanceSettings, reason string) (model.GovernanceSettings, error) {
	if err := audit.Require(ctx, s.security, p, model.PermRetentionManage, "governance.update", "organization", p.TenantID); err != nil {
		return model.GovernanceSettings{}, err
	}
	if s.deps.Governance == nil {
		return model.GovernanceSettings{}, errors.New("governance is not configured")
	}
	if err := next.Validate(); err != nil {
		return model.GovernanceSettings{}, err
	}
	current, err := s.settings(ctx, p.TenantID)
	if err != nil {
		return model.GovernanceSettings{}, err
	}
	next.TenantID = p.TenantID
	if current.TwoPerson && current.Weaker(next.ApplyProfile()) {
		return model.GovernanceSettings{}, s.requestApproval(ctx, p, model.ApprovalGovernanceChange, "loosen governance to "+next.Profile, reason, next)
	}
	return s.putGovernance(ctx, p, next)
}

func (s *Service) putGovernance(ctx context.Context, p model.Principal, next model.GovernanceSettings) (model.GovernanceSettings, error) {
	next.TenantID, next.UpdatedBy, next.UpdatedAt = p.TenantID, "user:"+p.UserID, s.now()
	if err := s.deps.Governance.PutSettings(ctx, next); err != nil {
		return model.GovernanceSettings{}, err
	}
	s.audit(ctx, p, "governance.update", "organization", p.TenantID, map[string]any{"profile": next.Profile,
		"two_person": next.TwoPerson, "approve_deletes": next.ApproveDeletes})
	return next.ApplyProfile(), nil
}

// ApprovalsFor lists the organization's approvals.
func (s *Service) ApprovalsFor(ctx context.Context, p model.Principal, state string) ([]model.Approval, error) {
	if err := audit.Require(ctx, s.security, p, model.PermRetentionManage, "governance.approval_read", "approval", ""); err != nil {
		return nil, err
	}
	if s.deps.Governance == nil {
		return nil, nil
	}
	return s.deps.Governance.ListApprovals(ctx, p.TenantID, state, s.now(), 200)
}

// DecideFor approves (and so carries out) or rejects a pending change. The
// member who asked cannot approve it.
func (s *Service) DecideFor(ctx context.Context, p model.Principal, id string, approve bool, note string) (model.Approval, error) {
	if err := audit.Require(ctx, s.security, p, model.PermRetentionManage, "governance.approval_decide", "approval", id); err != nil {
		return model.Approval{}, err
	}
	if s.deps.Governance == nil {
		return model.Approval{}, model.ErrApprovalNotFound
	}
	pending, err := s.deps.Governance.GetApproval(ctx, p.TenantID, id)
	if err != nil {
		return model.Approval{}, err
	}
	if approve && pending.RequestedBy == "user:"+p.UserID {
		return model.Approval{}, model.ErrSelfApproval
	}
	state := model.ApprovalRejected
	if approve {
		state = model.ApprovalExecuted
	}
	a, err := s.deps.Governance.Decide(ctx, p.TenantID, id, state, "user:"+p.UserID, note, s.now())
	if err != nil {
		return model.Approval{}, err
	}
	s.audit(ctx, p, "governance.approval_decide", "approval", id, map[string]any{"approved": approve, "kind": a.Kind})
	if !approve {
		return a, nil
	}
	result, err := s.execute(ctx, p, a)
	a.Result = result
	if err != nil {
		a.State, a.Result = model.ApprovalFailed, err.Error()
	}
	if finishErr := s.deps.Governance.Finish(ctx, a.ID, a.State, a.Result); finishErr != nil {
		return a, finishErr
	}
	return a, err
}

// execute carries out an approved change as the requester asked it.
func (s *Service) execute(ctx context.Context, approver model.Principal, a model.Approval) (string, error) {
	switch a.Kind {
	case model.ApprovalRetentionChange:
		var policy model.RetentionPolicy
		if err := json.Unmarshal(a.Payload, &policy); err != nil {
			return "", err
		}
		policy.Reason = policy.Reason + " (approved by user:" + approver.UserID + ")"
		stored, err := s.SetPolicy(ctx, policy)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("policy %s version %d", stored.ID, stored.Version), nil
	case model.ApprovalHoldRelease:
		var body struct {
			HoldID string `json:"hold_id"`
		}
		if err := json.Unmarshal(a.Payload, &body); err != nil {
			return "", err
		}
		if err := s.ReleaseHold(ctx, a.TenantID, body.HoldID, a.RequestedBy+" approved by user:"+approver.UserID); err != nil {
			return "", err
		}
		return "hold " + body.HoldID + " released", nil
	case model.ApprovalAssetDelete:
		var body bulkDeletePayload
		if err := json.Unmarshal(a.Payload, &body); err != nil {
			return "", err
		}
		scheduled := s.deleteAll(ctx, a.TenantID, body.AssetIDs, a.RequestedBy+" approved by user:"+approver.UserID, body.Note)
		return fmt.Sprintf("%d of %d assets scheduled for deletion", scheduled, len(body.AssetIDs)), nil
	case model.ApprovalGovernanceChange:
		var next model.GovernanceSettings
		if err := json.Unmarshal(a.Payload, &next); err != nil {
			return "", err
		}
		requester := model.Principal{TenantID: a.TenantID, UserID: strings.TrimPrefix(a.RequestedBy, "user:")}
		if _, err := s.putGovernance(ctx, requester, next); err != nil {
			return "", err
		}
		return "governance set to " + next.Profile, nil
	}
	return "", fmt.Errorf("unknown approval kind %s", a.Kind)
}

// ─── Deletions ────────────────────────────────────────────────────────

// RequestDeletionGoverned deletes an asset, or asks for approval when the
// organization approves every deletion.
func (s *Service) RequestDeletionGoverned(ctx context.Context, p model.Principal, assetID, reason string) (DeletionResult, error) {
	g, err := s.settings(ctx, p.TenantID)
	if err != nil {
		return DeletionResult{}, err
	}
	if !g.ApproveDeletes {
		return s.RequestDeletion(ctx, p, assetID)
	}
	if _, err := s.owned(ctx, p, assetID, model.PermAssetsDelete); err != nil {
		return DeletionResult{}, err
	}
	return DeletionResult{}, s.requestApproval(ctx, p, model.ApprovalAssetDelete, "delete asset "+assetID, reason,
		bulkDeletePayload{AssetIDs: []string{assetID}, Note: reason})
}

type bulkDeletePayload struct {
	AssetIDs []string `json:"asset_ids"`
	Note     string   `json:"note"`
}

// BulkDeleteInput selects assets: by IDs, or by dataset, path prefix and
// metadata together.
type BulkDeleteInput struct {
	AssetIDs []string
	Filter   model.HoldQuery
	Reason   string
	DryRun   bool
}

// BulkDeletePlan is what a bulk delete would do, or did.
type BulkDeletePlan struct {
	Matched int   `json:"matched"`
	Bytes   int64 `json:"bytes"`
	// Blocked counts assets whose deletion would be held back now, by
	// kind of reason; their deletion is scheduled and waits.
	Blocked   map[string]int `json:"blocked"`
	Checked   int            `json:"checked"`
	Scheduled int            `json:"scheduled"`
	// ApprovalID is set when the deletion waits for a second member.
	ApprovalID string   `json:"approval_id,omitempty"`
	Sample     []string `json:"sample"`
}

const bulkDeleteMax, bulkCheckMax = 10000, 1000

// BulkDeleteFor previews or schedules the deletion of many assets. A dry
// run checks up to bulkCheckMax of them for what would hold them back.
// The deletion itself waits for approval under the two-person rule.
func (s *Service) BulkDeleteFor(ctx context.Context, p model.Principal, in BulkDeleteInput) (BulkDeletePlan, error) {
	if err := audit.Require(ctx, s.security, p, model.PermAssetsDelete, "asset.bulk_delete", "asset", ""); err != nil {
		return BulkDeletePlan{}, err
	}
	if s.deps.Governance == nil {
		return BulkDeletePlan{}, errors.New("governance is not configured")
	}
	if len(in.AssetIDs) == 0 && in.Filter.Dataset == "" && in.Filter.PathPrefix == "" && len(in.Filter.Metadata) == 0 {
		return BulkDeletePlan{}, fmt.Errorf("%w: choose assets by id, dataset, path prefix or metadata", model.ErrGovernanceInvalid)
	}
	if len(in.AssetIDs) > bulkDeleteMax {
		return BulkDeletePlan{}, fmt.Errorf("%w: at most %d assets at once", model.ErrGovernanceInvalid, bulkDeleteMax)
	}
	assets, err := s.deps.Governance.FindAssets(ctx, p.TenantID, in.Filter, in.AssetIDs, bulkDeleteMax+1)
	if err != nil {
		return BulkDeletePlan{}, err
	}
	if len(assets) > bulkDeleteMax {
		return BulkDeletePlan{}, fmt.Errorf("%w: the selection has more than %d assets; narrow it", model.ErrGovernanceInvalid, bulkDeleteMax)
	}
	plan := BulkDeletePlan{Matched: len(assets), Blocked: map[string]int{}, Sample: []string{}}
	ids := make([]string, len(assets))
	for i, a := range assets {
		ids[i] = a.ID
		plan.Bytes += a.SizeBytes
		if len(plan.Sample) < 20 {
			plan.Sample = append(plan.Sample, a.ID+" "+a.FileName)
		}
	}
	if in.DryRun {
		for i, a := range assets {
			if i == bulkCheckMax {
				break
			}
			reasons, _, err := s.blockers(ctx, a)
			if err != nil {
				return plan, err
			}
			plan.Checked++
			for _, kind := range blockerKinds(reasons) {
				plan.Blocked[kind]++
			}
		}
		return plan, nil
	}
	g, err := s.settings(ctx, p.TenantID)
	if err != nil {
		return plan, err
	}
	if g.TwoPerson || g.ApproveDeletes {
		err := s.requestApproval(ctx, p, model.ApprovalAssetDelete, fmt.Sprintf("delete %d assets", len(ids)), in.Reason,
			bulkDeletePayload{AssetIDs: ids, Note: in.Reason})
		var pending *model.ApprovalRequiredError
		if errors.As(err, &pending) {
			plan.ApprovalID = pending.ID
			return plan, nil
		}
		return plan, err
	}
	plan.Scheduled = s.deleteAll(ctx, p.TenantID, ids, "user:"+p.UserID, in.Reason)
	s.audit(ctx, p, "asset.bulk_delete", "asset", "", map[string]any{"matched": plan.Matched, "scheduled": plan.Scheduled})
	return plan, nil
}

// blockerKinds names the kinds of reason in a blockers list.
func blockerKinds(reasons []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range reasons {
		kind := "other"
		switch {
		case strings.HasPrefix(r, "legal hold"):
			kind = "legal_hold"
		case strings.HasPrefix(r, "retention until"):
			kind = "retention"
		case strings.HasPrefix(r, "object lock"):
			kind = "object_lock"
		case strings.HasPrefix(r, "quarantine"):
			kind = "quarantine_evidence"
		}
		if !seen[kind] {
			seen[kind] = true
			out = append(out, kind)
		}
	}
	return out
}

// deleteAll schedules each asset's deletion; blocked ones wait in
// PROTECTED as any deletion does.
func (s *Service) deleteAll(ctx context.Context, tenantID string, ids []string, actor, note string) int {
	scheduled := 0
	for _, id := range ids {
		asset, err := s.tenantAsset(ctx, tenantID, id)
		if err != nil {
			continue
		}
		if _, err := s.requestDeletion(ctx, asset, model.DeletionReasonOperator, actor, note); err == nil {
			scheduled++
		}
	}
	return scheduled
}

// ─── Disposition certificates ─────────────────────────────────────────

// certificateBody is what a disposition certificate states. Its JSON, as
// stored, is what the signature covers.
type certificateBody struct {
	Type        string    `json:"type"`
	DeletionID  string    `json:"deletion_id"`
	TenantID    string    `json:"tenant_id"`
	AssetID     string    `json:"asset_id"`
	ObjectID    string    `json:"object_id,omitempty"`
	Version     int       `json:"version,omitempty"`
	FileName    string    `json:"file_name"`
	Dataset     string    `json:"dataset,omitempty"`
	SHA256      string    `json:"checksum_sha256"`
	SizeBytes   int64     `json:"size_bytes"`
	Connection  string    `json:"storage_connection_id"`
	ObjectKey   string    `json:"object_key"`
	StorageVer  string    `json:"storage_version_id,omitempty"`
	Reason      string    `json:"reason"`
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
	PurgeAfter  time.Time `json:"purge_after"`
	Policy      string    `json:"policy,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
	// Checked records that no legal hold, retention period or lock held
	// the asset when its bytes were removed.
	Checked string `json:"checked"`
}

// certify writes the deletion's certificate before it completes, so a
// completed deletion always has one.
func (s *Service) certify(ctx context.Context, op model.DeletionOperation) error {
	if s.deps.Governance == nil {
		return nil
	}
	asset, err := s.deps.Assets.GetByID(ctx, op.TenantID, op.AssetID)
	if err != nil {
		return err
	}
	body := certificateBody{
		Type: "disposition-certificate/v1", DeletionID: op.ID, TenantID: op.TenantID, AssetID: asset.ID,
		ObjectID: asset.ObjectID, Version: asset.Version, FileName: asset.FileName, Dataset: asset.Dataset,
		SHA256: asset.ChecksumSHA256, SizeBytes: asset.SizeBytes, Connection: asset.StorageConnectionID,
		ObjectKey: asset.ObjectKey, StorageVer: asset.StorageVersionID, Reason: op.Reason, RequestedBy: op.RequestedBy,
		RequestedAt: op.CreatedAt, PurgeAfter: op.PurgeAfter, Policy: op.PolicyDetail, CompletedAt: s.now(),
		Checked: "no active legal hold, retention period or object lock held the asset when its stored objects were removed",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	cert := model.DispositionCertificate{ID: s.newID(), TenantID: op.TenantID, DeletionID: op.ID, AssetID: asset.ID,
		Body: raw, BodySHA256: hex.EncodeToString(sum[:]), CreatedAt: body.CompletedAt}
	if s.cfg.Signer.Key != nil {
		cert.KeyID = s.cfg.Signer.KeyID
		cert.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.cfg.Signer.Key, raw))
	}
	return s.deps.Governance.PutCertificate(ctx, cert)
}

// CertificatesFor lists an asset's disposition certificates, or one
// deletion's.
func (s *Service) CertificatesFor(ctx context.Context, p model.Principal, assetID, deletionID string) ([]model.DispositionCertificate, error) {
	if err := audit.Require(ctx, s.security, p, model.PermAssetsRead, "deletion.certificate_read", "asset", assetID); err != nil {
		return nil, err
	}
	if s.deps.Governance == nil {
		return nil, nil
	}
	return s.deps.Governance.Certificates(ctx, p.TenantID, assetID, deletionID)
}

// PublicKey is the key that verifies certificates, base64; "" unsigned.
func (s *Service) PublicKey() (string, string) {
	if s.cfg.Signer.Key == nil {
		return "", ""
	}
	return s.cfg.Signer.KeyID, base64.StdEncoding.EncodeToString(s.cfg.Signer.Key.Public().(ed25519.PublicKey))
}

// Templates lists the retention templates.
func Templates() []model.RetentionTemplate { return model.RetentionTemplates }
