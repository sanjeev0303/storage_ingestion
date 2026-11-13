package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// oneObject is a catalog holding a single object of dataset "other".
type oneObject struct{ interfaces.CatalogRepository }

func (oneObject) Get(_ context.Context, tenantID, objectID string) (model.CatalogObject, error) {
	return model.CatalogObject{ID: objectID, TenantID: tenantID, Dataset: "other"}, nil
}

// A token confined to some datasets sees no object outside them, and may
// only list its own datasets.
func TestScopedTokenSeesOnlyItsDatasets(t *testing.T) {
	s := &Service{deps: Dependencies{Repo: oneObject{}}, cfg: Config{DefaultPageSize: 50, MaxPageSize: 100}}
	p := model.Principal{TenantID: "t1", UserID: "u1", Permissions: model.PermAssetsRead | model.PermCatalogWrite,
		Scope: model.Scope{TokenID: "tok", Restricted: true, Datasets: []string{"scans"}}}

	if _, err := s.Get(context.Background(), p, "o1"); !errors.Is(err, model.ErrObjectNotFound) {
		t.Fatalf("get outside scope: err = %v, want ErrObjectNotFound", err)
	}
	if _, err := s.Lookup(context.Background(), p, "other", "a.txt"); !errors.Is(err, model.ErrObjectNotFound) {
		t.Fatalf("lookup outside scope: err = %v, want ErrObjectNotFound", err)
	}
	if _, err := s.filter(p, ListRequest{AnyDataset: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("list every dataset: err = %v, want ErrInvalidRequest", err)
	}
	if _, err := s.filter(p, ListRequest{Dataset: "scans"}); err != nil {
		t.Fatalf("list own dataset: %v", err)
	}

	p.Scope.Datasets = []string{"other"}
	if _, err := s.Get(context.Background(), p, "o1"); err != nil {
		t.Fatalf("get inside scope: %v", err)
	}
}
