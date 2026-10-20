package asset

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// maxPolicyDuration bounds every policy setting (about 100 years), so
// adding one to a timestamp can never overflow.
const maxPolicyDuration = 100 * 365 * 24 * time.Hour

// PlatformRetention is the platform's part of every policy: the defaults
// a tenant without its own settings gets, and the floors no tenant,
// dataset or asset policy can go below.
type PlatformRetention struct {
	DefaultMinRetention        time.Duration
	DefaultMaxAge              time.Duration // zero: assets never expire
	DefaultDeletionGrace       time.Duration
	DefaultQuarantineRetention time.Duration

	MinRetentionFloor        time.Duration
	DeletionGraceFloor       time.Duration
	QuarantineRetentionFloor time.Duration
}

// EffectivePolicy is what applies to one asset at one moment, and where
// each setting came from ("PLATFORM", "TENANT v2", "DATASET v1",
// "ASSET v3", or "PLATFORM FLOOR" when a floor raised it).
type EffectivePolicy struct {
	MinRetention        time.Duration
	MaxAge              time.Duration
	DeletionGrace       time.Duration
	QuarantineRetention time.Duration

	MinRetentionFrom        string
	MaxAgeFrom              string
	DeletionGraceFrom       string
	QuarantineRetentionFrom string
}

// ResolvePolicy applies the precedence rules. For each setting:
//
//  1. the most specific policy that sets it wins: asset, then dataset,
//     then tenant, then the platform default;
//  2. a mandatory platform floor raises a value below it.
//
// Legal holds are not settings: an active hold blocks deletion whatever
// the policy says. Any of tenant, dataset and asset may be nil.
func ResolvePolicy(platform PlatformRetention, tenant, dataset, asset *model.RetentionPolicy) EffectivePolicy {
	levels := []*model.RetentionPolicy{asset, dataset, tenant}
	pick := func(setting func(*model.RetentionPolicy) *time.Duration, fallback time.Duration) (time.Duration, string) {
		for _, level := range levels {
			if level == nil {
				continue
			}
			if value := setting(level); value != nil {
				return *value, fmt.Sprintf("%s v%d", level.Scope, level.Version)
			}
		}
		return fallback, "PLATFORM"
	}
	floor := func(value time.Duration, from string, minimum time.Duration) (time.Duration, string) {
		if value < minimum {
			return minimum, "PLATFORM FLOOR"
		}
		return value, from
	}

	var p EffectivePolicy
	p.MinRetention, p.MinRetentionFrom = pick(func(r *model.RetentionPolicy) *time.Duration { return r.MinRetention }, platform.DefaultMinRetention)
	p.MinRetention, p.MinRetentionFrom = floor(p.MinRetention, p.MinRetentionFrom, platform.MinRetentionFloor)

	p.MaxAge, p.MaxAgeFrom = pick(func(r *model.RetentionPolicy) *time.Duration { return r.MaxAge }, platform.DefaultMaxAge)

	p.DeletionGrace, p.DeletionGraceFrom = pick(func(r *model.RetentionPolicy) *time.Duration { return r.DeletionGrace }, platform.DefaultDeletionGrace)
	p.DeletionGrace, p.DeletionGraceFrom = floor(p.DeletionGrace, p.DeletionGraceFrom, platform.DeletionGraceFloor)

	p.QuarantineRetention, p.QuarantineRetentionFrom = pick(func(r *model.RetentionPolicy) *time.Duration { return r.QuarantineRetention }, platform.DefaultQuarantineRetention)
	p.QuarantineRetention, p.QuarantineRetentionFrom = floor(p.QuarantineRetention, p.QuarantineRetentionFrom, platform.QuarantineRetentionFloor)
	return p
}

// RetainUntil is the earliest moment the asset may be permanently
// deleted. Every reference time is a server-side UTC timestamp.
func (p EffectivePolicy) RetainUntil(createdAt time.Time) time.Time {
	return createdAt.UTC().Add(p.MinRetention)
}

// ExpiresAt is when the asset expires, if it does. Expiry never comes
// before the end of the retention period.
func (p EffectivePolicy) ExpiresAt(createdAt time.Time) (time.Time, bool) {
	if p.MaxAge <= 0 {
		return time.Time{}, false
	}
	age := max(p.MaxAge, p.MinRetention)
	return createdAt.UTC().Add(age), true
}

