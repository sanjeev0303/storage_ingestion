package integration

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Phase 10 lineage against PostgreSQL: derived datasets and the graph
// their snapshots, imports and reports leave.

// walk collects a whole traversal, page by page.
func (s *securityStack) walk(t *testing.T, p model.Principal, versionID, direction string, depth int) []model.LineageEdge {
	t.Helper()
	var all []model.LineageEdge
	cursor := ""
	for {
		page, err := s.datasets.VersionLineage(context.Background(), p, versionID, datasetservice.LineageRequest{
			Direction: direction, Depth: depth, Cursor: cursor, Limit: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Edges...)
		if page.After == "" {
			return all
		}
		cursor = page.After
	}
}

func hasEdge(edges []model.LineageEdge, from string, relation model.LineageRelation, to string) *model.LineageEdge {
	for i, e := range edges {
		if e.From.RefID == from && e.Relation == relation && e.To.RefID == to {
			return &edges[i]
		}
	}
	return nil
}

func TestDerivedDatasetLineage(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-lin-a")
	globex := s.team(t, "ds-lin-b")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "lin", 4)
	source := s.newDataset(t, dev, "source")
	s.addMembers(t, dev, source.ID, memberOf(objs[0], "cat", "train"), memberOf(objs[1], "dog", "train"),
		memberOf(objs[2], "cat", "test"), memberOf(objs[3], "dog", "validation"))
	v1, _, err := s.datasets.Snapshot(ctx, dev, source.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	_, report, _ := s.validated(t, dev, source.ID, datasetservice.ValidationInput{}, false)

	// A derived dataset: the train split of v1, through a named
	// transformation.
	derived, created, err := s.datasets.Create(ctx, dev, datasetservice.CreateInput{
		Name: "train-only", SourceVersionIDs: []string{v1.ID}, Splits: []string{"train"},
		Transformation: &datasetservice.TransformationInput{Name: "resize", Version: "1.2", Config: map[string]any{"size": 224, "mode": "crop"}},
	})
	if err != nil || !created || derived.MemberCount != 2 || derived.Derivation == nil || derived.Derivation.Transformation == nil ||
		derived.Derivation.Transformation.ConfigSHA256 == "" {
		t.Fatalf("derived: %+v %v", derived, err)
	}
	members, _ := s.datasets.Members(ctx, dev, derived.ID, "", 10)
	for _, m := range members.Items {
		if m.Split != "train" || m.Source != "derived:"+v1.ID || (m.AssetID != objs[0].ID && m.AssetID != objs[1].ID) {
			t.Fatalf("derived member: %+v", m)
		}
	}
	// A newer version of a source object does not change what was derived:
	// members are pinned.
	s.publishAt(t, dev, "lin/item-00.txt", textData("lin-0-v2", 90))
	dv1, _, err := s.datasets.Snapshot(ctx, dev, derived.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.versionMembers(t, dev, dv1) {
		if m.AssetID != objs[0].ID && m.AssetID != objs[1].ID {
			t.Fatalf("derived version follows the source's current version: %+v", m)
		}
	}

	// Upstream from the derived version: what it came from, and, a step
	// further, how the source was checked.
	up := s.walk(t, dev, dv1.ID, "upstream", 3)
	from := hasEdge(up, dv1.ID, model.RelDerivedFrom, v1.ID)
	if from == nil || from.Depth != 1 || from.Actor != dev.UserID ||
		from.Details["input_manifest_sha256"] != v1.ManifestSHA256 || from.Details["output_manifest_sha256"] != dv1.ManifestSHA256 {
		t.Fatalf("DERIVED_FROM: %+v in %+v", from, up)
	}
	if e := hasEdge(up, v1.ID, model.RelValidatedBy, report.ID); e == nil || e.Depth != 2 {
		t.Fatalf("VALIDATED_BY at depth 2: %+v", up)
	}
	transformed := false
	for _, e := range up {
		if e.From.RefID == dv1.ID && e.Relation == model.RelTransformedBy && e.To.Kind == model.NodeTransformation {
			transformed = e.Details["name"] == "resize"
		}
	}
	if !transformed {
		t.Fatalf("TRANSFORMED_BY: %+v", up)
	}
	// Downstream from the source version: who contains it and what was
	// made from it.
	down := s.walk(t, dev, v1.ID, "downstream", 2)
	if hasEdge(down, source.ID, model.RelContains, v1.ID) == nil || hasEdge(down, derived.ID, model.RelCreatedFromSnapshot, v1.ID) == nil ||
		hasEdge(down, dv1.ID, model.RelDerivedFrom, v1.ID) == nil {
		t.Fatalf("downstream: %+v", down)
	}
	// Depth bounds the walk; a one-edge page walk finds what one big page
	// does.
	if shallow := s.walk(t, dev, dv1.ID, "upstream", 1); hasEdge(shallow, v1.ID, model.RelValidatedBy, report.ID) != nil {
		t.Fatalf("depth 1 went further: %+v", shallow)
	}
	whole, err := s.datasets.VersionLineage(ctx, dev, dv1.ID, datasetservice.LineageRequest{Depth: 3, Limit: 100})
	if err != nil || len(whole.Edges) != len(up) || whole.After != "" {
		t.Fatalf("one page: %d edges, paged: %d (%v)", len(whole.Edges), len(up), err)
	}
	for _, bad := range []datasetservice.LineageRequest{{Direction: "sideways"}, {Depth: 999}, {Cursor: "%%%"}, {Limit: 100000}} {
		if _, err := s.datasets.VersionLineage(ctx, dev, dv1.ID, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}

	// Provenance cannot loop, and an edge must make sense and name real
	// records of the tenant.
	err = s.datasetDB.AddLineage(ctx, acme.org.ID, model.LineageEdge{
		From: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: v1.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: dv1.ID},
		Relation: model.RelDerivedFrom, Actor: "test",
	}, time.Now())
	if !errors.Is(err, model.ErrLineageCycle) {
		t.Fatalf("cycle: %v", err)
	}
	for _, e := range []model.LineageEdge{
		{From: model.LineageRef{Kind: model.NodeValidationReport, RefID: report.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: v1.ID}, Relation: model.RelDerivedFrom},
		{From: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: dv1.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: "missing"}, Relation: model.RelDerivedFrom},
		{From: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: dv1.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: dv1.ID}, Relation: model.RelDerivedFrom},
	} {
		if err := s.datasetDB.AddLineage(ctx, acme.org.ID, e, time.Now()); !errors.Is(err, model.ErrInvalidLineage) && !errors.Is(err, model.ErrLineageCycle) {
			t.Fatalf("%+v: %v", e, err)
		}
	}
	// The same edge twice is one edge.
	if err := s.datasetDB.AddLineage(ctx, acme.org.ID, model.LineageEdge{
		From: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: dv1.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: v1.ID},
		Relation: model.RelDerivedFrom, Actor: "again",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if again := s.walk(t, dev, dv1.ID, "upstream", 3); len(again) != len(up) {
		t.Fatalf("a repeated edge: %d edges, was %d", len(again), len(up))
	}

	// Other tenants see none of it, and cannot derive from it.
	if _, err := s.datasets.VersionLineage(ctx, globex.developer, v1.ID, datasetservice.LineageRequest{}); !errors.Is(err, model.ErrDatasetVersionNotFound) {
		t.Fatalf("cross-tenant lineage: %v", err)
	}
	if _, err := s.datasets.DatasetLineage(ctx, globex.developer, source.ID, datasetservice.LineageRequest{}); !errors.Is(err, model.ErrDatasetNotFound) {
		t.Fatalf("cross-tenant dataset lineage: %v", err)
	}
	if _, _, err := s.datasets.Create(ctx, globex.developer, datasetservice.CreateInput{Name: "stolen", SourceVersionIDs: []string{v1.ID}}); !errors.Is(err, model.ErrDatasetVersionNotFound) {
		t.Fatalf("derive from another tenant's version: %v", err)
	}
	if err := s.datasetDB.AddLineage(ctx, globex.org.ID, model.LineageEdge{
		From: model.LineageRef{Kind: model.NodeDataset, RefID: source.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: v1.ID},
		Relation: model.RelContains,
	}, time.Now()); !errors.Is(err, model.ErrInvalidLineage) {
		t.Fatalf("an edge between another tenant's records: %v", err)
	}

	// Archiving keeps provenance.
	if _, err := s.datasets.Archive(ctx, acme.admin, source.ID); err != nil {
		t.Fatal(err)
	}
	if after := s.walk(t, dev, v1.ID, "downstream", 2); len(after) != len(down) {
		t.Fatalf("lineage after archiving: %d edges, was %d", len(after), len(down))
	}
	if dl, err := s.datasets.DatasetLineage(ctx, acme.auditor, source.ID, datasetservice.LineageRequest{Depth: 1}); err != nil ||
		!slices.ContainsFunc(dl.Edges, func(e model.LineageEdge) bool { return e.Relation == model.RelContains && e.To.RefID == v1.ID }) {
		t.Fatalf("dataset lineage: %+v %v", dl, err)
	}
}
