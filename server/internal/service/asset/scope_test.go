package asset

import (
	"context"
	"errors"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// A token confined to some datasets cannot delete an asset of another
// dataset: it is not found, as in delivery.
func TestScopedTokenCannotTouchAssetsOutsideItsDatasets(t *testing.T) {
	e := newEnv(t)
	e.published("a1", e.clock)
	e.assets.Update("a1", func(a *model.Asset) { a.Dataset = "other" })
	svc := e.service("worker-a")

	scoped := owner
	scoped.Scope = model.Scope{TokenID: "tok-1", Restricted: true, Datasets: []string{"scans"}}
	if _, err := svc.RequestDeletion(context.Background(), scoped, "a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete outside scope: err = %v, want ErrNotFound", err)
	}
	if got := e.status("a1"); got != model.AssetStatusAvailable {
		t.Fatalf("status = %s, want it untouched", got)
	}

	scoped.Scope.Datasets = []string{"other"}
	if _, err := svc.RequestDeletion(context.Background(), scoped, "a1"); err != nil {
		t.Fatalf("delete inside scope: %v", err)
	}
}
