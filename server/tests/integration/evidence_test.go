package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/access"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
	"github.com/sanjeev0303/object-storage-platform/internal/service/evidence"
	"github.com/sanjeev0303/object-storage-platform/internal/service/streaming"
)

func zipFiles(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range z.File {
		r, _ := f.Open()
		out[f.Name], _ = io.ReadAll(r)
		r.Close()
	}
	return out
}

// Anchors outside the database, audit export, exception reviews with
// sign-off, the lawful-basis gate and its evidence, evidence packs,
// snapshot holds, model training summaries and control mappings.
func TestEvidenceAndAttestation(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "evidence")
	rig := s.packingRig(t)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	signer := streaming.Signer{KeyID: "it-ev", Key: key}
	stream := streaming.New(streaming.Deps{Datasets: s.datasets, Delivery: s.delivery, Store: postgres.NewStreamingRepository(s.db),
		Packs: postgres.NewPackRepository(s.db), Audit: s.audit, Signer: signer})
	accessRepo := postgres.NewAccessRepository(s.db)
	ev := evidence.New(evidence.Deps{Store: postgres.NewEvidenceRepository(s.db), Chains: postgres.NewAuditRepository(s.db), Audit: s.audit,
		Router: rig.router, Datasets: s.datasets, Delivery: s.delivery, Streaming: stream, Access: access.NewService(accessRepo, s.audit, s.log),
		Holds: s.lifecycle, Alerts: accessRepo, Log: s.log, Signer: evidence.Signer{KeyID: "it-ev", Key: key}})
	s.datasets.GateProvenance(ev)
	t.Cleanup(func() { s.datasets.GateProvenance(nil) })

	files := s.publishObjects(t, acme.developer, "ev", 3)
	d := s.newDataset(t, acme.developer, "evidence-set")
	for _, a := range files {
		s.addMembers(t, acme.developer, d.ID, memberOf(a, "", ""))
	}

	// The gate: no snapshot before the lawful basis.
	st := model.DefaultEvidenceSettings(acme.org.ID)
	st.AnchorConnectionID, st.RequireLawfulBasis = model.PlatformConnectionID, true
	if _, err := ev.PutSettings(ctx, acme.developer, st); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("settings need org.manage: %v", err)
	}
	if _, err := ev.PutSettings(ctx, acme.owner, st); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.datasets.Snapshot(ctx, acme.developer, d.ID, ""); !errors.Is(err, model.ErrProvenanceRequired) {
		t.Fatalf("a snapshot waits for the lawful basis: %v", err)
	}
	if _, err := ev.PutProvenance(ctx, acme.owner, d.ID, model.Provenance{LawfulBasis: "consent (GDPR 6(1)(a))", License: "CC-BY-4.0",
		Source: "clinic intake 2026", SourceKind: "USER_DATA"}); err != nil {
		t.Fatal(err)
	}
	v, _, err := s.datasets.Snapshot(ctx, acme.developer, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.datasets.RecordTrainingRun(ctx, acme.developer, datasetservice.TrainingRunInput{Name: "ev-run", Source: "SDK",
		VersionIDs: []string{v.ID}, Model: &datasetservice.ModelInput{Name: "ev-model", Version: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	order, err := ev.RunOrder(ctx, acme.developer, run.ID)
	if err != nil || !order.InOrder || len(order.Steps) != 3 || order.Steps[0].AuditSeq == 0 {
		t.Fatalf("basis, snapshot, run in order: %+v %v", order, err)
	}

	// A snapshot taken before the basis cannot feed a run.
	late := s.newDataset(t, acme.developer, "late-basis")
	s.addMembers(t, acme.developer, late.ID, memberOf(files[0], "", ""))
	st.RequireLawfulBasis = false
	_, _ = ev.PutSettings(ctx, acme.owner, st)
	lv, _, err := s.datasets.Snapshot(ctx, acme.developer, late.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	st.RequireLawfulBasis = true
	_, _ = ev.PutSettings(ctx, acme.owner, st)
	time.Sleep(10 * time.Millisecond)
	_, _ = ev.PutProvenance(ctx, acme.owner, late.ID, model.Provenance{LawfulBasis: "contract", License: "internal", Source: "x", SourceKind: "OTHER"})
	if _, _, err := s.datasets.RecordTrainingRun(ctx, acme.developer, datasetservice.TrainingRunInput{Name: "late", Source: "SDK",
		VersionIDs: []string{lv.ID}}); !errors.Is(err, model.ErrProvenanceOrder) {
		t.Fatalf("a run over a snapshot older than its basis is refused: %v", err)
	}

	// Anchors: signed, in the bucket, checked three ways.
	anchor, err := ev.AnchorNow(ctx, acme.auditor)
	if err != nil || anchor.Signature == "" || anchor.ObjectKey == "" {
		t.Fatalf("anchor: %+v %v", anchor, err)
	}
	checks, err := ev.Anchors(ctx, acme.auditor, 10)
	if err != nil || len(checks) != 1 || !checks[0].SignatureOK || !checks[0].ChainOK || checks[0].StoredOK == nil || !*checks[0].StoredOK {
		t.Fatalf("anchor checks: %+v %v", checks, err)
	}
	stored, _ := s.ie.storage.Object(anchor.ObjectKey)
	s.ie.storage.PutObject(anchor.ObjectKey, []byte(`{"body":"forged"}`))
	checks, _ = ev.Anchors(ctx, acme.auditor, 10)
	if *checks[0].StoredOK {
		t.Fatal("a changed anchor in the bucket is caught")
	}
	s.ie.storage.PutObject(anchor.ObjectKey, stored)

	// The export chains: each entry's previous hash is the one before it.
	events, err := ev.Export(ctx, acme.auditor, 0, time.Time{}, time.Time{}, 1000)
	if err != nil || len(events) < 5 {
		t.Fatalf("export: %d %v", len(events), err)
	}
	for i := 1; i < len(events); i++ {
		if events[i].PrevHash != events[i-1].Hash || events[i].Seq != events[i-1].Seq+1 {
			t.Fatalf("entry %d does not chain to %d", events[i].Seq, events[i-1].Seq)
		}
	}
	if line := evidence.CEF(events[0]); !strings.HasPrefix(line, "CEF:0|OSP|") || !strings.Contains(line, "cs3="+events[0].Hash) {
		t.Fatalf("cef: %s", line)
	}

	// A review of the period finds the retention and permission changes;
	// its sign-off goes into the chain.
	if _, err := s.lifecycle.PlaceHoldFor(ctx, acme.owner, model.LegalHold{ID: "ev-hold-" + acme.org.ID, Scope: model.ScopeAsset,
		ScopeKey: files[2].ID, Reason: "case", PlacedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	review, err := ev.CreateReview(ctx, acme.auditor, time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
	if err != nil || review.Counts["RETENTION_CHANGE"] == 0 || review.State != "OPEN" || len(review.ReportSHA256) != 64 {
		t.Fatalf("review: %+v %v", review.Counts, err)
	}
	signed, err := ev.Sign(ctx, acme.auditor, review.ID, "looked at every change")
	if err != nil || signed.State != "SIGNED" || signed.SignedBy != acme.auditor.UserID {
		t.Fatalf("sign: %+v %v", signed, err)
	}
	if _, err := ev.Sign(ctx, acme.auditor, review.ID, ""); !errors.Is(err, model.ErrReviewSigned) {
		t.Fatalf("a review is signed once: %v", err)
	}
	after, _ := ev.Export(ctx, acme.auditor, events[len(events)-1].Seq, time.Time{}, time.Time{}, 1000)
	found := false
	for _, e := range after {
		if e.Action == "audit.review_signed" && strings.Contains(string(e.Details), review.ReportSHA256) {
			found = true
		}
	}
	if !found {
		t.Fatal("the sign-off is in the chain with the report's hash")
	}
	if z, err := ev.ReviewExport(ctx, acme.auditor, review.ID); err != nil || len(zipFiles(t, z)["audit.jsonl"]) == 0 {
		t.Fatalf("review export: %v", err)
	}

	// The evidence pack checks out on its own.
	var pack bytes.Buffer
	if err := ev.WritePack(ctx, acme.owner, d.ID, v.ID, evidence.PackOptions{Digests: true}, &pack); err != nil {
		t.Fatal(err)
	}
	in := zipFiles(t, pack.Bytes())
	sum := sha256.Sum256(in["manifest.jsonl"])
	if hex.EncodeToString(sum[:]) != v.ManifestSHA256 {
		t.Fatal("manifest.jsonl hashes to the snapshot's manifest hash")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(in["SHA256SUMS"])), "\n") {
		want, name, _ := strings.Cut(line, "  ")
		got := sha256.Sum256(in[name])
		if hex.EncodeToString(got[:]) != want {
			t.Fatalf("SHA256SUMS %s", name)
		}
	}
	var custody map[string]any
	_ = json.Unmarshal([]byte(strings.SplitN(string(in["custody.jsonl"]), "\n", 2)[0]), &custody)
	if custody["md5"] == "" || custody["sha1"] == "" || custody["uploaded_by"] == "" || custody["control_number"] != "EVIDENCE-SET-00000001" {
		t.Fatalf("custody: %+v", custody)
	}
	if !bytes.HasPrefix(in["loadfile.dat"], []byte("\ufeff\u00feBEGDOC\u00fe\x14")) || !strings.Contains(string(in["loadfile.opt"]), ",VOL001,NATIVES\\") {
		t.Fatalf("load files: %q", in["loadfile.dat"][:40])
	}

	// A FOIA hold over the snapshot holds every member.
	hold, err := ev.HoldSnapshot(ctx, acme.owner, d.ID, v.ID, "FOIA", "2026-0042", "request from a journalist")
	if err != nil || len(hold.Holds) != 3 {
		t.Fatalf("snapshot hold: %+v %v", hold, err)
	}
	if active, err := postgres.NewLegalHoldRepository(s.db).Active(ctx, acme.org.ID, "", files[0].ID); err != nil || len(active) == 0 {
		t.Fatalf("a member is under the hold, so it is never purged: %v %v", active, err)
	}
	groups, _ := ev.SnapshotHolds(ctx, acme.owner, d.ID, v.ID)
	if len(groups) != 1 || groups[0].Kind != "FOIA" || groups[0].Released {
		t.Fatalf("holds: %+v", groups)
	}
	if n, err := ev.ReleaseSnapshotHold(ctx, acme.owner, d.ID, v.ID, hold.Matter, "request closed"); err != nil || n != 3 {
		t.Fatalf("release: %d %v", n, err)
	}

	// The model's training summary walks its lineage.
	summary, err := ev.ModelSummary(ctx, acme.developer, run.Models[0].ID)
	if err != nil || len(summary.Sources["2.4 User data"]) != 1 || summary.Sources["2.4 User data"][0].License != "CC-BY-4.0" {
		t.Fatalf("summary: %+v %v", summary, err)
	}
	if md := summary.Markdown(); !strings.Contains(md, "evidence-set") || !strings.Contains(md, "Next update due") {
		t.Fatal(md)
	}

	// Controls, their evidence, the undertaking and regulator access.
	fw, err := ev.Compliance(ctx, acme.auditor, "nist-800-171")
	if err != nil || len(fw.Controls) < 8 {
		t.Fatalf("controls: %+v %v", fw, err)
	}
	var zipped bytes.Buffer
	if err := ev.WriteComplianceEvidence(ctx, acme.auditor, "21-cfr-11", &zipped); err != nil || len(zipFiles(t, zipped.Bytes())["members.json"]) == 0 {
		t.Fatalf("compliance evidence: %v", err)
	}
	if letter, err := ev.Undertaking(ctx, acme.auditor, "DEO", "A. Officer", "CCO", "Acme Securities"); err != nil || !strings.Contains(letter, "17a-4(f)(3)(vii)") {
		t.Fatalf("undertaking: %v", err)
	}
	tok, secret, err := ev.RegulatorAccess(ctx, acme.owner, "FINRA examiner", 7, nil)
	if err != nil || !strings.HasPrefix(secret, "osp_pat_") || !strings.HasPrefix(tok.Name, "Regulator: ") {
		t.Fatalf("regulator access: %+v %v", tok, err)
	}
	if _, _, err := ev.RegulatorAccess(ctx, acme.auditor, "FINRA examiner", 7, nil); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("an auditor issued regulator access: %v", err)
	}
	asOf, err := ev.AsOf(ctx, acme.developer, "", time.Now())
	if err != nil || len(asOf) < 3 {
		t.Fatalf("as of: %d %v", len(asOf), err)
	}
}
