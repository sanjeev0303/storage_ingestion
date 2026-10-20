package postgres

import (
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestPrefixUpperBound(t *testing.T) {
	for prefix, want := range map[string]string{
		"photos/":     "photos0", // '/' + 1
		"a":           "b",
		"café":        "cafê",
		"x\U0010FFFF": "y", // the last code point cannot grow: the one before does
		"퟿":           "", // the surrogates are skipped
	} {
		got, ok := prefixUpperBound(prefix)
		if !ok || got != want {
			t.Errorf("%q: %q %v, want %q", prefix, got, ok, want)
		}
		// Every string that starts with the prefix sorts below the bound.
		if !(prefix+"zzz" < got) || !(prefix < got) {
			t.Errorf("%q: %q is not an upper bound", prefix, got)
		}
	}
	if _, ok := prefixUpperBound("\U0010FFFF"); ok {
		t.Error("a prefix of only the last code point has no bound")
	}
}

func TestListQueryAppliesTheTenantFirstAndReadsOneMore(t *testing.T) {
	query, args, err := listQuery(model.CatalogFilter{
		TenantID: "t1", Dataset: "ds", PathPrefix: "a/", Metadata: map[string]string{"k": "v"},
		After: &model.CatalogPosition{Dataset: "ds", PathKey: "a/b"}, Order: model.OrderByPath, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "WHERE o.tenant_id = $1 AND") || args[0] != "t1" || args[len(args)-1] != 11 {
		t.Fatalf("tenant first, limit+1: %s %v", query, args)
	}
	for _, want := range []string{"o.current_asset_id IS NOT NULL", "o.dataset = $2", "o.path_key >= $3", "o.path_key < $4",
		"o.metadata @> $5::jsonb", "(o.dataset, o.path_key) > ($6, $7)", "ORDER BY o.dataset, o.path_key"} {
		if !strings.Contains(query, want) {
			t.Fatalf("missing %q in %s", want, query)
		}
	}
	if strings.Contains(query, "OFFSET") {
		t.Fatal("listings never use OFFSET")
	}
}