// Expired reports whether the asset has expired at now: expiry is
// inclusive of its exact moment.
func (p EffectivePolicy) Expired(createdAt, now time.Time) bool {
	at, ok := p.ExpiresAt(createdAt)
	return ok && !now.Before(at)
}

// validatePolicy checks a new policy version against the platform floors,
// so a tenant can never store a setting less restrictive than a mandatory
// platform rule.
func validatePolicy(platform PlatformRetention, policy model.RetentionPolicy) error {
	switch policy.Scope {
	case model.ScopeTenant:
		if policy.ScopeKey != "" {
			return fmt.Errorf("%w: a tenant policy has no scope key", model.ErrInvalidPolicy)
		}
	case model.ScopeDataset:
		if !model.ValidDataset(policy.ScopeKey) {
			return fmt.Errorf("%w: invalid dataset name", model.ErrInvalidPolicy)
		}
	case model.ScopeAsset:
		if policy.ScopeKey == "" || len(policy.ScopeKey) > 200 {
			return fmt.Errorf("%w: an asset policy needs the asset ID", model.ErrInvalidPolicy)
		}
	default:
		return fmt.Errorf("%w: scope must be TENANT, DATASET or ASSET", model.ErrInvalidPolicy)
	}
	if policy.TenantID == "" || policy.CreatedBy == "" || policy.Reason == "" {
		return fmt.Errorf("%w: tenant, actor and reason are required", model.ErrInvalidPolicy)
	}

	settings := []struct {
		name    string
		value   *time.Duration
		minimum time.Duration
	}{
		{"min retention", policy.MinRetention, platform.MinRetentionFloor},
		{"max age", policy.MaxAge, 0},
		{"deletion grace", policy.DeletionGrace, platform.DeletionGraceFloor},
		{"quarantine retention", policy.QuarantineRetention, platform.QuarantineRetentionFloor},
	}
	for _, s := range settings {
		if s.value == nil {
			continue
		}
		if *s.value < 0 || *s.value > maxPolicyDuration {
			return fmt.Errorf("%w: %s must be between 0 and 100 years", model.ErrInvalidPolicy, s.name)
		}
		if *s.value < s.minimum {
			return fmt.Errorf("%w: %s %s is below the platform minimum of %s", model.ErrInvalidPolicy, s.name, *s.value, s.minimum)
		}
	}
	if policy.QuarantineRetention != nil && *policy.QuarantineRetention == 0 {
		return fmt.Errorf("%w: quarantine retention must be positive", model.ErrInvalidPolicy)
	}
	return nil
}

// PolicyResolver resolves the policy in effect for an asset. Other
// services use it without the rest of the lifecycle.
type PolicyResolver struct {
	policies interfaces.RetentionPolicyRepository
	platform PlatformRetention
}

func NewPolicyResolver(policies interfaces.RetentionPolicyRepository, platform PlatformRetention) PolicyResolver {
	return PolicyResolver{policies: policies, platform: platform}
}

// For resolves the asset's tenant, dataset and own policies in effect at
// asOf.
func (r PolicyResolver) For(ctx context.Context, asset model.Asset, asOf time.Time) (EffectivePolicy, error) {
	tenant, err := r.at(ctx, asset.TenantID, model.ScopeTenant, "", asOf)
	if err != nil {
		return EffectivePolicy{}, err
	}
	var dataset *model.RetentionPolicy
	if asset.Dataset != "" {
		if dataset, err = r.at(ctx, asset.TenantID, model.ScopeDataset, asset.Dataset, asOf); err != nil {
			return EffectivePolicy{}, err
		}
	}
	own, err := r.at(ctx, asset.TenantID, model.ScopeAsset, asset.ID, asOf)
	if err != nil {
		return EffectivePolicy{}, err
	}
	return ResolvePolicy(r.platform, tenant, dataset, own), nil
}

// QuarantineRetention is how long the asset's rejected upload is kept.
func (r PolicyResolver) QuarantineRetention(ctx context.Context, asset model.Asset) (time.Duration, error) {
	policy, err := r.For(ctx, asset, time.Now().UTC())
	return policy.QuarantineRetention, err
}

func (r PolicyResolver) at(ctx context.Context, tenantID, scope, key string, asOf time.Time) (*model.RetentionPolicy, error) {
	policy, ok, err := r.policies.Effective(ctx, tenantID, scope, key, asOf)
	if err != nil {
		return nil, fmt.Errorf("load %s policy: %w", strings.ToLower(scope), err)
	}
	if !ok {
		return nil, nil
	}
	return &policy, nil
}
