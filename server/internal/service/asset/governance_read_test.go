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

func TestHoldsArePagedNewestFirst(t *testing.T) {
	e := newEnv(t)
	svc := e.service("worker-a")
	ctx := context.Background()
	admin := member("tenant-1", "user-2", model.RoleAdmin)

	var placed []model.LegalHold
	for _, reason := range []string{"first", "second", "third"} {
		h, err := svc.PlaceHoldFor(ctx, admin, model.LegalHold{Scope: model.ScopeTenant, Reason: reason})
		if err != nil {
			t.Fatal(err)
		}
		placed = append(placed, h)
		e.clock = e.clock.Add(time.Minute)
	}
	if err := svc.ReleaseHoldFor(ctx, admin, placed[1].ID); err != nil {
		t.Fatal(err)
	}

	page, next, err := svc.HoldPageFor(ctx, admin, true, "", 2)
	if err != nil || len(page) != 2 || page[0].ID != placed[2].ID || page[1].ID != placed[1].ID || next == "" {
		t.Fatalf("first page: %+v %q %v", page, next, err)
	}
	page, next, err = svc.HoldPageFor(ctx, admin, true, next, 2)
	if err != nil || len(page) != 1 || page[0].ID != placed[0].ID || next != "" {
		t.Fatalf("last page: %+v %q %v", page, next, err)
	}

	// Released holds stay out unless asked for, and paging skips none.
	page, next, err = svc.HoldPageFor(ctx, admin, false, "", 1)
	if err != nil || len(page) != 1 || page[0].ID != placed[2].ID || next == "" {
		t.Fatalf("active first page: %+v %q %v", page, next, err)
	}
	page, next, err = svc.HoldPageFor(ctx, admin, false, next, 1)
	if err != nil || len(page) != 1 || page[0].ID != placed[0].ID || next != "" {
		t.Fatalf("active last page: %+v %q %v", page, next, err)
	}

	for _, bad := range []string{"%%%", "bm90LWEtY3Vyc29y"} {
		if _, _, err := svc.HoldPageFor(ctx, admin, true, bad, 2); !errors.Is(err, model.ErrInvalidCursor) {
			t.Fatalf("cursor %q: %v", bad, err)
		}
	}
	developer := member("tenant-1", "user-4", model.RoleDeveloper)
	if _, _, err := svc.HoldPageFor(ctx, developer, false, "", 0); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("hold reasons need retention.manage, got %v", err)
	}
}
