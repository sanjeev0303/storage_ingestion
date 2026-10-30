package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

// Phase 10 validation and readiness against PostgreSQL and in-memory
// storage: the worker's member pass, content checks, dataset-level
// checks, reports and findings.

func ruleOf(id, typ string, severity model.Severity, config string) model.Rule {
	r := model.Rule{ID: id, Version: 1, Type: typ, Severity: severity, Enabled: true}
	if config != "" {
		r.Config = json.RawMessage(config)
	}
	return r
}

func (s *securityStack) profileOf(t *testing.T, p model.Principal, name string, maxErrors int64, rules ...model.Rule) model.ValidationProfile {
	t.Helper()
	prof, err := s.datasets.CreateProfile(context.Background(), p, datasetservice.ProfileInput{
		Name: name, Definition: model.ProfileDefinition{Kind: "test", Rules: rules, Thresholds: model.Thresholds{MaxErrors: maxErrors}},
	})
	if err != nil {
		t.Fatalf("profile %s: %v", name, err)
	}
	return prof
}

// reportView is the part of a report body the tests read.
type reportView struct {
	SchemaVersion  int    `json:"schema_version"`
	Status         string `json:"status"`
	ManifestSHA256 string `json:"manifest_sha256"`
	DatasetVersion int    `json:"dataset_version"`
	Mode           string `json:"mode"`
	Profile        struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"profile"`
	Members struct {
		Total    int64 `json:"total"`
		Examined int64 `json:"examined"`
		Passed   int64 `json:"passed"`
		Warnings int64 `json:"warnings"`
		Failed   int64 `json:"failed"`
	} `json:"members"`
	Content struct {
		Coverage    string `json:"coverage"`
		Examined    int64  `json:"examined"`
		Read        int64  `json:"read"`
		NotExamined int64  `json:"not_examined"`
		BytesRead   int64  `json:"bytes_read"`
		Note        string `json:"note"`
	} `json:"content"`
	Results []struct {
		RuleID      string `json:"rule_id"`
		Status      string `json:"status"`
		Checked     int64  `json:"checked"`
		Failed      int64  `json:"failed"`
		NotAssessed int64  `json:"not_assessed"`
	} `json:"results"`
	Findings struct {
		BySeverity map[string]int64 `json:"by_severity"`
	} `json:"findings"`
	Distributions struct {
		Splits []model.Count `json:"splits"`
		Labels []model.Count `json:"labels"`
	} `json:"distributions"`
	Readiness *struct {
		Dimensions []struct {
			Dimension string   `json:"dimension"`
			Status    string   `json:"status"`
			Coverage  string   `json:"coverage"`
			Notes     []string `json:"notes"`
		} `json:"dimensions"`
		Disclaimer string `json:"disclaimer"`
	} `json:"readiness"`
	Summary string `json:"summary"`
}

func (r reportView) result(ruleID string) (status string, checked, failed int64) {
	for _, res := range r.Results {
		if res.RuleID == ruleID {
			return res.Status, res.Checked, res.Failed
		}
	}
	return "", 0, 0
}

// validated runs a validation to its end and returns the job, its report
// and the report's body.
func (s *securityStack) validated(t *testing.T, p model.Principal, datasetID string, in datasetservice.ValidationInput, readiness bool) (model.ValidationJob, model.ValidationReport, reportView) {
	t.Helper()
	ctx := context.Background()
	start := s.datasets.StartValidation
	if readiness {
		start = s.datasets.StartReadiness
	}
	job, _, err := start(ctx, p, datasetID, in)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	drain(t, s.newDatasetService(t, nil), "worker-v")
	if job, err = s.datasets.Validation(ctx, p, job.ID); err != nil || job.ReportID == "" {
		t.Fatalf("job after the worker: %+v %v", job, err)
	}
	report, err := s.datasets.Report(ctx, p, job.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	var view reportView
	if err := json.Unmarshal(report.Body, &view); err != nil {
		t.Fatal(err)
	}
	return job, report, view
}

// allFindings pages through a report's findings.
func (s *securityStack) allFindings(t *testing.T, p model.Principal, reportID, severity string) []model.Finding {
	t.Helper()
	var all []model.Finding
	cursor := ""
	for {
		page, err := s.datasets.Findings(context.Background(), p, reportID, severity, "", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			return all
		}
		cursor = page.NextCursor
	}
}

func findingCodes(findings []model.Finding) map[string]int {
	out := map[string]int{}
	for _, f := range findings {
		out[f.Code]++
	}
	return out
}

func TestValidationRulesFindingsAndReport(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-val")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "val", 5)
	d := s.newDataset(t, dev, "validated")
	licensed := map[string]string{"license": "cc-by"}
	s.addMembers(t, dev, d.ID,
		datasetservice.MemberInput{ObjectID: objs[0].ObjectID, Label: "a", Split: "train", Metadata: licensed},
		datasetservice.MemberInput{ObjectID: objs[1].ObjectID, Label: "b", Split: "train", Metadata: licensed},
		datasetservice.MemberInput{ObjectID: objs[2].ObjectID, Label: "c", Split: "train"},
		datasetservice.MemberInput{ObjectID: objs[3].ObjectID, Label: "d", Split: "test"},
		datasetservice.MemberInput{ObjectID: objs[4].ObjectID, Metadata: licensed},
	)
	v, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	rules := []model.Rule{
		ruleOf("license", "required_metadata", model.SeverityError, `{"keys":["license"]}`),
		ruleOf("labelled", "required_label", model.SeverityWarning, ""),
		ruleOf("split", "split_assignment", model.SeverityError, ""),
		ruleOf("images-only", "allowed_media_types", model.SeverityInfo, `{"types":["image/*"]}`),
		ruleOf("not-empty", "member_count", model.SeverityError, `{"min":1}`),
		ruleOf("verified", "checksum_verified", model.SeverityWarning, ""),
	}
	// Profiles are administered: developers use them but do not write them.
	if _, err := s.datasets.CreateProfile(ctx, dev, datasetservice.ProfileInput{Name: "curation",
		Definition: model.ProfileDefinition{Rules: rules}}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("developer writes a profile: %v", err)
	}
	if _, err := s.datasets.CreateProfile(ctx, acme.admin, datasetservice.ProfileInput{Name: "broken",
		Definition: model.ProfileDefinition{Rules: []model.Rule{ruleOf("x", "guess_quality", model.SeverityError, "")}}}); !errors.Is(err, model.ErrInvalidProfile) {
		t.Fatalf("unknown rule type: %v", err)
	}
	prof := s.profileOf(t, acme.admin, "curation", 10, rules...)
	// A second version of the same name keeps the ID.
	if v2 := s.profileOf(t, acme.admin, "curation", 0, rules...); v2.ID != prof.ID || v2.Version != 2 {
		t.Fatalf("profile v2: %+v", v2)
	}

	in := datasetservice.ValidationInput{ProfileID: prof.ID, ProfileVersion: 1, IdempotencyKey: "val-1"}
	job, created, err := s.datasets.StartValidation(ctx, dev, d.ID, in)
	if err != nil || !created || job.State != model.ValidationQueued || job.TotalItems != 5 || job.DatasetVersionID != v.ID {
		t.Fatalf("start: %+v %v", job, err)
	}
	if again, created, err := s.datasets.StartValidation(ctx, dev, d.ID, in); err != nil || created || again.ID != job.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, _, err := s.datasets.StartValidation(ctx, dev, d.ID, datasetservice.ValidationInput{ProfileID: prof.ID, IdempotencyKey: "val-1", Mode: model.ModeFull}); !errors.Is(err, model.ErrValidationConflict) {
		t.Fatalf("other request under the key: %v", err)
	}
	if got, _ := s.datasets.Get(ctx, dev, d.ID); got.Lifecycle != model.LifecycleValidating {
		t.Fatalf("dataset while queued: %s", got.Lifecycle)
	}
	drain(t, s.newDatasetService(t, nil), "worker-v")

	job, _ = s.datasets.Validation(ctx, dev, job.ID)
	if job.State != model.ValidationCompletedWithWarnings || job.ProcessedItems != 5 || job.PassedItems != 2 || job.FailedItems != 3 ||
		job.WarningItems != 0 || job.ReportID == "" {
		t.Fatalf("job: %+v", job)
	}
	dataset, _ := s.datasets.Get(ctx, dev, d.ID)
	version, _ := s.datasets.Version(ctx, dev, d.ID, v.ID)
	if dataset.Lifecycle != model.LifecycleReadyWithWarnings || dataset.LatestReportID != job.ReportID ||
		version.ValidationStatus != string(model.LifecycleReadyWithWarnings) || version.LatestReportID != job.ReportID {
		t.Fatalf("outcome: dataset %s %s, version %s", dataset.Lifecycle, dataset.LatestReportID, version.ValidationStatus)
	}

	report, err := s.datasets.Report(ctx, acme.auditor, job.ReportID)
	if err != nil || report.Status != model.LifecycleReadyWithWarnings || report.SchemaVersion != 1 || report.Readiness {
		t.Fatalf("report: %+v %v", report, err)
	}
	var body reportView
	if err := json.Unmarshal(report.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.SchemaVersion != 1 || body.ManifestSHA256 != v.ManifestSHA256 || body.Profile.ID != prof.ID || body.Profile.Version != 1 ||
		body.Members.Total != 5 || body.Members.Examined != 5 || body.Content.Coverage != "NONE" || body.Mode != "METADATA" {
		t.Fatalf("body: %+v", body)
	}
	for rule, want := range map[string]struct {
		status  string
		failed  int64
		checked int64
	}{
		"license": {datasetservice.CheckFailed, 2, 5}, "labelled": {datasetservice.CheckFailed, 1, 5},
		"split": {datasetservice.CheckFailed, 1, 5}, "images-only": {datasetservice.CheckFailed, 5, 5},
		"not-empty": {datasetservice.CheckPassed, 0, 1}, "verified": {datasetservice.CheckPassed, 0, 5},
	} {
		if status, checked, failed := body.result(rule); status != want.status || failed != want.failed || checked != want.checked {
			t.Errorf("%s: %s checked %d failed %d, want %+v", rule, status, checked, failed, want)
		}
	}
	if body.Findings.BySeverity["ERROR"] != 3 || body.Findings.BySeverity["WARNING"] != 1 || body.Findings.BySeverity["INFO"] != 5 {
		t.Fatalf("findings by severity: %v", body.Findings.BySeverity)
	}
	if !strings.Contains(body.Summary, "READY_WITH_WARNINGS") || !strings.Contains(body.Content.Note, "no content check") {
		t.Fatalf("summary: %s / %s", body.Summary, body.Content.Note)
	}

	// Findings page by severity, each once, with safe, stable fields.
	all := s.allFindings(t, dev, job.ReportID, "")
	if len(all) != 9 {
		t.Fatalf("findings: %d", len(all))
	}
	seen := map[string]bool{}
	for _, f := range all {
		if seen[f.ID] || f.RuleVersion != 1 || f.Evidence == "" || f.Message == "" {
			t.Fatalf("finding: %+v", f)
		}
		seen[f.ID] = true
	}
	if errs := s.allFindings(t, dev, job.ReportID, "error"); len(errs) != 3 || findingCodes(errs)["MISSING_METADATA"] != 2 || findingCodes(errs)["MISSING_SPLIT"] != 1 {
		t.Fatalf("errors: %v", findingCodes(errs))
	}
	if _, err := s.datasets.Findings(ctx, dev, job.ReportID, "FATAL", "", "", 0); !errors.Is(err, datasetservice.ErrInvalidRequest) {
		t.Fatalf("bad severity: %v", err)
	}
	if page, err := s.datasets.Findings(ctx, dev, job.ReportID, "", "split", "", 10); err != nil || len(page.Items) != 1 {
		t.Fatalf("by rule: %+v %v", page, err)
	}

	// Changing the draft leaves the validated outcome behind.
	extra := s.publishAt(t, dev, "val/extra.txt", textData("extra", 50))
	if res := s.addMembers(t, dev, d.ID, memberOf(extra, "e", "test")); res.Dataset.Lifecycle != model.LifecycleDraft {
		t.Fatalf("after a change: %s", res.Dataset.Lifecycle)
	}
}

func TestContentValidationModes(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-content")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "content", 6)
	d := s.newDataset(t, dev, "content")
	for _, o := range objs {
		s.addMembers(t, dev, d.ID, memberOf(o, "x", "train"))
	}
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	// One version's bytes are damaged, another's are gone.
	s.ie.storage.PutObject(objs[1].ObjectKey, []byte("damaged bytes"))
	if err := s.ie.storage.DeleteObject(ctx, s.ie.bucket, objs[2].ObjectKey); err != nil {
		t.Fatal(err)
	}
	reread := s.profileOf(t, acme.admin, "reread", 0,
		ruleOf("bytes", "content_checksum", model.SeverityError, `{"reverify":true}`),
		ruleOf("present", "checksum_present", model.SeverityWarning, ""))

	// FULL reads every member and finds both.
	job, report, body := s.validated(t, dev, d.ID, datasetservice.ValidationInput{ProfileID: reread.ID, Mode: model.ModeFull}, false)
	if report.Status != model.LifecycleFailed || job.State != model.ValidationFailed || job.BytesRead == 0 ||
		body.Content.Coverage != "FULL" || body.Content.Read != 5 || body.Content.Examined != 6 || body.Content.NotExamined != 0 {
		t.Fatalf("full: %s %+v %+v", report.Status, job, body.Content)
	}
	codes := findingCodes(s.allFindings(t, dev, report.ID, ""))
	if codes["CHECKSUM_MISMATCH"] != 1 || codes["SIZE_MISMATCH"] != 1 || codes["CONTENT_MISSING"] != 1 || len(codes) != 3 {
		t.Fatalf("full findings: %v", codes)
	}

	// The built-in storage audit is this check, ready made: it finds the
	// same damaged and missing bytes, and flags the deleted object.
	_, report, body = s.validated(t, dev, d.ID, datasetservice.ValidationInput{ProfileID: "builtin:storage-audit", Mode: model.ModeFull}, false)
	audit := findingCodes(s.allFindings(t, dev, report.ID, ""))
	if report.Status != model.LifecycleFailed || body.Content.Read != 5 ||
		audit["CHECKSUM_MISMATCH"] != 1 || audit["SIZE_MISMATCH"] != 1 || audit["CONTENT_MISSING"] != 1 {
		t.Fatalf("storage audit: %s %+v %v", report.Status, body.Content, audit)
	}

	// SAMPLED reads a deterministic sample and says so.
	_, report, body = s.validated(t, dev, d.ID, datasetservice.ValidationInput{ProfileID: reread.ID, Mode: model.ModeSampled, SampleSize: 2}, false)
	if body.Content.Coverage != "SAMPLED" || body.Content.Examined+body.Content.NotExamined != 6 || body.Content.Examined > 2 ||
		!strings.Contains(body.Content.Note, "not a full integrity scan") {
		t.Fatalf("sampled: %+v", body.Content)
	}
	if status, _, _ := body.result("bytes"); status != datasetservice.CheckPartial && status != datasetservice.CheckFailed {
		t.Fatalf("a sampled check never passes outright: %s", status)
	}
	// The same sample on the same version.
	_, _, again := s.validated(t, dev, d.ID, datasetservice.ValidationInput{ProfileID: reread.ID, Mode: model.ModeSampled, SampleSize: 2}, false)
	if again.Content.Examined != body.Content.Examined || again.Status != body.Status {
		t.Fatalf("samples differ: %+v / %+v", again.Content, body.Content)
	}

	// METADATA reads nothing and says the content was not assessed.
	job, _, body = s.validated(t, dev, d.ID, datasetservice.ValidationInput{ProfileID: reread.ID}, false)
	if job.BytesRead != 0 || body.Content.Coverage != "NONE" || !strings.Contains(body.Content.Note, "metadata-only") {
		t.Fatalf("metadata: %+v %+v", job, body.Content)
	}
	if status, _, _ := body.result("bytes"); status != datasetservice.CheckNotAssessed {
		t.Fatalf("content check in metadata mode: %s", status)
	}

	// Without reverify, FULL trusts processing's verification of the same
	// bytes and reads nothing; the report says how each was established.
	job, _, body = s.validated(t, dev, d.ID, datasetservice.ValidationInput{ProfileID: "builtin:general", Mode: model.ModeFull}, false)
	if job.BytesRead != 0 || body.Content.Read != 0 || body.Content.Examined != 6 || !strings.Contains(body.Content.Note, "processing's verification") {
		t.Fatalf("evidence-based full: %+v %+v", job, body.Content)
	}
}

// A snapshot names exact versions, and content checks read exactly those.
// In a versioned bucket the recorded provider version is read, so bytes
// written over the key later neither pass nor fail for it; a read that
// fails leaves the member unexamined; a deleted version is missing. A new
// catalog version at the same path changes only later snapshots. Without
// provider versions the key is read, and an overwrite shows as a mismatch
// (TestContentValidationModes).
func TestContentValidationReadsTheSnapshotsVersions(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-pinned")
	dev := acme.developer
	s.ie.storage.Versioning = true
	objs := s.publishObjects(t, dev, "pinned", 3)
	for _, o := range objs {
		if o.StorageVersionID == "" {
			t.Fatalf("no provider version recorded: %+v", o)
		}
	}
	d := s.newDataset(t, dev, "pinned")
	for _, o := range objs {
		s.addMembers(t, dev, d.ID, memberOf(o, "x", "train"))
	}
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	reread := s.profileOf(t, acme.admin, "pinned-reread", 0,
		ruleOf("bytes", "content_checksum", model.SeverityError, `{"reverify":true}`))
	full := datasetservice.ValidationInput{VersionID: v1.ID, ProfileID: reread.ID, Mode: model.ModeFull}

	// Object 0's key is written over out of band, and object 2 gets a new
	// catalog version at its path: v1's bytes are all still there.
	s.ie.storage.PutObject(objs[0].ObjectKey, []byte("written over the key"))
	newer := s.publishAt(t, dev, "pinned/item-02.txt", textData("pinned-2 again", 77))
	job, report, body := s.validated(t, dev, d.ID, full, false)
	if status, _, _ := body.result("bytes"); status != datasetservice.CheckPassed || report.Status != model.LifecycleReady ||
		job.State != model.ValidationCompleted || body.Content.Read != 3 || body.Content.Examined != 3 {
		t.Fatalf("pinned reads: %s %s %s %+v", status, report.Status, job.State, body.Content)
	}

	// A read that fails is not examined, and the check does not pass.
	s.ie.storage.RangeErr = errors.New("storage unavailable")
	_, _, body = s.validated(t, dev, d.ID, full, false)
	if status, _, _ := body.result("bytes"); status == datasetservice.CheckPassed || body.Content.NotExamined != 1 {
		t.Fatalf("an unread member passed: %s %+v", status, body.Content)
	}

	// Object 1's recorded version is deleted: missing, not a pass.
	if err := s.ie.storage.DeleteObjectVersion(ctx, s.ie.bucket, objs[1].ObjectKey, objs[1].StorageVersionID); err != nil {
		t.Fatal(err)
	}
	_, report, _ = s.validated(t, dev, d.ID, full, false)
	findings := s.allFindings(t, dev, report.ID, "")
	if codes := findingCodes(findings); codes["CONTENT_MISSING"] != 1 || len(codes) != 1 || findings[0].AssetID != objs[1].ID {
		t.Fatalf("deleted version: %v", findings)
	}

	// The next snapshot names the new catalog version; v1 still names the old.
	v2, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	pinned := func(v model.DatasetVersion) string {
		for _, m := range s.versionMembers(t, dev, v) {
			if m.ObjectID == objs[2].ObjectID {
				return m.AssetID
			}
		}
		return ""
	}
	if pinned(v1) != objs[2].ID || pinned(v2) != newer.ID {
		t.Fatalf("v1 names %s, v2 names %s; want %s and %s", pinned(v1), pinned(v2), objs[2].ID, newer.ID)
	}
}

func TestEvidenceRulesLeakageAndDuplicates(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-evidence")
	dev := acme.developer
	same := textData("identical content", 64)
	a := s.publishAt(t, dev, "ev/a.txt", same)
	b := s.publishAt(t, dev, "ev/b.txt", same)
	c := s.publishAt(t, dev, "ev/c.txt", textData("c", 64))
	e := s.publishAt(t, dev, "ev/e.txt", textData("e", 64))
	if a.ChecksumSHA256 != b.ChecksumSHA256 {
		t.Fatal("identical content has one checksum")
	}
	// c's scan found malware; e's checksum was never verified.
	for _, q := range []struct {
		sql string
		id  string
	}{
		{`UPDATE processing_jobs SET scan_result = 'INFECTED' WHERE asset_id = $1`, c.ID},
		{`UPDATE processing_jobs SET verification_status = NULL, checksum_verified_at = NULL WHERE asset_id = $1`, e.ID},
	} {
		if _, err := s.db.Pool.Exec(ctx, q.sql, q.id); err != nil {
			t.Fatal(err)
		}
	}
	d := s.newDataset(t, dev, "evidence")
	s.addMembers(t, dev, d.ID, memberOf(a, "x", "train"), memberOf(b, "x", "test"), memberOf(c, "y", "train"), memberOf(e, "y", "test"))
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	prof := s.profileOf(t, acme.admin, "evidence", 0,
		ruleOf("malware", "malware_scan", model.SeverityError, ""),
		ruleOf("verified", "checksum_verified", model.SeverityWarning, ""),
		ruleOf("leakage", "split_leakage", model.SeverityError, ""),
		ruleOf("duplicates", "duplicate_content", model.SeverityWarning, ""),
		ruleOf("references", "duplicate_references", model.SeverityError, ""))

	_, report, body := s.validated(t, dev, d.ID, datasetservice.ValidationInput{ProfileID: prof.ID}, false)
	findings := s.allFindings(t, dev, report.ID, "")
	codes := findingCodes(findings)
	if report.Status != model.LifecycleFailed || codes["MALWARE_DETECTED"] != 1 || codes["CHECKSUM_UNVERIFIED"] != 1 ||
		codes["SPLIT_LEAKAGE"] != 1 || codes["DUPLICATE_CONTENT"] != 1 || codes["DUPLICATE_REFERENCE"] != 0 {
		t.Fatalf("%s: %v", report.Status, codes)
	}
	for _, f := range findings {
		if f.Code == "MALWARE_DETECTED" && f.ObjectID != c.ObjectID {
			t.Fatalf("malware finding names %s", f.ObjectID)
		}
		if f.Code == "SPLIT_LEAKAGE" && (!strings.Contains(f.Observed, a.ObjectID) || !strings.Contains(f.Observed, b.ObjectID)) {
			t.Fatalf("leakage finding: %+v", f)
		}
	}
	if status, _, _ := body.result("references"); status != datasetservice.CheckPassed {
		t.Fatalf("duplicate references: %s", status)
	}

	// Without verified checksums, equal checksums prove nothing: neither
	// leakage nor duplication is reported.
	if _, err := s.db.Pool.Exec(ctx, `UPDATE processing_jobs SET verification_status = NULL, checksum_verified_at = NULL WHERE asset_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	_, report, _ = s.validated(t, dev, d.ID, datasetservice.ValidationInput{ProfileID: prof.ID}, false)
	codes = findingCodes(s.allFindings(t, dev, report.ID, ""))
	if codes["SPLIT_LEAKAGE"] != 0 || codes["DUPLICATE_CONTENT"] != 0 || codes["CHECKSUM_UNVERIFIED"] != 2 {
		t.Fatalf("unverified checksums: %v", codes)
	}
}

