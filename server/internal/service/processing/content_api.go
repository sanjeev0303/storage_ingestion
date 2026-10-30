package processing

import (
	"context"
	"errors"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ErrChecksNotConfigured: the API was started without content checks.
var ErrChecksNotConfigured = errors.New("content checks are not configured")

// FindingsFor lists what content checks found in an asset, for a member
// who can read assets.
func (q *Quarantine) FindingsFor(ctx context.Context, p model.Principal, assetID string) ([]model.ContentFinding, error) {
	if q.checks == nil {
		return nil, ErrChecksNotConfigured
	}
	if err := audit.Require(ctx, q.audit, p, model.PermAssetsRead, "asset.findings", "asset", assetID); err != nil {
		return nil, err
	}
	if _, err := q.assets.GetByID(ctx, p.TenantID, assetID); err != nil {
		return nil, err
	}
	return q.checks.ListFindings(ctx, p.TenantID, assetID)
}

// BlastRadiusFor shows where an asset's bytes went. It names who received
// download links, so it needs audit.read, and reading it is audited.
func (q *Quarantine) BlastRadiusFor(ctx context.Context, p model.Principal, assetID string) (model.BlastRadius, error) {
	if q.checks == nil {
		return model.BlastRadius{}, ErrChecksNotConfigured
	}
	if err := audit.Require(ctx, q.audit, p, model.PermAuditRead, "asset.blast_radius", "asset", assetID); err != nil {
		return model.BlastRadius{}, err
	}
	out, err := q.checks.BlastRadius(ctx, p.TenantID, assetID, 500)
	if err == nil && q.audit != nil {
		q.audit.Record(ctx, audit.From(ctx, p, "asset.blast_radius", "asset", assetID))
	}
	return out, err
}

// ContentPolicyFor returns the organization's own content policy (empty
// when it has none: the platform's applies).
func (q *Quarantine) ContentPolicyFor(ctx context.Context, p model.Principal) (model.ContentPolicy, error) {
	if q.checks == nil {
		return model.ContentPolicy{}, ErrChecksNotConfigured
	}
	if err := audit.Require(ctx, q.audit, p, model.PermOrgRead, "content_policy.read", "organization", p.TenantID); err != nil {
		return model.ContentPolicy{}, err
	}
	policy, _, err := q.checks.GetPolicy(ctx, p.TenantID)
	return policy, err
}

// SetContentPolicyFor replaces the organization's content policy. It can
// only narrow the platform's: processing merges the two.
func (q *Quarantine) SetContentPolicyFor(ctx context.Context, p model.Principal, policy model.ContentPolicy) (model.ContentPolicy, error) {
	if q.checks == nil {
		return model.ContentPolicy{}, ErrChecksNotConfigured
	}
	if err := audit.Require(ctx, q.audit, p, model.PermOrgManage, "content_policy.update", "organization", p.TenantID); err != nil {
		return model.ContentPolicy{}, err
	}
	if err := validPolicy(policy); err != nil {
		return model.ContentPolicy{}, err
	}
	if err := q.checks.PutPolicy(ctx, p.TenantID, policy, q.now()); err != nil {
		return model.ContentPolicy{}, err
	}
	if q.audit != nil {
		q.audit.Record(ctx, audit.From(ctx, p, "content_policy.update", "organization", p.TenantID))
	}
	return policy, nil
}

// ErrInvalidContentPolicy wraps what is wrong with a submitted policy.
var ErrInvalidContentPolicy = errors.New("invalid content policy")

func validPolicy(p model.ContentPolicy) error {
	bad := func(detail string) error { return errors.Join(ErrInvalidContentPolicy, errors.New(detail)) }
	for _, list := range [][]string{p.AllowedTypes, p.DeniedTypes} {
		if len(list) > 200 {
			return bad("at most 200 types in a list")
		}
		for _, t := range list {
			family, sub, ok := strings.Cut(t, "/")
			if !ok || family == "" || sub == "" || len(t) > 100 || strings.ContainsAny(t, " ;,") {
				return bad("type " + t + " is not like image/png or image/*")
			}
		}
	}
	if p.MaxSizeBytes < 0 {
		return bad("max_size_bytes cannot be negative")
	}
	if p.UnknownTypeAction != "" && p.UnknownTypeAction != "allow" && p.UnknownTypeAction != "reject" {
		return bad("unknown_type_action must be allow or reject")
	}
	if err := model.ValidateFindingActions(p.FindingActions); err != nil {
		return errors.Join(ErrInvalidContentPolicy, err)
	}
	return nil
}
