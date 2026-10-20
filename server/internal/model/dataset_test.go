package model

import (
	"bytes"
	"slices"
	"testing"
)

func TestDatasetTransitions(t *testing.T) {
	allowed := []struct{ from, to DatasetLifecycle }{
		{LifecycleDraft, LifecycleValidating},
		{LifecycleDraft, LifecycleArchived},
		{LifecycleValidating, LifecycleReady},
		{LifecycleValidating, LifecycleReadyWithWarnings},
		{LifecycleValidating, LifecycleFailed},
		{LifecycleValidating, LifecycleDraft},
		{LifecycleReady, LifecycleDraft},
		{LifecycleReady, LifecycleValidating},
		{LifecycleFailed, LifecycleArchived},
	}
	for _, tr := range allowed {
		if !CanTransitionDataset(tr.from, tr.to) {
			t.Errorf("%s -> %s should be allowed", tr.from, tr.to)
		}
	}
	refused := []struct{ from, to DatasetLifecycle }{
		{LifecycleDraft, LifecycleReady},
		{LifecycleDraft, LifecycleFailed},
		{LifecycleValidating, LifecycleArchived},
		{LifecycleArchived, LifecycleDraft},
		{LifecycleArchived, LifecycleValidating},
		{LifecycleReady, LifecycleFailed},
		{"BOGUS", LifecycleDraft},
	}
	for _, tr := range refused {
		if CanTransitionDataset(tr.from, tr.to) {
			t.Errorf("%s -> %s should be refused", tr.from, tr.to)
		}
	}
	// The SQL guard for archiving lists exactly the statuses that may archive.
	if got := TransitionSources(LifecycleArchived); !slices.Equal(got, []string{"DRAFT", "FAILED", "READY", "READY_WITH_WARNINGS"}) {
		t.Fatalf("archive sources: %v", got)
	}
	if !LifecycleArchived.Valid() || DatasetLifecycle("OPEN").Valid() {
		t.Fatal("validity of statuses")
	}
}

func TestThresholdOutcome(t *testing.T) {
	five := int64(5)
	cases := []struct {
		th               Thresholds
		errors, warnings int64
		want             DatasetLifecycle
	}{
		{Thresholds{}, 0, 0, LifecycleReady},
		{Thresholds{}, 0, 3, LifecycleReadyWithWarnings},
		{Thresholds{}, 1, 0, LifecycleFailed},
		{Thresholds{MaxErrors: 2}, 2, 0, LifecycleReadyWithWarnings},
		{Thresholds{MaxErrors: 2}, 3, 0, LifecycleFailed},
		{Thresholds{MaxWarnings: &five}, 0, 5, LifecycleReadyWithWarnings},
		{Thresholds{MaxWarnings: &five}, 0, 6, LifecycleFailed},
	}
	for _, c := range cases {
		if got := c.th.Outcome(c.errors, c.warnings); got != c.want {
			t.Errorf("%+v with %d errors, %d warnings: %s, want %s", c.th, c.errors, c.warnings, got, c.want)
		}
	}
}

func TestCanonicalLineIsDeterministic(t *testing.T) {
	m := VersionMember{Ordinal: 1, ObjectID: "o1", AssetID: "a1", ObjectVersion: 2, Path: "img/<cat>.jpg", SizeBytes: 10,
		ContentType: "image/jpeg", ChecksumSHA256: "ab", Label: "cat", Split: "train",
		Metadata: map[string]string{"z": "1", "a": "2", "m": "3"}}
	first := m.CanonicalLine()
	for range 20 {
		if again := m.CanonicalLine(); !bytes.Equal(again, first) {
			t.Fatalf("lines differ:\n%s\n%s", first, again)
		}
	}
	want := `{"object_id":"o1","version_id":"a1","version":2,"path":"img/\u003ccat\u003e.jpg","size_bytes":10,` +
		`"content_type":"image/jpeg","sha256":"ab","label":"cat","split":"train","metadata":{"a":"2","m":"3","z":"1"}}` + "\n"
	if string(first) != want {
		t.Fatalf("canonical line:\n%s\nwant\n%s", first, want)
	}
	// No metadata is an empty object, not null.
	empty := VersionMember{ObjectID: "o"}.CanonicalLine()
	if !bytes.Contains(empty, []byte(`"metadata":{}`)) {
		t.Fatalf("empty metadata: %s", empty)
	}
}

func TestLineageRules(t *testing.T) {
	if !ValidLineage(RelDerivedFrom, NodeDatasetVersion, NodeDatasetVersion) ||
		!ValidLineage(RelContains, NodeDataset, NodeDatasetVersion) ||
		!ValidLineage(RelValidatedBy, NodeDatasetVersion, NodeValidationReport) ||
		!ValidLineage(RelImportedFrom, NodeDataset, NodeManifestImport) {
		t.Fatal("expected relations are refused")
	}
	if ValidLineage(RelDerivedFrom, NodeDataset, NodeDatasetVersion) || ValidLineage(RelValidatedBy, NodeValidationReport, NodeDatasetVersion) ||
		ValidLineage("OWNS", NodeDataset, NodeDataset) {
		t.Fatal("invalid relations are accepted")
	}
	if !RelDerivedFrom.Acyclic() || !RelCreatedFromSnapshot.Acyclic() || RelValidatedBy.Acyclic() {
		t.Fatal("acyclic relations")
	}
}
