package dataset

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// one compiles a single rule of the given type and config.
func one(t *testing.T, typ, config string) check {
	t.Helper()
	r := model.Rule{ID: "r", Version: 1, Type: typ, Severity: model.SeverityError, Enabled: true}
	if config != "" {
		r.Config = json.RawMessage(config)
	}
	checks, err := compile(model.ProfileDefinition{Rules: []model.Rule{r}})
	if err != nil {
		t.Fatalf("compile %s: %v", typ, err)
	}
	return checks[0]
}

// good is a member every per-member rule accepts.
func good() model.MemberEvidence {
	return model.MemberEvidence{
		VersionMember: model.VersionMember{Ordinal: 1, ObjectID: "obj-1", AssetID: "asset-1", ObjectVersion: 1,
			Path: "images/cat.jpg", Label: "cat", Split: "train", SizeBytes: 100, ContentType: "image/jpeg",
			ChecksumSHA256: "aa", Metadata: map[string]string{"license": "cc-by", "width": "640"}},
		ObjectExists: true, AssetStatus: model.AssetStatusAvailable, AssetSizeBytes: 100, AssetChecksum: "aa",
		ObjectKey: "published/x", VerificationStatus: "PASSED", ChecksumSource: "DECLARED", ScanResult: "CLEAN",
	}
}

func codes(issues []issue) string {
	var out []string
	for _, is := range issues {
		out = append(out, is.Code)
	}
	return strings.Join(out, ",")
}