func TestValidationRecoveryRetriesAndIdempotence(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-recover")
	globex := s.team(t, "ds-recover-b")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "rec", 7)
	d := s.newDataset(t, dev, "recover")
	for i, o := range objs {
		label := ""
		if i%2 == 0 {
			label = "even"
		}
		s.addMembers(t, dev, d.ID, memberOf(o, label, "train"))
	}
	if _, _, err := s.datasets.Snapshot(ctx, dev, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	prof := s.profileOf(t, acme.admin, "labels", 0, ruleOf("labelled", "required_label", model.SeverityWarning, ""))
	job, _, err := s.datasets.StartValidation(ctx, dev, d.ID, datasetservice.ValidationInput{ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}

	// A worker that lets go after each batch checks three members...
	slow := s.newDatasetService(t, func(c *config.Datasets) { c.ValidationBudget = time.Nanosecond })
	if n, err := slow.RunDueValidations(ctx, "worker-slow", 1); err != nil || n != 1 {
		t.Fatalf("first pass: %d %v", n, err)
	}
	job, _ = s.datasets.Validation(ctx, dev, job.ID)
	if job.State != model.ValidationRunning || job.ProcessedItems != 3 || job.CheckpointOrdinal != 3 {
		t.Fatalf("after one batch: %+v", job)
	}
	// ...the same batch delivered again is refused whole, writing nothing.
	err = s.datasetDB.CommitValidationBatch(ctx, interfaces.ValidationBatch{JobID: job.ID, Owner: "worker-slow", From: 0, To: 3,
		Processed: 3, Findings: []model.Finding{{ID: "dup", JobID: job.ID, TenantID: acme.org.ID, RuleID: "labelled", RuleVersion: 1,
			Severity: model.SeverityWarning, Code: "X", Message: "x", Evidence: "x", CreatedAt: time.Now()}}, Now: time.Now()})
	if !errors.Is(err, model.ErrValidationState) {
		t.Fatalf("stale batch: %v", err)
	}
	// ...then a worker takes it and dies.
	if claimed, err := s.datasetDB.ClaimValidations(ctx, "worker-dead", job.ID, time.Now(), 50*time.Millisecond, 5, 1); err != nil || len(claimed) != 1 {
		t.Fatalf("dead claim: %v %v", claimed, err)
	}
	time.Sleep(100 * time.Millisecond)
	drain(t, s.newDatasetService(t, nil), "worker-live")

	job, _ = s.datasets.Validation(ctx, dev, job.ID)
	if job.State != model.ValidationCompletedWithWarnings || job.ProcessedItems != 7 || job.PassedItems != 4 || job.WarningItems != 3 {
		t.Fatalf("recovered job: %+v", job)
	}
	findings := s.allFindings(t, dev, job.ReportID, "")
	if len(findings) != 3 || findingCodes(findings)["MISSING_LABEL"] != 3 {
		t.Fatalf("findings after recovery: %v", findingCodes(findings))
	}
	// Running a finished job again does nothing.
	if note, err := s.newDatasetService(t, nil).RunValidation(ctx, "worker-live", acme.org.ID, job.ID); err != nil || !strings.Contains(note, "not due") {
		t.Fatalf("rerun: %q %v", note, err)
	}

	// An event naming another tenant runs nothing.
	other, _, err := s.datasets.StartValidation(ctx, dev, d.ID, datasetservice.ValidationInput{ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.newDatasetService(t, nil).RunValidation(ctx, "worker-live", globex.org.ID, other.ID); err == nil {
		t.Fatal("a job ran for another tenant's event")
	}
	// A job that keeps failing ends FAILED, without a report, and its
	// dataset leaves VALIDATING.
	if _, err := s.db.Pool.Exec(ctx, `UPDATE validation_jobs SET attempts = max_attempts + 1, lease_owner = NULL, lease_expires_at = NULL,
		next_attempt_at = now() WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	drain(t, s.newDatasetService(t, nil), "worker-live")
	other, _ = s.datasets.Validation(ctx, dev, other.ID)
	if other.State != model.ValidationFailed || other.ReportID != "" || !strings.Contains(other.LastError, "attempts exhausted") {
		t.Fatalf("exhausted job: %+v", other)
	}
	if got, _ := s.datasets.Get(ctx, dev, d.ID); got.Lifecycle != model.LifecycleDraft {
		t.Fatalf("dataset after a failed run: %s", got.Lifecycle)
	}

	// A dataset left VALIDATING by hand is released.
	if _, err := s.db.Pool.Exec(ctx, `UPDATE datasets SET lifecycle_status = 'VALIDATING' WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.datasets.ReleaseStuck(ctx); err != nil || n < 1 {
		t.Fatalf("release: %d %v", n, err)
	}
	if got, _ := s.datasets.Get(ctx, dev, d.ID); got.Lifecycle != model.LifecycleDraft {
		t.Fatalf("released dataset: %s", got.Lifecycle)
	}

	// Per-tenant limits: unfinished validations, and how many run at once.
	limited := s.newDatasetService(t, func(c *config.Datasets) { c.MaxActiveValidations = 2 })
	for i := range 3 {
		_, _, err := limited.StartValidation(ctx, dev, d.ID, datasetservice.ValidationInput{ProfileID: prof.ID})
		if i < 2 && err != nil {
			t.Fatal(err)
		}
		if i == 2 && !errors.Is(err, model.ErrTooManyJobs) {
			t.Fatalf("third active validation: %v", err)
		}
	}
	if claimed, err := s.datasetDB.ClaimValidations(ctx, "worker-fair", "", time.Now(), time.Minute, 1, 5); err != nil || len(claimed) != 1 {
		t.Fatalf("one running per tenant: %d %v", len(claimed), err)
	}
}

func TestReadinessAssessment(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "ds-ready")
	dev := acme.developer
	objs := s.publishObjects(t, dev, "ready", 8)
	d := s.newDataset(t, dev, "ready")
	splits := []string{"train", "train", "train", "train", "train", "train", "validation", "test"}
	for i, o := range objs {
		s.addMembers(t, dev, d.ID, memberOf(o, []string{"cat", "dog"}[i%2], splits[i]))
	}
	v, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	// An assessment is against a profile someone chose.
	if _, _, err := s.datasets.StartReadiness(ctx, dev, d.ID, datasetservice.ValidationInput{}); !errors.Is(err, datasetservice.ErrInvalidRequest) {
		t.Fatalf("readiness without a profile: %v", err)
	}
	profile := "builtin:supervised-classification"
	current, _ := s.datasets.Get(ctx, dev, d.ID)
	d, err = s.datasets.Update(ctx, dev, d.ID, current.Revision, datasetservice.UpdateInput{ProfileID: &profile})
	if err != nil {
		t.Fatal(err)
	}

	_, report, body := s.validated(t, dev, d.ID, datasetservice.ValidationInput{Mode: model.ModeSampled, SampleSize: 3}, true)
	if !report.Readiness || body.Readiness == nil || len(body.Readiness.Dimensions) != 9 || body.Readiness.Disclaimer == "" ||
		report.ProfileID != profile || body.ManifestSHA256 != v.ManifestSHA256 {
		t.Fatalf("readiness report: %+v %+v", report, body.Readiness)
	}
	status := map[string]string{}
	coverage := map[string]string{}
	for _, dim := range body.Readiness.Dimensions {
		status[dim.Dimension], coverage[dim.Dimension] = dim.Status, dim.Coverage
	}
	want := map[string]string{
		datasetservice.DimCompleteness:    datasetservice.DimensionPass,
		datasetservice.DimIntegrity:       datasetservice.DimensionWarn, // content only sampled
		datasetservice.DimSecurity:        datasetservice.DimensionPass,
		datasetservice.DimDuplication:     datasetservice.DimensionPass,
		datasetservice.DimReproducibility: datasetservice.DimensionPass,
		datasetservice.DimGovernance:      datasetservice.DimensionNotAssessed,
		datasetservice.DimAccessibility:   datasetservice.DimensionWarn, // eight small files, none packed
		datasetservice.DimConsistency:     datasetservice.DimensionPass,
	}
	for dim, s := range want {
		if status[dim] != s {
			t.Errorf("%s: %s, want %s (%+v)", dim, status[dim], s, body.Readiness.Dimensions)
		}
	}
	if coverage[datasetservice.DimIntegrity] != "SAMPLED" {
		t.Fatalf("integrity coverage: %s", coverage[datasetservice.DimIntegrity])
	}

	// A plain validation is not a readiness report; a second assessment is
	// a second report, the newest first.
	s.validated(t, dev, d.ID, datasetservice.ValidationInput{}, false)
	_, second, _ := s.validated(t, dev, d.ID, datasetservice.ValidationInput{Mode: model.ModeFull}, true)
	page, err := s.datasets.Reports(ctx, acme.auditor, d.ID, true, "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != second.ID || page.NextCursor == "" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	rest, err := s.datasets.Reports(ctx, acme.auditor, d.ID, true, page.NextCursor, 5)
	if err != nil || len(rest.Items) != 1 || rest.Items[0].ID != report.ID {
		t.Fatalf("second page: %+v %v", rest, err)
	}
	if all, _ := s.datasets.Reports(ctx, dev, d.ID, false, "", 10); len(all.Items) != 3 {
		t.Fatalf("all reports: %d", len(all.Items))
	}
	// The first report is unchanged by the later ones.
	if again, _ := s.datasets.Report(ctx, dev, report.ID); string(again.Body) != string(report.Body) {
		t.Fatal("a report changed")
	}
}
