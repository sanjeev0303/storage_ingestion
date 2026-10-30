package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

func scenesCSV(from, to int) []byte {
	var sb strings.Builder
	sb.WriteString("scene,reflectance,band\n")
	for i := from; i < to; i++ {
		band := "B04"
		if i%4 == 0 {
			band = "B08"
		}
		fmt.Fprintf(&sb, "s-%d,%d,%s\n", i, i, band)
	}
	return []byte(sb.String())
}

func tableNamed(t *testing.T, tables []model.TableProfile, name string) model.TableProfile {
	t.Helper()
	for _, tb := range tables {
		if tb.Table == name {
			return tb
		}
	}
	t.Fatalf("no table %s in %d tables", name, len(tables))
	return model.TableProfile{}
}

func tableDrift(t *testing.T, r model.DriftReport, name string) model.TableDrift {
	t.Helper()
	for _, tb := range r.Tables {
		if tb.Table == name {
			return tb
		}
	}
	t.Fatalf("no drift for %s", name)
	return model.TableDrift{}
}

func (s *securityStack) profileAll(t *testing.T) {
	t.Helper()
	for i := 0; i < 20; i++ {
		found, err := s.datasets.ProfileOnce(context.Background(), "it-quality")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return
		}
	}
	t.Fatal("profiles kept coming")
}

