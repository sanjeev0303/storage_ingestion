package asset

import (
	"errors"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

const day = 24 * time.Hour

func duration(d time.Duration) *time.Duration { return &d }

var platform = PlatformRetention{
	DefaultMinRetention:        0,
	DefaultMaxAge:              0,
	DefaultDeletionGrace:       7 * day,
	DefaultQuarantineRetention: 30 * day,
	MinRetentionFloor:          1 * day,
	DeletionGraceFloor:         time.Hour,
	QuarantineRetentionFloor:   30 * day,
}

func policy(scope string, version int, minRetention, maxAge, grace *time.Duration) *model.RetentionPolicy {
	return &model.RetentionPolicy{Scope: scope, Version: version, MinRetention: minRetention, MaxAge: maxAge, DeletionGrace: grace}
}

func TestPolicyPrecedence(t *testing.T) {
	tenant := policy(model.ScopeTenant, 3, duration(10*day), duration(90*day), duration(2*day))
	dataset := policy(model.ScopeDataset, 1, nil, duration(30*day), nil)
	asset := policy(model.ScopeAsset, 2, duration(400*day), nil, nil)

	got := ResolvePolicy(platform, tenant, dataset, asset)
	if got.MinRetention != 400*day || got.MinRetentionFrom != "ASSET v2" {
		t.Fatalf("the asset's own setting must win: %s from %s", got.MinRetention, got.MinRetentionFrom)
	}
	if got.MaxAge != 30*day || got.MaxAgeFrom != "DATASET v1" {
		t.Fatalf("the dataset must win over the tenant: %s from %s", got.MaxAge, got.MaxAgeFrom)
	}
	if got.DeletionGrace != 2*day || got.DeletionGraceFrom != "TENANT v3" {
		t.Fatalf("the tenant must win over the platform: %s from %s", got.DeletionGrace, got.DeletionGraceFrom)
	}
	if got.QuarantineRetention != 30*day || got.QuarantineRetentionFrom != "PLATFORM" {
		t.Fatalf("unset everywhere means the platform default: %s from %s", got.QuarantineRetention, got.QuarantineRetentionFrom)
	}

	none := ResolvePolicy(platform, nil, nil, nil)
	if none.MinRetention != day || none.MinRetentionFrom != "PLATFORM FLOOR" || none.MaxAge != 0 {
		t.Fatalf("no policies: %+v", none)
	}
}

func TestPlatformFloorsCannotBeUndercut(t *testing.T) {
	// Stored before the floors were raised, or written around the API: the
	// floor still wins at evaluation.
	lax := policy(model.ScopeTenant, 1, duration(0), nil, duration(0))
	got := ResolvePolicy(platform, lax, nil, nil)
	if got.MinRetention != day || got.DeletionGrace != time.Hour ||
		got.MinRetentionFrom != "PLATFORM FLOOR" || got.DeletionGraceFrom != "PLATFORM FLOOR" {
		t.Fatalf("a tenant must not go below a platform floor: %+v", got)
	}

	shortQuarantine := &model.RetentionPolicy{Scope: model.ScopeDataset, Version: 1, QuarantineRetention: duration(day)}
	if got := ResolvePolicy(platform, nil, shortQuarantine, nil); got.QuarantineRetention != 30*day {
		t.Fatalf("quarantine retention must not go below the platform floor: %s", got.QuarantineRetention)
	}

	base := model.RetentionPolicy{TenantID: "t", Scope: model.ScopeTenant, CreatedBy: "ops", Reason: "test"}
	for name, change := range map[string]func(*model.RetentionPolicy){
		"retention below floor":  func(p *model.RetentionPolicy) { p.MinRetention = duration(time.Hour) },
		"grace below floor":      func(p *model.RetentionPolicy) { p.DeletionGrace = duration(time.Minute) },
		"quarantine below floor": func(p *model.RetentionPolicy) { p.QuarantineRetention = duration(day) },
		"negative max age":       func(p *model.RetentionPolicy) { p.MaxAge = duration(-time.Hour) },
		"absurd retention":       func(p *model.RetentionPolicy) { p.MinRetention = duration(200 * 365 * day) },
		"tenant with a key":      func(p *model.RetentionPolicy) { p.ScopeKey = "x" },
		"bad dataset":            func(p *model.RetentionPolicy) { p.Scope, p.ScopeKey = model.ScopeDataset, "Bad Name" },
		"no reason":              func(p *model.RetentionPolicy) { p.Reason = "" },
	} {
		p := base
		change(&p)
		if err := validatePolicy(platform, p); !errors.Is(err, model.ErrInvalidPolicy) {
			t.Errorf("%s: expected ErrInvalidPolicy, got %v", name, err)
		}
	}
	ok := base
	ok.MinRetention, ok.MaxAge = duration(2*day), duration(0)
	if err := validatePolicy(platform, ok); err != nil {
		t.Fatalf("a valid policy was refused: %v", err)
	}
}

func TestExpirationBoundaries(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := EffectivePolicy{MinRetention: 10 * day, MaxAge: 30 * day}

	expires, ok := p.ExpiresAt(created)
	if !ok || !expires.Equal(created.Add(30*day)) {
		t.Fatalf("expires at %v %v", expires, ok)
	}
	if p.Expired(created, expires.Add(-time.Nanosecond)) {
		t.Fatal("one nanosecond before expiry is not expired")
	}
	if !p.Expired(created, expires) {
		t.Fatal("expiry is inclusive of its exact moment")
	}

	// Expiry never comes before the retention period ends.
	short := EffectivePolicy{MinRetention: 30 * day, MaxAge: 7 * day}
	if at, _ := short.ExpiresAt(created); !at.Equal(created.Add(30 * day)) {
		t.Fatalf("max age shorter than retention must wait for retention, got %v", at)
	}
	if never := (EffectivePolicy{}); never.Expired(created, created.Add(1000*day)) {
		t.Fatal("a zero max age never expires")
	}

	// The reference time is UTC whatever zone the timestamp carries.
	local := created.In(time.FixedZone("UTC+5:30", 5*3600+1800))
	if at, _ := p.ExpiresAt(local); !at.Equal(expires) || at.Location() != time.UTC {
		t.Fatalf("expiry must not depend on the time zone: %v", at)
	}
	if got := p.RetainUntil(created); !got.Equal(created.Add(10 * day)) {
		t.Fatalf("retain until %v", got)
	}
}
