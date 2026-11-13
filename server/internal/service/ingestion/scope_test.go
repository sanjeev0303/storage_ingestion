package ingestion

import (
	"errors"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestIngestionHonoursTokenScope(t *testing.T) {
	ds := model.Dataset{ID: "d1", Name: "scans"}
	cases := []struct {
		name  string
		scope model.Scope
		ok    bool
	}{
		{"member session", model.Scope{}, true},
		{"token for this dataset", model.Scope{Restricted: true, Datasets: []string{"scans"}}, true},
		{"token for another dataset", model.Scope{Restricted: true, Datasets: []string{"other"}}, false},
		{"token confined to a path prefix", model.Scope{PathPrefix: "cam-1/"}, false},
	}
	for _, c := range cases {
		err := inScope(model.Principal{Scope: c.scope}, ds)
		if c.ok != (err == nil) || (err != nil && !errors.Is(err, model.ErrForbidden)) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}