// Column profiles per snapshot, drift against the parent, the readiness
// rules built on them, leakage between training and evaluation versions
// (by hash, by subject, recorded n-grams, and from a run's roles), and
// release labels with their diffs.
func TestDataQuality(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "quality")
	dev := acme.developer

	a := s.publishAt(t, dev, "scenes/part-1.csv", scenesCSV(0, 200),
		withMetadata(map[string]string{"processing_baseline": "04.00", "patient": "p1"}))
	meta := s.publishAt(t, dev, "meta.jsonl", []byte("{\"tile\":\"T31\",\"cloud\":0.1}\n{\"tile\":\"T32\",\"cloud\":null}\n"))
	note := s.publishAt(t, dev, "README.txt", textData("readme", 64))
	d := s.newDataset(t, dev, "sentinel")
	s.addMembers(t, dev, d.ID, memberOf(a, "", "train"), memberOf(meta, "", "train"), memberOf(note, "", "train"))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.datasets.VersionProfile(ctx, dev, d.ID, v1.ID); err != nil || p.State != model.ProfilePending {
		t.Fatalf("a new version is queued for profiling: %+v %v", p, err)
	}
	s.profileAll(t)
	p1, err := s.datasets.VersionProfile(ctx, dev, d.ID, v1.ID)
	if err != nil || p1.State != model.ProfileDone || len(p1.Tables) != 2 || p1.OtherFiles != 1 || p1.FilesProfiled != 2 || p1.Drift != nil {
		t.Fatalf("first profile: %+v %v", p1, err)
	}
	if scenes := tableNamed(t, p1.Tables, "scenes"); scenes.Rows != 200 || len(scenes.Columns) != 3 {
		t.Fatalf("scenes: %+v", scenes)
	}
	if tableNamed(t, p1.Tables, "meta.jsonl").Rows != 2 {
		t.Fatal("meta.jsonl rows")
	}

	// A reprocessed part: more rows, shifted values, a new baseline.
	b := s.publishAt(t, dev, "scenes/part-2.csv", scenesCSV(1000, 1300),
		withMetadata(map[string]string{"processing_baseline": "05.00", "patient": "p2"}))
	s.addMembers(t, dev, d.ID, memberOf(b, "", "test"))
	v2, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	s.profileAll(t)
	p2, err := s.datasets.VersionProfile(ctx, dev, d.ID, v2.ID)
	if err != nil || !p2.Drifted || p2.BaselineVersionID != v1.ID || p2.Drift == nil || p2.Drift.Breaches < 2 {
		t.Fatalf("drift against the parent: %+v %v", p2, err)
	}
	drift, err := s.datasets.Drift(ctx, dev, d.ID, v2.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if sd := tableDrift(t, drift, "scenes"); sd.Breached[0] != "rows" || sd.Columns[1].Breached[0] != "distribution" {
		t.Fatalf("drift on demand: %+v", sd)
	}

	// The readiness rules: drift, one baseline per snapshot, and subjects
	// split across train and test.
	prof := s.profileOf(t, acme.admin, "quality", 0,
		ruleOf("drift", "column_drift", model.SeverityError, ""),
		ruleOf("baseline", "uniform_metadata", model.SeverityError, `{"key":"processing_baseline"}`),
		ruleOf("subjects", "subject_leakage", model.SeverityError, `{"pattern":"^scenes/(part)-"}`))
	_, report, _ := s.validated(t, dev, d.ID, datasetservice.ValidationInput{VersionID: v2.ID, ProfileID: prof.ID}, true)
	codes := findingCodes(s.allFindings(t, dev, report.ID, ""))
	if codes["COLUMN_DRIFT"] == 0 || codes["TABLE_DRIFT"] == 0 || codes["MIXED_METADATA"] != 1 || codes["SUBJECT_LEAKAGE"] != 1 {
		t.Fatalf("quality findings: %v", codes)
	}

	// Leakage: an evaluation set sharing one file's bytes and one patient.
	if _, err := s.datasets.PutQualitySettings(ctx, acme.auditor, model.QualitySettings{DatasetID: d.ID, MaxPSI: 0.2, MaxNullChange: 0.1,
		MaxRowChange: 0.5}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("settings need datasets.manage: %v", err)
	}
	evalSet := s.newDataset(t, dev, "benchmark")
	bad := model.DefaultQualitySettings(evalSet.ID)
	bad.SubjectKey, bad.SubjectPattern = "patient", "^(x)"
	if _, err := s.datasets.PutQualitySettings(ctx, acme.owner, bad); !errors.Is(err, datasetservice.ErrInvalidRequest) {
		t.Fatalf("subject key and pattern together: %v", err)
	}
	set := model.DefaultQualitySettings(evalSet.ID)
	set.SubjectKey = "patient"
	if _, err := s.datasets.PutQualitySettings(ctx, acme.owner, set); err != nil {
		t.Fatal(err)
	}
	copied := s.publishAt(t, dev, "bench/copy.csv", scenesCSV(0, 200))
	sameSubject := s.publishAt(t, dev, "bench/p2.csv", scenesCSV(5000, 5010), withMetadata(map[string]string{"patient": "p2"}))
	fresh := s.publishAt(t, dev, "bench/new.csv", scenesCSV(9000, 9010), withMetadata(map[string]string{"patient": "p9"}))
	s.addMembers(t, dev, evalSet.ID, memberOf(copied, "", ""), memberOf(sameSubject, "", ""), memberOf(fresh, "", ""))
	ev, _, err := s.datasets.Snapshot(ctx, dev, evalSet.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.datasets.CheckLeakage(ctx, dev, evalSet.ID, ev.ID, v2.ID)
	if err != nil || len(rep.Checks) != 2 {
		t.Fatalf("leakage: %+v %v", rep, err)
	}
	exact, subject := rep.Checks[0], rep.Checks[1]
	if exact.State != model.LeakLeaked || exact.Overlap != 1 || exact.EvalMembers != 3 || exact.Examples[0].TrainPath != "scenes/part-1.csv" {
		t.Fatalf("exact overlap: %+v", exact)
	}
	if subject.State != model.LeakLeaked || subject.Overlap != 1 || subject.Examples[0].Key != "p2" {
		t.Fatalf("subject overlap: %+v", subject)
	}
	if len(rep.NotChecked) != 1 || !strings.Contains(rep.NotChecked[0], "near-duplicate") {
		t.Fatalf("what was not checked: %v", rep.NotChecked)
	}

	// A run that names its roles is checked by the worker.
	if _, _, err := s.datasets.RecordTrainingRun(ctx, dev, datasetservice.TrainingRunInput{Name: "bad-role", Source: "SDK",
		VersionIDs: []string{v2.ID}, Roles: map[string]string{v2.ID: "HOLDOUT"}}); !errors.Is(err, datasetservice.ErrInvalidRequest) {
		t.Fatalf("an unknown role: %v", err)
	}
	run, _, err := s.datasets.RecordTrainingRun(ctx, dev, datasetservice.TrainingRunInput{Name: "with-roles", Source: "SDK",
		VersionIDs: []string{v2.ID, ev.ID}, Roles: map[string]string{v2.ID: "train", ev.ID: "TEST"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.datasets.TrainingRun(ctx, dev, run.ID); err != nil || got.Roles[v2.ID] != model.RoleTrain || got.Roles[ev.ID] != model.RoleTest {
		t.Fatalf("roles kept: %+v %v", got.Roles, err)
	}
	if n, err := s.datasets.CheckRunsOnce(ctx); err != nil || n != 1 {
		t.Fatalf("worker checked %d runs: %v", n, err)
	}
	if n, _ := s.datasets.CheckRunsOnce(ctx); n != 0 {
		t.Fatal("a run was checked twice")
	}
	checks, err := s.datasets.LeakageChecks(ctx, dev, run.ID, "", "")
	if err != nil || len(checks) != 2 || checks[0].RunID != run.ID {
		t.Fatalf("the run's checks: %+v %v", checks, err)
	}

	// An n-gram comparison made by the SDK, recorded as the caller's.
	ng, err := s.datasets.RecordNgram(ctx, dev, datasetservice.NgramInput{RunID: run.ID, TrainVersionID: v2.ID, EvalVersionID: ev.ID,
		N: 13, Threshold: 0.5, Overlap: 1, EvalMembers: 3})
	if err != nil || ng.State != model.LeakLeaked || ng.CreatedBy != dev.UserID {
		t.Fatalf("n-grams: %+v %v", ng, err)
	}
	if rep, _ = s.datasets.CheckLeakage(ctx, dev, evalSet.ID, ev.ID, v2.ID); rep.NotChecked == nil || len(rep.NotChecked) != 0 {
		t.Fatalf("after n-grams nothing is unchecked: %v", rep.NotChecked)
	}

	// Releases.
	if _, err := s.datasets.Release(ctx, dev, d.ID, v1.ID, "DR1", "first data release"); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("releases need datasets.manage: %v", err)
	}
	if _, err := s.datasets.Release(ctx, acme.owner, d.ID, v1.ID, "DR1", "first data release"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.datasets.Release(ctx, acme.owner, d.ID, v2.ID, "DR1", ""); !errors.Is(err, model.ErrReleaseTaken) {
		t.Fatalf("a label is used once: %v", err)
	}
	if _, err := s.datasets.Release(ctx, acme.owner, d.ID, v1.ID, "DR1b", ""); !errors.Is(err, model.ErrReleaseTaken) {
		t.Fatalf("a version is released once: %v", err)
	}
	if _, err := s.datasets.Release(ctx, acme.owner, d.ID, v2.ID, "bad label", ""); !errors.Is(err, datasetservice.ErrInvalidRequest) {
		t.Fatalf("label syntax: %v", err)
	}
	if _, err := s.datasets.Release(ctx, acme.owner, d.ID, v2.ID, "DR2", "baseline 05.00"); err != nil {
		t.Fatal(err)
	}
	diff, err := s.datasets.DiffRelease(ctx, dev, d.ID, "DR2", "")
	if err != nil || diff.Previous.Label != "DR1" || diff.Members.Added != 1 || diff.Drift == nil || diff.Drift.Breaches == 0 {
		t.Fatalf("release diff: %+v %v", diff, err)
	}
	if _, err := s.datasets.DiffRelease(ctx, dev, d.ID, "DR1", ""); !errors.Is(err, datasetservice.ErrInvalidRequest) {
		t.Fatalf("the first release has nothing before it: %v", err)
	}

	// A dataset that turns profiling off is not queued.
	off := s.newDataset(t, dev, "no-profiles")
	quiet := model.DefaultQualitySettings(off.ID)
	quiet.ProfileSnapshots = false
	if _, err := s.datasets.PutQualitySettings(ctx, acme.owner, quiet); err != nil {
		t.Fatal(err)
	}
	s.addMembers(t, dev, off.ID, memberOf(note, "", ""))
	ov, _, err := s.datasets.Snapshot(ctx, dev, off.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.datasets.VersionProfile(ctx, dev, off.ID, ov.ID); err != nil || p.State != datasetservice.ProfileNone {
		t.Fatalf("profiling off: %+v %v", p, err)
	}
}