func TestMemberRules(t *testing.T) {
	cases := []struct {
		typ, config string
		bad         func(m *model.MemberEvidence)
		want        string
	}{
		{"required_metadata", `{"keys":["license"]}`, func(m *model.MemberEvidence) { delete(m.Metadata, "license") }, "MISSING_METADATA"},
		{"governance_metadata", `{"keys":["license"]}`, func(m *model.MemberEvidence) { m.Metadata["license"] = " " }, "MISSING_GOVERNANCE_METADATA"},
		{"metadata_schema", `{"fields":{"width":{"type":"integer"},"license":{"enum":["cc-by","mit"]}}}`,
			func(m *model.MemberEvidence) { m.Metadata["width"] = "wide" }, "METADATA_SCHEMA"},
		{"metadata_schema", `{"fields":{"width":{"type":"integer"},"license":{}}}`,
			func(m *model.MemberEvidence) { m.Metadata["extra"] = "x" }, "METADATA_SCHEMA"},
		{"allowed_media_types", `{"types":["image/*"]}`, func(m *model.MemberEvidence) { m.ContentType = "video/mp4" }, "MEDIA_TYPE_NOT_ALLOWED"},
		{"allowed_extensions", `{"extensions":["jpg","png"]}`, func(m *model.MemberEvidence) { m.Path = "images/cat.exe" }, "EXTENSION_NOT_ALLOWED"},
		{"object_size", `{"min_bytes":10,"max_bytes":1000}`, func(m *model.MemberEvidence) { m.SizeBytes = 5 }, "OBJECT_TOO_SMALL"},
		{"object_size", `{"max_bytes":50}`, func(m *model.MemberEvidence) {}, "OBJECT_TOO_LARGE"},
		{"path_pattern", `{"pattern":"^images/"}`, func(m *model.MemberEvidence) { m.Path = "docs/cat.jpg" }, "PATH_PATTERN_MISMATCH"},
		{"required_label", ``, func(m *model.MemberEvidence) { m.Label = "" }, "MISSING_LABEL"},
		{"split_assignment", ``, func(m *model.MemberEvidence) { m.Split = "" }, "MISSING_SPLIT"},
		{"split_assignment", `{"allowed":["train","test"]}`, func(m *model.MemberEvidence) { m.Split = "validation" }, "SPLIT_NOT_ALLOWED"},
		{"object_exists", ``, func(m *model.MemberEvidence) { m.ObjectExists = false }, "OBJECT_MISSING"},
		{"object_exists", ``, func(m *model.MemberEvidence) { m.AssetStatus = "" }, "VERSION_MISSING"},
		{"object_exists", ``, func(m *model.MemberEvidence) { m.ObjectDeleted = true }, "OBJECT_DELETED"},
		{"lifecycle_state", ``, func(m *model.MemberEvidence) { m.AssetStatus = model.AssetStatusSoftDeleted }, "VERSION_NOT_ACCESSIBLE"},
		{"size_consistency", ``, func(m *model.MemberEvidence) { m.AssetSizeBytes = 99 }, "SIZE_INCONSISTENT"},
		{"size_consistency", ``, func(m *model.MemberEvidence) { m.AssetChecksum = "bb" }, "CHECKSUM_INCONSISTENT"},
		{"checksum_present", ``, func(m *model.MemberEvidence) { m.ChecksumSHA256 = "" }, "CHECKSUM_MISSING"},
		{"checksum_verified", ``, func(m *model.MemberEvidence) { m.VerificationStatus = "" }, "CHECKSUM_UNVERIFIED"},
		{"checksum_verified", ``, func(m *model.MemberEvidence) { m.VerificationStatus = "FAILED" }, "CHECKSUM_VERIFICATION_FAILED"},
		{"checksum_verified", `{"require_declared":true}`, func(m *model.MemberEvidence) { m.ChecksumSource = "COMPUTED" }, "CHECKSUM_NOT_DECLARED"},
		{"malware_scan", ``, func(m *model.MemberEvidence) { m.ScanResult = "INFECTED" }, "MALWARE_DETECTED"},
		{"malware_scan", ``, func(m *model.MemberEvidence) { m.ScanResult = "" }, "SCAN_MISSING"},
		{"malware_scan", `{"policy":"clean"}`, func(m *model.MemberEvidence) { m.ScanResult = "NOT_SCANNED" }, "NOT_SCANNED_CLEAN"},
		{"malware_scan", `{"policy":"scanned"}`, func(m *model.MemberEvidence) { m.ScanResult = "TIMEOUT" }, "SCAN_INCOMPLETE"},
	}
	for _, c := range cases {
		t.Run(c.typ+"/"+c.want, func(t *testing.T) {
			ch := one(t, c.typ, c.config)
			if ch.member == nil {
				t.Fatalf("%s is not a member check", c.typ)
			}
			if c.want != "OBJECT_TOO_LARGE" {
				if issues := ch.member(good()); len(issues) != 0 {
					t.Fatalf("a good member fails: %s", codes(issues))
				}
			}
			m := good()
			m.Metadata = map[string]string{"license": "cc-by", "width": "640"}
			c.bad(&m)
			issues := ch.member(m)
			if codes(issues) != c.want {
				t.Fatalf("issues %q, want %q", codes(issues), c.want)
			}
			if issues[0].ObjectID != m.ObjectID || issues[0].Key == "" {
				t.Fatalf("an issue names its member: %+v", issues[0])
			}
		})
	}
	// "scanned" accepts a scan that found nothing even when not CLEAN.
	if issues := one(t, "malware_scan", `{"policy":"scanned"}`).member(func() model.MemberEvidence {
		m := good()
		m.ScanResult = "NOT_SCANNED"
		return m
	}()); len(issues) != 0 {
		t.Fatalf("scanned policy: %s", codes(issues))
	}
	// Metadata values never appear in an issue: they may be sensitive.
	m := good()
	m.Metadata["width"] = "secret-value"
	issues := one(t, "metadata_schema", `{"fields":{"width":{"type":"integer"},"license":{}}}`).member(m)
	if len(issues) != 1 || strings.Contains(issues[0].Observed+issues[0].Message, "secret-value") {
		t.Fatalf("schema issue: %+v", issues)
	}
}

func aggregates() model.VersionAggregates {
	return model.VersionAggregates{
		Members: 10, DistinctAssets: 10, TotalBytes: 1000, MinSize: 10, MaxSize: 200,
		Splits:            []model.Count{{Value: "train", Count: 8}, {Value: "validation", Count: 1}, {Value: "test", Count: 1}},
		Labels:            []model.Count{{Value: "cat", Count: 5}, {Value: "dog", Count: 5}},
		LabelCount:        2,
		ContentTypes:      []model.Count{{Value: "image/jpeg", Count: 10}},
		VerifiedChecksums: 10,
	}
}

