package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/catalogbench"
	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/catalog"
)

// seed populates synthetic published objects (catalogbench) for a tenant.
func (s *securityStack) seed(t *testing.T, tenant string, from, n int64) {
	t.Helper()
	_, err := catalogbench.Populate(context.Background(), s.db.Pool, catalogbench.Spec{
		TenantID: tenant, From: from, Objects: n, VersionsPerObject: 1, BatchSize: 5000,
		Base: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

// listAll pages through a listing and returns every object in order.
func (s *securityStack) listAll(t *testing.T, p model.Principal, req catalog.ListRequest, between func(page int)) []model.CatalogObject {
	t.Helper()
	var all []model.CatalogObject
	for page := 0; ; page++ {
		res, err := s.catalog.List(context.Background(), p, req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		all = append(all, res.Objects...)
		if res.NextCursor == "" {
			return all
		}
		req.Cursor = res.NextCursor
		if between != nil {
			between(page)
		}
	}
}

func TestCursorPaginationCoversEveryObjectOnce(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "page-a")
	globex := s.team(t, "page-b")
	s.seed(t, acme.org.ID, 0, 2500)
	s.seed(t, globex.org.ID, 0, 300) // the same paths, another tenant

	// Objects are added while the listing is paged: a live view, so they
	// may or may not appear, but nothing repeats and the order holds.
	added := false
	all := s.listAll(t, acme.developer, catalog.ListRequest{AnyDataset: true, Limit: 100}, func(page int) {
		if page == 5 && !added {
			s.seed(t, acme.org.ID, 100000, 50)
			added = true
		}
	})
	seen := map[string]bool{}
	for i, o := range all {
		if seen[o.ID] {
			t.Fatalf("object %s listed twice", o.ID)
		}
		seen[o.ID] = true
		if o.TenantID != acme.org.ID {
			t.Fatalf("another tenant's object %s in the listing", o.ID)
		}
		if i > 0 && !(all[i-1].Dataset < o.Dataset || (all[i-1].Dataset == o.Dataset && all[i-1].PathKey < o.PathKey)) {
			t.Fatalf("out of order at %d: %q then %q", i, all[i-1].PathKey, o.PathKey)
		}
	}
	original := 0
	for id := range seen {
		var n int64
		if _, err := fmt.Sscanf(id[strings.LastIndex(id, "-")+1:], "%d", &n); err == nil && n < 2500 {
			original++
		}
	}
	if original != 2500 || len(all) < 2500 || len(all) > 2550 {
		t.Fatalf("listed %d objects, %d of the original 2500", len(all), original)
	}
	// The other tenant sees its 300 and nothing else.
	theirs := s.listAll(t, globex.developer, catalog.ListRequest{AnyDataset: true, Limit: 1000}, nil)
	if len(theirs) != 300 {
		t.Fatalf("globex lists %d objects", len(theirs))
	}
}

func TestTimeOrderedPagesBreakTies(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "ties")
	// Updated times repeat every 1000 objects: 2500 objects share 1000
	// timestamps.
	s.seed(t, acme.org.ID, 0, 2500)
	all := s.listAll(t, acme.developer, catalog.ListRequest{AnyDataset: true, Order: "updated", Limit: 7}, nil)
	if len(all) != 2500 {
		t.Fatalf("listed %d of 2500", len(all))
	}
	seen := map[string]bool{}
	ties := 0
	for i, o := range all {
		if seen[o.ID] {
			t.Fatalf("object %s listed twice", o.ID)
		}
		seen[o.ID] = true
		if i == 0 {
			continue
		}
		prev := all[i-1]
		switch {
		case prev.UpdatedAt.Before(o.UpdatedAt):
		case prev.UpdatedAt.Equal(o.UpdatedAt) && prev.ID < o.ID:
			ties++
		default:
			t.Fatalf("out of order at %d: %v/%s then %v/%s", i, prev.UpdatedAt, prev.ID, o.UpdatedAt, o.ID)
		}
	}
	if ties < 1000 {
		t.Fatalf("expected ties broken by ID, saw %d", ties)
	}
}

func TestManipulatedCursorsAreRefused(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "cursor-a")
	globex := s.team(t, "cursor-b")
	s.seed(t, acme.org.ID, 0, 300)
	s.seed(t, globex.org.ID, 0, 300)

	first, err := s.catalog.List(ctx, acme.developer, catalog.ListRequest{AnyDataset: true, Limit: 50})
	if err != nil || first.NextCursor == "" {
		t.Fatal(err)
	}
	for name, req := range map[string]catalog.ListRequest{
		"garbage":         {AnyDataset: true, Cursor: "%%%not-base64"},
		"not a cursor":    {AnyDataset: true, Cursor: base64.RawURLEncoding.EncodeToString([]byte("{}"))},
		"other filters":   {AnyDataset: true, Prefix: "photos/", Cursor: first.NextCursor},
		"other order":     {AnyDataset: true, Order: "updated", Cursor: first.NextCursor},
		"other namespace": {Dataset: "", Cursor: first.NextCursor},
	} {
		if _, err := s.catalog.List(ctx, acme.developer, req); !errors.Is(err, model.ErrInvalidCursor) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Another tenant cannot continue a listing that is not theirs.
	if _, err := s.catalog.List(ctx, globex.developer, catalog.ListRequest{AnyDataset: true, Cursor: first.NextCursor}); !errors.Is(err, model.ErrInvalidCursor) {
		t.Fatalf("a cursor of another tenant: %v", err)
	}

	// A position edited by hand is still only a position: the tenant is
	// applied first, so it can only move within the caller's own objects.
	raw, _ := base64.RawURLEncoding.DecodeString(first.NextCursor)
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c["p"] = "" // from the very start
	edited, _ := json.Marshal(c)
	page, err := s.catalog.List(ctx, acme.developer, catalog.ListRequest{AnyDataset: true, Limit: 50, Cursor: base64.RawURLEncoding.EncodeToString(edited)})
	if !errors.Is(err, model.ErrInvalidCursor) {
		for _, o := range page.Objects {
			if o.TenantID != acme.org.ID {
				t.Fatalf("an edited cursor reached another tenant's object %s", o.ID)
			}
		}
	}
}

func TestPrefixAndMetadataFilters(t *testing.T) {
	s := openSecurityStack(t, nil)
	acme := s.team(t, "filters")
	const n = 1200
	s.seed(t, acme.org.ID, 0, n)

	// Expected counts come from independent SQL predicates, not from the
	// listing's own query.
	ctx := context.Background()
	want := func(where string) int {
		t.Helper()
		var c int
		if err := s.db.Pool.QueryRow(ctx, `SELECT count(*) FROM catalog_objects
			WHERE tenant_id = $1 AND current_asset_id IS NOT NULL AND `+where, acme.org.ID).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	var batch string
	if err := s.db.Pool.QueryRow(ctx, `SELECT metadata->>'batch' FROM catalog_objects
		WHERE tenant_id = $1 AND metadata->>'camera' = 'c3' LIMIT 1`, acme.org.ID).Scan(&batch); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		req   catalog.ListRequest
		where string
	}{
		{"prefix, any case", catalog.ListRequest{Prefix: "PHOTOS/2022/"}, `path LIKE 'photos/2022/%'`},
		{"prefix inside a segment", catalog.ListRequest{Prefix: "vid"}, `path LIKE 'vid%'`},
		{"metadata", catalog.ListRequest{Metadata: map[string]string{"camera": "c3"}}, `metadata->>'camera' = 'c3'`},
		{"metadata, two keys", catalog.ListRequest{Metadata: map[string]string{"camera": "c3", "batch": batch}},
			`metadata->>'camera' = 'c3' AND metadata->>'batch' = '` + batch + `'`},
		{"prefix, content type and size", catalog.ListRequest{Prefix: "docs/", ContentType: "image/jpeg", MinSize: 1100, MaxSize: 60000},
			`path LIKE 'docs/%' AND content_type = 'image/jpeg' AND size_bytes BETWEEN 1100 AND 60000`},
	}
	for _, c := range cases {
		c.req.AnyDataset, c.req.Limit = true, 37
		got := s.listAll(t, acme.developer, c.req, nil)
		if expected := want(c.where); len(got) != expected || expected == 0 {
			t.Fatalf("%s: listed %d, want %d", c.name, len(got), expected)
		}
	}
	// Filters with no result, and unknown metadata.
	if got := s.listAll(t, acme.developer, catalog.ListRequest{AnyDataset: true, Metadata: map[string]string{"camera": "nope"}}, nil); len(got) != 0 {
		t.Fatalf("no match: %d", len(got))
	}
	if _, err := s.catalog.List(ctx, acme.developer, catalog.ListRequest{AnyDataset: true, Prefix: "../x"}); !errors.Is(err, catalog.ErrInvalidRequest) {
		t.Fatalf("a traversal prefix: %v", err)
	}
}

// planUses walks an EXPLAIN (FORMAT JSON) plan: the indexes it uses on
// catalog_objects, and whether it scans the table sequentially.
func planUses(t *testing.T, plan []byte) (indexes []string, seqScan bool) {
	t.Helper()
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(plan, &doc); err != nil {
		t.Fatal(err)
	}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == "catalog_objects" && node["Node Type"] == "Seq Scan" {
			seqScan = true
		}
		if name, ok := node["Index Name"].(string); ok && strings.Contains(name, "catalog_objects") {
			indexes = append(indexes, name)
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
	}
	walk(doc[0].Plan)
	return indexes, seqScan
}

func TestCriticalListingsUseIndexes(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "plans")
	s.seed(t, acme.org.ID, 0, 20000)
	if _, err := s.db.Pool.Exec(ctx, `ANALYZE catalog_objects`); err != nil {
		t.Fatal(err)
	}
	base := model.CatalogFilter{TenantID: acme.org.ID, AnyDataset: true, Order: model.OrderByPath, Limit: 100}
	with := func(change func(f *model.CatalogFilter)) model.CatalogFilter {
		f := base
		change(&f)
		return f
	}
	cases := map[string]model.CatalogFilter{
		"first page": base,
		"continuation": with(func(f *model.CatalogFilter) {
			f.After = &model.CatalogPosition{PathKey: "logs/2023/file-000010001.txt"}
		}),
		"one namespace": with(func(f *model.CatalogFilter) { f.AnyDataset, f.Dataset = false, "" }),
		"prefix":        with(func(f *model.CatalogFilter) { f.PathPrefix = "photos/2022/" }),
		"time order":    with(func(f *model.CatalogFilter) { f.Order = model.OrderByUpdated }),
		"time range": with(func(f *model.CatalogFilter) {
			f.Order, f.UpdatedAfter = model.OrderByUpdated, time.Date(2026, 10, 1, 11, 55, 0, 0, time.UTC)
		}),
		"metadata filter": with(func(f *model.CatalogFilter) { f.Metadata = map[string]string{"batch": "42", "camera": "c2"} }),
	}
	for name, f := range cases {
		query, args, err := postgres.ListQuery(f)
		if err != nil {
			t.Fatal(err)
		}
		var plan []byte
		if err := s.db.Pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+query, args...).Scan(&plan); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		indexes, seqScan := planUses(t, plan)
		if seqScan || len(indexes) == 0 {
			t.Fatalf("%s: the listing must be index-backed; indexes %v, sequential scan %v\n%s", name, indexes, seqScan, plan)
		}
		t.Logf("%s: %v", name, indexes)
	}
}

