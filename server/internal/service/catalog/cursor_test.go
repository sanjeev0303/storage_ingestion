package catalog

import (
	"errors"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestCursorRoundTripAndBinding(t *testing.T) {
	f := model.CatalogFilter{TenantID: "t1", AnyDataset: true, PathPrefix: "photos/", Order: model.OrderByPath, Limit: 50}
	pos := model.CatalogPosition{Dataset: "", PathKey: "photos/2024/a.txt"}
	token := encodeCursor(f, pos)

	// The page size may change between pages; the position survives.
	f.Limit = 100
	got, err := decodeCursor(token, f)
	if err != nil || got != pos {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for name, other := range map[string]model.CatalogFilter{
		"another tenant": {TenantID: "t2", AnyDataset: true, PathPrefix: "photos/", Order: model.OrderByPath},
		"another prefix": {TenantID: "t1", AnyDataset: true, PathPrefix: "docs/", Order: model.OrderByPath},
		"another order":  {TenantID: "t1", AnyDataset: true, PathPrefix: "photos/", Order: model.OrderByUpdated},
		"one namespace":  {TenantID: "t1", PathPrefix: "photos/", Order: model.OrderByPath},
		"metadata added": {TenantID: "t1", AnyDataset: true, PathPrefix: "photos/", Order: model.OrderByPath, Metadata: map[string]string{"a": "b"}},
	} {
		if _, err := decodeCursor(token, other); !errors.Is(err, model.ErrInvalidCursor) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, bad := range []string{"", "!!", "e30", "bm90IGpzb24"} {
		if _, err := decodeCursor(bad, f); !errors.Is(err, model.ErrInvalidCursor) {
			t.Fatalf("%q: %v", bad, err)
		}
	}

	// Time order needs both the time and the ID.
	byTime := model.CatalogFilter{TenantID: "t1", AnyDataset: true, Order: model.OrderByUpdated}
	if _, err := decodeCursor(encodeCursor(byTime, model.CatalogPosition{UpdatedAt: time.Now()}), byTime); !errors.Is(err, model.ErrInvalidCursor) {
		t.Fatalf("incomplete time position: %v", err)
	}
	full := model.CatalogPosition{UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC), ID: "o1"}
	if got, err := decodeCursor(encodeCursor(byTime, full), byTime); err != nil || !got.UpdatedAt.Equal(full.UpdatedAt) || got.ID != "o1" {
		t.Fatalf("time position: %+v %v", got, err)
	}
}

func TestPrefixKey(t *testing.T) {
	for raw, want := range map[string]string{"Photos/2024/": "photos/2024/", "CAFÉ/": "café/", "a": "a"} {
		if got, err := prefixKey(raw); err != nil || got != want {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	for _, bad := range []string{"/abs", "a/../b", `a\b`, "a\x00", "\xff"} {
		if _, err := prefixKey(bad); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}