func TestDatasetRules(t *testing.T) {
	cases := []struct {
		typ, config string
		bad         func(a *model.VersionAggregates)
		want        string
	}{
		{"member_count", ``, func(a *model.VersionAggregates) { a.Members = 0 }, "EMPTY_DATASET"},
		{"member_count", `{"min":20}`, nil, "TOO_FEW_MEMBERS"},
		{"member_count", `{"max":5}`, nil, "TOO_MANY_MEMBERS"},
		{"total_bytes", `{"max":500}`, nil, "TOTAL_BYTES_OUT_OF_RANGE"},
		{"duplicate_references", ``, func(a *model.VersionAggregates) { a.DistinctAssets = 9 }, "DUPLICATE_REFERENCE"},
		{"duplicate_content", ``, func(a *model.VersionAggregates) {
			a.Duplicates = []model.ChecksumGroup{{Checksum: "aa", Members: 2, ObjectIDs: []string{"o1", "o2"}, Splits: []string{"train"}}}
			a.DuplicateGroups = 1
		}, "DUPLICATE_CONTENT"},
		{"split_leakage", ``, func(a *model.VersionAggregates) {
			a.Leaks = []model.ChecksumGroup{{Checksum: "aa", Members: 2, ObjectIDs: []string{"o1", "o2"}, Splits: []string{"test", "train"}}}
			a.LeakGroups = 3 // two more than listed
		}, "SPLIT_LEAKAGE,SPLIT_LEAKAGE_MORE"},
		{"split_proportions", `{"proportions":{"train":[0.5,0.7]}}`, nil, "SPLIT_PROPORTION"},
		{"required_labels", `{"labels":["cat","bird"]}`, nil, "LABEL_MISSING"},
		{"label_distribution", `{"min_per_class":6}`, nil, "CLASS_TOO_SMALL,CLASS_TOO_SMALL"},
		{"label_distribution", `{"max_class_share":0.4}`, nil, "CLASS_IMBALANCE"},
		{"file_type_distribution", `{"expected":["image/png"]}`, nil, "UNEXPECTED_FILE_TYPE"},
		{"file_type_distribution", `{"max_share":0.9}`, nil, "FILE_TYPE_DOMINANT"},
	}
	for _, c := range cases {
		t.Run(c.typ+"/"+c.want, func(t *testing.T) {
			ch := one(t, c.typ, c.config)
			if ch.dataset == nil {
				t.Fatalf("%s is not a dataset check", c.typ)
			}
			a := aggregates()
			if c.bad != nil {
				if issues, _ := ch.dataset(a); len(issues) != 0 {
					t.Fatalf("a good version fails: %s", codes(issues))
				}
				c.bad(&a)
			}
			issues, _ := ch.dataset(a)
			if codes(issues) != c.want {
				t.Fatalf("issues %q, want %q", codes(issues), c.want)
			}
		})
	}
}

func TestPerClassRulesAreNotAssessedWhenLabelsAreTruncated(t *testing.T) {
	a := aggregates()
	a.LabelCount = 5000 // more labels than the aggregates list
	for _, c := range []struct{ typ, config string }{
		{"required_labels", `{"labels":["cat"]}`},
		{"label_distribution", `{"min_per_class":1}`},
	} {
		issues, note := one(t, c.typ, c.config).dataset(a)
		if len(issues) != 0 || !strings.HasPrefix(note, notAssessedPrefix) {
			t.Fatalf("%s: %v %q", c.typ, issues, note)
		}
	}
}