func TestLargeCatalogPopulationIsBounded(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "bulk")

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	peak := before.HeapAlloc
	res, err := catalogbench.Populate(ctx, s.db.Pool, catalogbench.Spec{
		TenantID: acme.org.ID, Objects: 60000, VersionsPerObject: 2, BatchSize: 5000,
	}, func(int64) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		peak = max(peak, m.HeapAlloc)
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Objects != 60000 || res.Versions != 120000 || res.Batches != 12 {
		t.Fatalf("populate: %+v", res)
	}
	if grew := int64(peak) - int64(before.HeapAlloc); grew > 32<<20 {
		t.Fatalf("populating grew the heap by %d MiB; it must stay bounded by one batch", grew>>20)
	}
	var objects, versions, current int
	if err := s.db.Pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM catalog_objects WHERE tenant_id = $1),
		       (SELECT count(*) FROM assets WHERE tenant_id = $1 AND version IS NOT NULL),
		       (SELECT count(*) FROM catalog_objects WHERE tenant_id = $1 AND current_version = 2)
	`, acme.org.ID).Scan(&objects, &versions, &current); err != nil {
		t.Fatal(err)
	}
	if objects != 60000 || versions != 120000 || current != 60000 {
		t.Fatalf("rows: %d objects, %d versions, %d current", objects, versions, current)
	}
	page, err := s.catalog.List(ctx, acme.developer, catalog.ListRequest{AnyDataset: true, Prefix: "videos/2024/", Limit: 10})
	if err != nil || len(page.Objects) != 10 {
		t.Fatalf("listing the large catalog: %d %v", len(page.Objects), err)
	}
	t.Logf("populated %d objects and %d versions in %s (%d batches); heap grew %d KiB at most",
		res.Objects, res.Versions, res.Duration, res.Batches, (int64(peak)-int64(before.HeapAlloc))>>10)
}
