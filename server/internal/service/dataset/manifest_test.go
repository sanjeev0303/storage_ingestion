package dataset

import (
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestDecodeManifestLines(t *testing.T) {
	cases := []struct {
		line, code string
	}{
		{`{"object_id":"o1","label":"cat","split":"train","metadata":{"a":"b"}}`, ""},
		{`{"namespace":"photos","path":"2024/cat.jpg","version":2,"schema_version":1}`, ""},
		{`{"object_id":"o1"`, model.LineInvalidJSON},
		{`["o1"]`, model.LineInvalidJSON},
		{`{"object_id":"o1"} {"object_id":"o2"}`, model.LineInvalidJSON},
		{`{"object_id":"o1","colour":"red"}`, model.LineInvalidJSON},
		{`{"object_id":"o1","tenant_id":"someone-else"}`, lineTenantField},
		{`{"object_id":"o1","schema_version":2}`, lineSchemaVersion},
		{`{"object_id":"o1","metadata":{"n":1}}`, model.LineInvalidJSON},
		{"{\"object_id\":\"o\xff\"}", model.LineInvalidUTF8},
	}
	for _, c := range cases {
		_, code, detail := decodeLine([]byte(c.line))
		if code != c.code {
			t.Errorf("%s: code %q (%s), want %q", c.line, code, detail, c.code)
		}
	}
}

func TestParseMember(t *testing.T) {
	cases := []struct {
		in   MemberInput
		code string
	}{
		{MemberInput{ObjectID: "o1", Version: 3, Split: "test"}, ""},
		{MemberInput{Path: "Photos/Cat.JPG", Namespace: "photos"}, ""},
		{MemberInput{}, model.LineMissingReference},
		{MemberInput{ObjectID: "o1", Path: "a.jpg"}, model.LineInvalidReference},
		{MemberInput{ObjectID: "o1", VersionID: "a1", Version: 2}, model.LineInvalidReference},
		{MemberInput{ObjectID: "o1", Version: -1}, model.LineInvalidReference},
		{MemberInput{Path: "../../etc/passwd"}, model.LineInvalidPath},
		{MemberInput{Path: "/abs/path.jpg"}, model.LineInvalidPath},
		{MemberInput{Path: "a\x00b.jpg"}, model.LineInvalidPath},
		{MemberInput{Path: "a.jpg", Namespace: "Bad Namespace"}, model.LineInvalidReference},
		{MemberInput{ObjectID: "o1", Split: "holdout"}, model.LineInvalidSplit},
		{MemberInput{ObjectID: "o1", Label: strings.Repeat("x", 300)}, model.LineInvalidLabel},
		{MemberInput{ObjectID: "o1", Label: "a\nb"}, model.LineInvalidLabel},
		{MemberInput{ObjectID: "o1", Metadata: map[string]string{"bad key": "v"}}, model.LineInvalidMetadata},
	}
	for _, c := range cases {
		if got := parseMember(c.in); got.code != c.code {
			t.Errorf("%+v: code %q (%s), want %q", c.in, got.code, got.detail, c.code)
		}
	}
	// A path reference is folded the catalog's way.
	p := parseMember(MemberInput{Path: "Photos/Cat.JPG"})
	if p.ref.PathKey != "photos/cat.jpg" {
		t.Fatalf("path key %q", p.ref.PathKey)
	}
}

func TestDedupeFollowsThePolicy(t *testing.T) {
	items := func() []parsed {
		out := make([]parsed, 4)
		for i, id := range []string{"a", "b", "a", "a"} {
			out[i].change.ObjectID = id
		}
		return out
	}
	keep, dropped := dedupe(items(), model.DuplicateReject)
	if len(keep) != 2 || keep[0] != 0 || keep[1] != 1 || !dropped[2] || !dropped[3] {
		t.Fatalf("reject keeps the first: %v %v", keep, dropped)
	}
	keep, dropped = dedupe(items(), model.DuplicateReplace)
	if len(keep) != 2 || keep[0] != 1 || keep[1] != 3 || !dropped[0] || !dropped[2] {
		t.Fatalf("replace keeps the last: %v %v", keep, dropped)
	}
	// Refused lines take no part.
	it := items()
	it[0].code = model.LineObjectNotFound
	keep, _ = dedupe(it, model.DuplicateSkip)
	if len(keep) != 2 || keep[0] != 1 || keep[1] != 2 {
		t.Fatalf("skip after a refused line: %v", keep)
	}
}

func TestSampleIsDeterministicAndBounded(t *testing.T) {
	job := model.ValidationJob{Mode: model.ModeSampled, SampleSize: 7, TotalItems: 100}
	var picked []int64
	for ordinal := int64(1); ordinal <= 100; ordinal++ {
		if sampled(job, ordinal) {
			picked = append(picked, ordinal)
		}
	}
	if len(picked) == 0 || len(picked) > 7 || picked[0] != 1 {
		t.Fatalf("sample: %v", picked)
	}
	small := model.ValidationJob{Mode: model.ModeSampled, SampleSize: 50, TotalItems: 10}
	for ordinal := int64(1); ordinal <= 10; ordinal++ {
		if !sampled(small, ordinal) {
			t.Fatal("a version smaller than the sample is read whole")
		}
	}
	if sampled(model.ValidationJob{Mode: model.ModeMetadata, TotalItems: 10}, 1) ||
		!sampled(model.ValidationJob{Mode: model.ModeFull, TotalItems: 10}, 7) {
		t.Fatal("metadata mode reads nothing; full mode reads everything")
	}
}

func TestCursorsRoundTrip(t *testing.T) {
	for _, key := range []string{"a", "ds-name.with_chars", "0123456789abcdef"} {
		got, err := decodeCursor(encodeCursor(key))
		if err != nil || got != key {
			t.Fatalf("%q: %q %v", key, got, err)
		}
	}
	for _, bad := range []string{"garbage", "c1.zz", "c1."} {
		if _, err := decodeCursor(bad); err == nil {
			t.Fatalf("%q decoded", bad)
		}
	}
}

func TestReadinessDimensions(t *testing.T) {
	results := []checkResult{
		{RuleID: "exists", Dimension: DimCompleteness, Severity: "ERROR", Status: CheckPassed, Checked: 10},
		{RuleID: "verified", Dimension: DimIntegrity, Severity: "WARNING", Status: CheckFailed, Checked: 10, Failed: 2},
		{RuleID: "content", Type: "content_checksum", Dimension: DimIntegrity, Severity: "ERROR", Status: CheckPartial, Checked: 3, NotAssessed: 7},
		{RuleID: "malware", Dimension: DimSecurity, Severity: "ERROR", Status: CheckFailed, Checked: 10, Failed: 1},
		{RuleID: "leak", Dimension: DimConsistency, Severity: "ERROR", Status: CheckNotAssessed},
		{RuleID: "types", Dimension: DimDistribution, Severity: "INFO", Status: CheckFailed, Checked: 1, Failed: 1},
	}
	job := model.ValidationJob{Mode: model.ModeSampled, ProfileID: "builtin:image", ProfileVersion: 1}
	v := model.DatasetVersion{Version: 2, Status: "FINALIZED", ManifestSHA256: "abc", MemberCount: 10}
	agg := model.VersionAggregates{AssetStatuses: []model.Count{{Value: "AVAILABLE", Count: 9}, {Value: "SOFT_DELETED", Count: 1}}}
	got := map[string]dimensionResult{}
	for _, d := range assess(results, job, v, agg).Dimensions {
		got[d.Dimension] = d
	}
	want := map[string]string{
		DimCompleteness:    DimensionPass,
		DimIntegrity:       DimensionWarn, // a WARNING failure, and content only sampled
		DimSecurity:        DimensionFail,
		DimConsistency:     DimensionNotAssessed,
		DimDuplication:     DimensionNotAssessed, // no check in the profile
		DimDistribution:    DimensionPass,        // INFO never changes a status
		DimReproducibility: DimensionWarn,        // one version is no longer available
		DimGovernance:      DimensionNotAssessed,
		DimAccessibility:   DimensionNotAssessed,
	}
	if len(got) != len(Dimensions) {
		t.Fatalf("dimensions: %v", got)
	}
	for dim, status := range want {
		if got[dim].Status != status {
			t.Errorf("%s: %s %v, want %s", dim, got[dim].Status, got[dim].Notes, status)
		}
	}
	if got[DimIntegrity].Coverage != "SAMPLED" {
		t.Fatalf("a sampled integrity check is labelled so: %+v", got[DimIntegrity])
	}
}
