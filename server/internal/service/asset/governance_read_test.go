package asset

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestPoliciesAndHoldsCanBeReadBack(t *testing.T) {
	e := newEnv(t)
	svc := e.service("worker-a")
	ctx := context.Background()
	admin := member("tenant-1", "user-2", model.RoleAdmin)
	month, year := 30*day, 365*day

	for _, p := range []model.RetentionPolicy{
		{Scope: model.ScopeTenant, MaxAge: &year, Reason: "default"},
		{Scope: model.ScopeDataset, ScopeKey: "pets", MinRetention: &month, Reason: "first"},
		{Scope: model.ScopeDataset, ScopeKey: "pets", MinRetention: &year, Reason: "second"},
	} {
		if _, err := svc.SetPolicyFor(ctx, admin, p); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := svc.PoliciesFor(ctx, admin)
	if err != nil || len(latest) != 2 {
		t.Fatalf("one policy per scope: %+v %v", latest, err)
	}
	if latest[0].Scope != model.ScopeDataset || latest[0].Version != 2 || latest[0].Reason != "second" {
		t.Fatalf("the newest version of the dataset's policy: %+v", latest[0])
	}
	history, err := svc.PolicyHistoryFor(ctx, admin, model.ScopeDataset, "pets")
	if err != nil || len(history) != 2 || history[0].Version != 2 || history[1].Version != 1 {
		t.Fatalf("every version, newest first: %+v %v", history, err)
	}

	kept, err := svc.PlaceHoldFor(ctx, admin, model.LegalHold{Scope: model.ScopeDataset, ScopeKey: "pets", Reason: "litigation"})
	if err != nil {
		t.Fatal(err)
	}
	released, err := svc.PlaceHoldFor(ctx, admin, model.LegalHold{Scope: model.ScopeTenant, Reason: "audit"})
	if err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(time.Hour)
	if err := svc.ReleaseHoldFor(ctx, admin, released.ID); err != nil {
		t.Fatal(err)
	}
	active, err := svc.HoldsFor(ctx, admin, false)
	if err != nil || len(active) != 1 || active[0].ID != kept.ID {
		t.Fatalf("only the active hold: %+v %v", active, err)
	}
	all, err := svc.HoldsFor(ctx, admin, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("released holds on request: %+v %v", all, err)
	}

	other := member("tenant-2", "user-3", model.RoleAdmin)
	if policies, _ := svc.PoliciesFor(ctx, other); len(policies) != 0 {
		t.Fatalf("another organization sees none of these policies: %+v", policies)
	}
	if holds, _ := svc.HoldsFor(ctx, other, true); len(holds) != 0 {
		t.Fatalf("another organization sees none of these holds: %+v", holds)
	}

	developer := member("tenant-1", "user-4", model.RoleDeveloper)
	if _, err := svc.HoldsFor(ctx, developer, false); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("hold reasons need retention.manage, got %v", err)
	}
	if _, err := svc.PoliciesFor(ctx, developer); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("policies need retention.manage, got %v", err)
	}
}