func TestCompileRefusesMalformedProfiles(t *testing.T) {
	rule := func(typ, config string) model.Rule {
		return model.Rule{ID: "x", Version: 1, Type: typ, Severity: model.SeverityError, Enabled: true, Config: json.RawMessage(config)}
	}
	bad := map[string]model.ProfileDefinition{
		"no rules":         {},
		"unknown type":     {Rules: []model.Rule{rule("guess_quality", "")}},
		"unknown field":    {Rules: []model.Rule{rule("required_metadata", `{"keys":["a"],"kyes":["b"]}`)}},
		"empty keys":       {Rules: []model.Rule{rule("required_metadata", `{"keys":[]}`)}},
		"bad regexp":       {Rules: []model.Rule{rule("path_pattern", `{"pattern":"("}`)}},
		"bad split":        {Rules: []model.Rule{rule("split_assignment", `{"allowed":["holdout"]}`)}},
		"bad proportion":   {Rules: []model.Rule{rule("split_proportions", `{"proportions":{"train":[0.9,0.1]}}`)}},
		"bad policy":       {Rules: []model.Rule{rule("malware_scan", `{"policy":"maybe"}`)}},
		"bad severity":     {Rules: []model.Rule{{ID: "x", Version: 1, Type: "checksum_present", Severity: "FATAL", Enabled: true}}},
		"bad scope":        {Rules: []model.Rule{{ID: "x", Version: 1, Type: "checksum_present", Severity: model.SeverityInfo, Enabled: true, Scope: model.RuleScopeDataset}}},
		"bad id":           {Rules: []model.Rule{{ID: "Has Space", Version: 1, Type: "checksum_present", Severity: model.SeverityInfo, Enabled: true}}},
		"repeated id":      {Rules: []model.Rule{rule("checksum_present", ""), rule("object_exists", "")}},
		"negative maximum": {Rules: []model.Rule{rule("checksum_present", "")}, Thresholds: model.Thresholds{MaxErrors: -1}},
	}
	for name, def := range bad {
		if _, err := compile(def); !errors.Is(err, model.ErrInvalidProfile) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A disabled rule is checked but not run.
	off := rule("checksum_present", "")
	off.Enabled = false
	checks, err := compile(model.ProfileDefinition{Rules: []model.Rule{off, {ID: "y", Version: 1, Type: "object_exists", Severity: model.SeverityError, Enabled: true}}})
	if err != nil || len(checks) != 1 || checks[0].rule.ID != "y" {
		t.Fatalf("disabled rule: %v %v", checks, err)
	}
}

func TestBuiltinProfilesCompile(t *testing.T) {
	for _, p := range BuiltinProfiles() {
		checks, err := compile(p.Definition)
		if err != nil {
			t.Fatalf("%s: %v", p.ID, err)
		}
		if len(checks) != len(p.Definition.Rules) || p.Version != 5 || !strings.HasPrefix(p.ID, builtinPrefix) {
			t.Fatalf("%s: %d checks of %d rules", p.ID, len(checks), len(p.Definition.Rules))
		}
	}
	// Version 2 added the content checks and version 3 the storage
	// integrity rule; the versions older reports were judged by still
	// resolve unchanged.
	v1, ok := builtinProfile("builtin:image", 1)
	v2, _ := builtinProfile("builtin:image", 2)
	v3, _ := builtinProfile("builtin:image", 3)
	v4, _ := builtinProfile("builtin:image", 4)
	v5, _ := builtinProfile("builtin:image", 0)
	if !ok || v2.Version != 2 || len(v2.Definition.Rules) != len(v1.Definition.Rules)+1 ||
		v2.Definition.Rules[len(v2.Definition.Rules)-1].Type != "content_checks" {
		t.Fatalf("versions: v1 %d rules, v2 %d rules", len(v1.Definition.Rules), len(v2.Definition.Rules))
	}
	if v3.Version != 3 || len(v3.Definition.Rules) != len(v2.Definition.Rules)+1 ||
		v3.Definition.Rules[len(v3.Definition.Rules)-1].Type != "storage_integrity" {
		t.Fatalf("version 3: %d rules", len(v3.Definition.Rules))
	}
	if v4.Version != 4 || v4.Definition.Rules[len(v4.Definition.Rules)-1].Type != "residency" {
		t.Fatalf("version 4: %+v", v4.Definition.Rules)
	}
	if v5.Version != 5 || len(v5.Definition.Rules) != len(v4.Definition.Rules)+1 ||
		v5.Definition.Rules[len(v5.Definition.Rules)-1].Type != "small_files" {
		t.Fatalf("version 5: %d rules", len(v5.Definition.Rules))
	}
	if _, ok := builtinProfile("builtin:image", 6); ok {
		t.Fatal("a built-in profile version that does not exist was found")
	}
}

func TestSmallFilesWarnsWhenMostFilesAreSmallAndUnpacked(t *testing.T) {
	var c check
	if err := compileSmallFiles(&c, nil); err != nil {
		t.Fatal(err)
	}
	small := model.VersionAggregates{Members: 100, MedianSize: 40 << 10, PackedMembers: 10}
	if issues, _ := c.dataset(small); len(issues) != 1 || issues[0].Code != "SMALL_FILES_UNPACKED" {
		t.Fatalf("small and unpacked: %+v", issues)
	}
	small.PackedMembers = 60
	if issues, _ := c.dataset(small); len(issues) != 0 {
		t.Fatalf("mostly packed: %+v", issues)
	}
	large := model.VersionAggregates{Members: 100, MedianSize: 8 << 20}
	if issues, _ := c.dataset(large); len(issues) != 0 {
		t.Fatalf("large files: %+v", issues)
	}
	if err := compileSmallFiles(&c, json.RawMessage(`{"packed_share":2}`)); err == nil {
		t.Fatal("a share above 1 is refused")
	}
}
