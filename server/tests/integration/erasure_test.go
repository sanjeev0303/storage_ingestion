package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/erasure"
)

func (s *securityStack) runDeletions(t *testing.T) assetservice.CycleStats {
	t.Helper()
	var cycle assetservice.CycleStats
	if err := s.lifecycle.ProcessDeletions(context.Background(), &cycle); err != nil {
		t.Fatal(err)
	}
	return cycle
}

func memberPaths(t *testing.T, s *securityStack, p model.Principal, v model.DatasetVersion) []string {
	t.Helper()
	var out []string
	for _, m := range s.versionMembers(t, p, v) {
		out = append(out, m.Path)
	}
	return out
}

// A subject's withdrawal reaches every copy, both snapshots that pin it,
// the draft and the model trained on it; it makes successor snapshots,
// stops training reads of the old ones, deletes what no hold protects,
// waits for the hold, and certifies the destruction. A takedown by
// content hash does the same for a file at any path.
func TestErasureAndWithdrawal(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "erasure")
	dev, owner := acme.developer, acme.owner
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	er := erasure.New(erasure.Deps{Store: postgres.NewErasureRepository(s.db), Lifecycle: s.lifecycle, Catalog: s.catalog,
		Audit: s.audit, Log: s.log, Signer: erasure.Signer{KeyID: "it-er", Key: key}})

	scan := textData("participant one scan", 300)
	a := s.publishAt(t, dev, "cohort/p1-scan.txt", scan, withMetadata(map[string]string{"subject_id": "P-1"}))
	b := s.publishAt(t, dev, "cohort/p2-scan.txt", textData("participant two scan", 300), withMetadata(map[string]string{"subject_id": "P-2"}))
	copied := s.publishAt(t, dev, "exports/shared-copy.txt", scan)
	notes := s.publishAt(t, dev, "cohort/p1-notes.txt", textData("participant one notes", 200), withMetadata(map[string]string{"subject_id": "P-1"}))
	book := s.publishAt(t, dev, "books/pirated.txt", textData("a pirated book", 500))

	d := s.newDataset(t, dev, "cohort")
	s.addMembers(t, dev, d.ID, memberOf(a, "", "train"), memberOf(b, "", "train"), memberOf(notes, "", "train"))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	s.addMembers(t, dev, d.ID, memberOf(book, "", "train"))
	v2, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.datasets.RecordTrainingRun(ctx, dev, datasetservice.TrainingRunInput{Name: "cohort-model", Source: "SDK",
		VersionIDs: []string{v1.ID}, Model: &datasetservice.ModelInput{Name: "risk-model", Version: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := s.lifecycle.PlaceHoldFor(ctx, owner, model.LegalHold{Scope: model.ScopeAsset, ScopeKey: notes.ID, Reason: "case 7"})
	if err != nil {
		t.Fatal(err)
	}

	// The plan.
	if _, err := er.Search(ctx, dev, model.ErasureSelector{SubjectIDs: []string{"P-1"}}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("search needs retention.manage: %v", err)
	}
	if _, err := er.Search(ctx, owner, model.ErasureSelector{SHA256s: []string{"not-a-hash"}}); !errors.Is(err, model.ErrInvalidErasure) {
		t.Fatalf("a bad hash: %v", err)
	}
	plan, err := er.Search(ctx, owner, model.ErasureSelector{SubjectIDs: []string{"P-1"}})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	held := map[string][]string{}
	for _, o := range plan.Objects {
		paths = append(paths, o.Path)
		held[o.Path] = o.HeldBy
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"cohort/p1-notes.txt", "cohort/p1-scan.txt", "exports/shared-copy.txt"}) {
		t.Fatalf("subject and its exact copy: %v", paths)
	}
	if len(held["cohort/p1-notes.txt"]) != 1 || held["cohort/p1-notes.txt"][0] != "legal hold: case 7" || len(held["cohort/p1-scan.txt"]) != 0 {
		t.Fatalf("holds in the plan: %v", held)
	}
	if len(plan.Versions) != 2 || len(plan.Drafts) != 1 || len(plan.Models) != 1 || plan.Models[0].RunID != run.ID {
		t.Fatalf("snapshots, draft and model: %+v %+v %+v", plan.Versions, plan.Drafts, plan.Models)
	}
	if len(plan.NotSearched) < 3 {
		t.Fatalf("what was not searched: %v", plan.NotSearched)
	}

	// The request, and the second person.
	if _, err := er.Create(ctx, owner, erasure.CreateInput{Kind: "SUBJECT_WITHDRAWAL", Reason: "withdrew",
		Selector: model.ErasureSelector{SubjectIDs: []string{"NOBODY"}}}); !errors.Is(err, model.ErrNothingToErase) {
		t.Fatalf("nothing matched: %v", err)
	}
	e, err := er.Create(ctx, owner, erasure.CreateInput{Kind: "subject_withdrawal", Reason: "participant withdrew consent",
		Institution: "Example Biobank", Reference: "WD-2026-118", Selector: model.ErasureSelector{SubjectIDs: []string{"P-1"}}})
	if err != nil || e.State != model.ErasurePlanned || e.SubjectHashes[0] != sha256Of([]byte("P-1")) {
		t.Fatalf("request: %+v %v", e, err)
	}
	if _, err := er.Approve(ctx, owner, e.ID, ""); !errors.Is(err, model.ErrSelfApproval) {
		t.Fatalf("the requester approving: %v", err)
	}
	if e, err = er.Approve(ctx, acme.admin, e.ID, "consent form checked"); err != nil || e.State != model.ErasureApproved {
		t.Fatalf("approval: %+v %v", e, err)
	}

	// Executing.
	if e, err = er.Execute(ctx, owner, e.ID); err != nil || e.State != model.ErasureDeleting || len(e.Successors) != 2 {
		t.Fatalf("execute: %+v %v", e, err)
	}
	if _, err := er.Execute(ctx, owner, e.ID); !errors.Is(err, model.ErrErasureState) {
		t.Fatalf("executed twice: %v", err)
	}
	old1, _ := s.datasets.Version(ctx, dev, d.ID, v1.ID)
	succ1, _ := s.datasets.Version(ctx, dev, d.ID, old1.SupersededBy)
	if old1.SupersededBy == "" || !slices.Equal(memberPaths(t, s, dev, succ1), []string{"cohort/p2-scan.txt"}) || succ1.ParentVersionID != v1.ID {
		t.Fatalf("successor of version 1: %+v %v", succ1, memberPaths(t, s, dev, succ1))
	}
	ds, _ := s.datasets.Get(ctx, dev, d.ID)
	old2, _ := s.datasets.Version(ctx, dev, d.ID, v2.ID)
	if ds.CurrentVersionID != old2.SupersededBy || ds.MemberCount != 2 {
		t.Fatalf("the current version's successor becomes current and the draft loses the subject: %+v", ds)
	}
	if _, _, err := s.datasets.RecordTrainingRun(ctx, dev, datasetservice.TrainingRunInput{Name: "again", Source: "SDK",
		VersionIDs: []string{v1.ID}}); !errors.Is(err, model.ErrVersionSuperseded) {
		t.Fatalf("training on a superseded snapshot: %v", err)
	}

	// The lifecycle deletes the scan and its copy now; the notes are held.
	s.runDeletions(t)
	if _, err := er.Progress(ctx); err != nil {
		t.Fatal(err)
	}
	e, _ = er.Get(ctx, owner, e.ID)
	if e.State != model.ErasureWaiting || e.Progress != (model.ErasureProgress{Total: 3, Destroyed: 2, Held: 1}) {
		t.Fatalf("waiting on the hold: %s %+v %+v", e.State, e.Progress, e.Items)
	}
	if s.stored(a.ObjectKey) {
		t.Fatal("the scan's bytes are still stored")
	}
	if got, err := s.ie.assets.GetByID(ctx, acme.org.ID, copied.ID); err != nil || got.Status != model.AssetStatusDeleted {
		t.Fatalf("the copy at another path: %+v %v", got, err)
	}
	if _, err := er.Certificate(ctx, owner, e.ID); !errors.Is(err, model.ErrErasureState) {
		t.Fatalf("a certificate before the end: %v", err)
	}

	// The hold is released; the erasure finishes and is certified.
	if err := s.lifecycle.ReleaseHoldFor(ctx, owner, hold.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.lifecycle.ResumeAfterHold(ctx, acme.org.ID, hold.ID); err != nil {
		t.Fatal(err)
	}
	s.runDeletions(t)
	if n, err := er.Progress(ctx); err != nil || n != 1 {
		t.Fatalf("progress: %d %v", n, err)
	}
	e, _ = er.Get(ctx, owner, e.ID)
	if e.State != model.ErasureCompleted || len(e.Selector.SubjectIDs) != 0 || len(e.SubjectHashes) != 1 {
		t.Fatalf("completed, subjects kept only as hashes: %+v", e)
	}
	cert, err := er.Certificate(ctx, acme.auditor, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.StdEncoding.DecodeString(cert.PublicKey)
	sig, _ := base64.StdEncoding.DecodeString(cert.Signature)
	if !ed25519.Verify(pub, []byte(cert.Body), sig) {
		t.Fatal("the certificate's signature does not verify")
	}
	var body struct {
		Institution string              `json:"institution"`
		Destroyed   []json.RawMessage   `json:"destroyed"`
		Superseded  []model.Successor   `json:"snapshots_superseded"`
		Models      []model.PlanModel   `json:"models"`
		Tombstone   struct{ Seq int64 } `json:"audit_tombstone"`
		NotSearched []string            `json:"not_searched"`
	}
	if err := json.Unmarshal([]byte(cert.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Institution != "Example Biobank" || len(body.Destroyed) != 3 || len(body.Superseded) != 2 || len(body.Models) != 1 ||
		body.Tombstone.Seq == 0 || len(body.NotSearched) < 3 || bytes.Contains([]byte(cert.Body), []byte("P-1")) {
		t.Fatalf("certificate: %s", cert.Body)
	}

	// A takedown by content hash: the book, wherever it is.
	sum := sha256.Sum256(textData("a pirated book", 500))
	takedown, err := er.Create(ctx, owner, erasure.CreateInput{Kind: "COPYRIGHT", Reason: "settlement: destroy and any copies",
		Selector: model.ErasureSelector{SHA256s: []string{hex.EncodeToString(sum[:])}}})
	if err != nil || len(takedown.Plan.Objects) != 1 {
		t.Fatalf("takedown plan: %+v %v", takedown.Plan, err)
	}
	if _, err := er.Execute(ctx, owner, takedown.ID); err != nil {
		t.Fatal(err)
	}
	s.runDeletions(t)
	if _, err := er.Progress(ctx); err != nil {
		t.Fatal(err)
	}
	if takedown, _ = er.Get(ctx, owner, takedown.ID); takedown.State != model.ErasureCompleted {
		t.Fatalf("takedown: %s %+v", takedown.State, takedown.Items)
	}
	ds, _ = s.datasets.Get(ctx, dev, d.ID)
	current, _ := s.datasets.Version(ctx, dev, d.ID, ds.CurrentVersionID)
	if !slices.Equal(memberPaths(t, s, dev, current), []string{"cohort/p2-scan.txt"}) {
		t.Fatalf("the current snapshot after both: %v", memberPaths(t, s, dev, current))
	}
}
