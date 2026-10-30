package asset

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestRetentionCountsFromAMetadataDateButNeverShorter(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := ResolvePolicy(PlatformRetention{}, &model.RetentionPolicy{Scope: model.ScopeTenant, Version: 1,
		MinRetention: duration(365 * day), RetainFromKey: "study_end_date"}, nil, nil)
	later := model.Asset{CreatedAt: created, Metadata: map[string]string{"study_end_date": "2028-06-30"}}
	if got := p.RetainUntilFor(later); !got.Equal(time.Date(2029, 6, 30, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("from the study end: %v", got)
	}
	earlier := model.Asset{CreatedAt: created, Metadata: map[string]string{"study_end_date": "2020-01-01"}}
	missing := model.Asset{CreatedAt: created}
	for _, a := range []model.Asset{earlier, missing} {
		if got := p.RetainUntilFor(a); !got.Equal(created.Add(365 * day)) {
			t.Fatalf("an earlier or missing date must not shorten retention: %v", got)
		}
	}
}

func TestComplianceAnywhereInTheChainWins(t *testing.T) {
	tenant := &model.RetentionPolicy{Scope: model.ScopeTenant, Version: 1, LockMode: model.LockCompliance, MinRetention: duration(day)}
	asset := &model.RetentionPolicy{Scope: model.ScopeAsset, Version: 1, LockMode: model.LockGovernance}
	if p := ResolvePolicy(PlatformRetention{}, tenant, nil, asset); p.LockMode != model.LockCompliance {
		t.Fatalf("mode %s", p.LockMode)
	}
	if p := ResolvePolicy(PlatformRetention{}, nil, nil, nil); p.LockMode != model.LockGovernance {
		t.Fatalf("default mode %s", p.LockMode)
	}
}

func TestLoosens(t *testing.T) {
	year, month := duration(365*day), duration(30*day)
	cases := []struct {
		prev, next model.RetentionPolicy
		want       bool
	}{
		{model.RetentionPolicy{MinRetention: year}, model.RetentionPolicy{MinRetention: month}, true},
		{model.RetentionPolicy{MinRetention: month}, model.RetentionPolicy{MinRetention: year}, false},
		{model.RetentionPolicy{MinRetention: year}, model.RetentionPolicy{}, true},
		{model.RetentionPolicy{MinRetention: year, LockMode: model.LockCompliance}, model.RetentionPolicy{MinRetention: year}, true},
		{model.RetentionPolicy{MinRetention: year}, model.RetentionPolicy{MinRetention: year, LockMode: model.LockCompliance}, false},
		{model.RetentionPolicy{MinRetention: year, RetainFromKey: "trade_date"}, model.RetentionPolicy{MinRetention: year}, true},
	}
	for i, c := range cases {
		if got := loosens(c.prev, c.next); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

func TestComplianceGuardrails(t *testing.T) {
	platform := PlatformRetention{ComplianceMaxRetention: 10 * 365 * day}
	base := model.RetentionPolicy{TenantID: "t", Scope: model.ScopeTenant, CreatedBy: "u", Reason: "r", LockMode: model.LockCompliance}
	if err := validatePolicy(platform, base); err == nil {
		t.Fatal("COMPLIANCE without a retention period")
	}
	tooLong := base
	tooLong.MinRetention = duration(50 * 365 * day)
	if err := validatePolicy(platform, tooLong); err == nil || !strings.Contains(err.Error(), "limited") {
		t.Fatalf("COMPLIANCE past the platform cap: %v", err)
	}
	ok := base
	ok.MinRetention = duration(6 * 365 * day)
	if err := validatePolicy(platform, ok); err != nil {
		t.Fatal(err)
	}
}

func TestProfilesOnlyAddRules(t *testing.T) {
	g := model.GovernanceSettings{Profile: model.ProfileSEC17a4AT}.ApplyProfile()
	if !g.TwoPerson || g.RetentionFloor < 6*365*day {
		t.Fatalf("17a-4 audit trail: %+v", g)
	}
	g = model.GovernanceSettings{Profile: model.ProfileGxPVault}.ApplyProfile()
	if !g.TwoPerson || !g.ApproveDeletes {
		t.Fatalf("GxP vault: %+v", g)
	}
	strict := model.GovernanceSettings{Profile: model.ProfileGxPVault}.ApplyProfile()
	if !strict.Weaker(model.GovernanceSettings{Profile: model.ProfileStandard}) {
		t.Fatal("dropping the GxP vault is a loosening")
	}
	if (model.GovernanceSettings{Profile: model.ProfileStandard}).Weaker(strict) {
		t.Fatal("adopting a profile is not a loosening")
	}
}

type alerts struct {
	Governance
	sent []string
}

func (a *alerts) QueueWebhook(_ context.Context, _, eventType string, _ map[string]any) error {
	a.sent = append(a.sent, eventType)
	return nil
}

// A COMPLIANCE policy locks new versions in COMPLIANCE mode and extends a
// GOVERNANCE lock still in force in its own mode, as providers require; a
// read-back that finds a lock lifted is recorded and announced.
func TestComplianceLocksAndTamperDetection(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	locker := &lockingStorage{ObjectStorage: e.storage, enabled: true, clock: &e.clock, until: map[string]time.Time{}, hold: map[string]bool{}}
	e.storageAPI = locker
	e.versioned("a1", "v1")
	locks := memLocks{}
	sent := &alerts{}
	svc := e.service("worker-a")
	svc.deps.Locks, svc.deps.Governance = locks, sent

	set := func(mode string, d time.Duration) {
		t.Helper()
		if _, err := svc.SetPolicy(ctx, model.RetentionPolicy{TenantID: "tenant-1", Scope: model.ScopeTenant, MinRetention: duration(d),
			LockMode: mode, CreatedBy: "compliance", Reason: "test", EffectiveFrom: e.clock.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	set(model.LockGovernance, 30*day)
	e.evaluate(svc)
	if locker.mode["v1"] != model.LockGovernance {
		t.Fatalf("mode %q", locker.mode["v1"])
	}
	set(model.LockCompliance, 60*day)
	e.versioned("a2", "v2")
	e.evaluate(svc)
	if locker.mode["v1"] != model.LockGovernance || !locker.until["v1"].After(e.clock.Add(59*day)) {
		t.Fatalf("a lock in force is extended in its mode: %q %v", locker.mode["v1"], locker.until["v1"])
	}
	if locker.mode["v2"] != model.LockCompliance || locks["a2"].Mode != model.LockCompliance {
		t.Fatalf("a new version gets COMPLIANCE: provider %q record %q", locker.mode["v2"], locks["a2"].Mode)
	}
	delete(locks, "a2")

	stats := &CycleStats{}
	if err := svc.VerifyLocks(ctx, stats); err != nil || stats.LocksVerified != 1 || stats.LocksTampered != 0 {
		t.Fatalf("clean verify: %+v %v", stats, err)
	}
	if locks["a1"].VerifyResult != "MATCH" {
		t.Fatalf("result %q", locks["a1"].VerifyResult)
	}
	// Someone with the bucket's keys lifts the retention out of band.
	delete(locker.until, "v1")
	stats = &CycleStats{}
	if err := svc.VerifyLocks(ctx, stats); err != nil || stats.LocksTampered != 1 {
		t.Fatalf("tamper verify: %+v %v", stats, err)
	}
	if !strings.HasPrefix(locks["a1"].VerifyResult, "MISMATCH") || len(sent.sent) == 0 || sent.sent[len(sent.sent)-1] != "object.lock_tampered" {
		t.Fatalf("not reported: %q %v", locks["a1"].VerifyResult, sent.sent)
	}
}

// Deleting an adopted asset never deletes the organization's object under
// DETACH, and deletes exactly its version under MANAGE.
func TestAdoptedAssetsDeleteTargets(t *testing.T) {
	e := newEnv(t)
	svc := e.service("worker-a")
	asset := model.Asset{ID: "a1", TenantID: "tenant-1", ObjectKey: "lab/run1/a.csv", StorageVersionID: "v7",
		StorageConnectionID: model.PlatformConnectionID, StagingConnectionID: model.PlatformConnectionID, Adoption: model.AdoptionDetach}
	targets, err := svc.targets(context.Background(), asset, model.ProcessingJob{PromotionKey: asset.ObjectKey})
	if err != nil || len(targets) != 0 {
		t.Fatalf("DETACH: %+v %v", targets, err)
	}
	asset.Adoption = model.AdoptionManage
	targets, err = svc.targets(context.Background(), asset, model.ProcessingJob{PromotionKey: asset.ObjectKey})
	if err != nil || len(targets) != 1 || targets[0].key != "lab/run1/a.csv" || targets[0].versionID != "v7" {
		t.Fatalf("MANAGE: %+v %v", targets, err)
	}
}
