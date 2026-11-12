package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/edge"
)

// A device's credential writes only under its prefix and reads nothing; a
// run snapshots only when every hash matches; a recording knows its gaps,
// and a late chunk closes one.
func TestEdgeDevices(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "edge")
	owner := acme.owner
	svc := edge.New(edge.Deps{Store: postgres.NewEdgeRepository(s.db), Access: s.access, Datasets: s.datasets, Audit: s.audit, Log: s.log})
	ds := s.newDataset(t, owner, "instruments")

	if _, _, err := svc.Register(ctx, acme.developer, "novaseq", ds.ID, ""); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("registering needs org.manage: %v", err)
	}
	dev, token, err := svc.Register(ctx, owner, "novaseq 1", ds.ID, "")
	if err != nil || !strings.HasPrefix(token, "orb_pat_") || dev.PathPrefix != "devices/novaseq-1/" {
		t.Fatalf("register: %+v %v", dev, err)
	}
	if _, _, err := svc.Register(ctx, owner, "novaseq 1", ds.ID, ""); !errors.Is(err, model.ErrInvalidEdge) {
		t.Fatalf("a repeated name: %v", err)
	}
	// What the token resolves to, as the API would see it.
	me := model.Principal{TenantID: owner.TenantID, UserID: owner.UserID, Permissions: model.PermUploadsWrite,
		Scope: model.Scope{TokenID: dev.TokenID, Datasets: []string{ds.Name}, Restricted: true, NoLinks: true, PathPrefix: dev.PathPrefix}}

	// Upload-only, and only under its prefix.
	inside := catalogUpload(dev.PathPrefix+"run-1/a.txt", textData("run one a", 100))
	inside.Dataset = ds.Name
	a := s.startUpload(t, me, inside, textData("run one a", 100))
	outside := catalogUpload("elsewhere/x.txt", textData("x", 50))
	outside.Dataset = ds.Name
	if _, err := s.ie.single.CreateFor(ctx, me, outside); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("outside its prefix: %v", err)
	}
	climbing := catalogUpload(dev.PathPrefix+"../../x.txt", textData("x", 50))
	climbing.Dataset = ds.Name
	if _, err := s.ie.single.CreateFor(ctx, me, climbing); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("climbing out of its prefix: %v", err)
	}
	other := catalogUpload(dev.PathPrefix+"x.txt", textData("x", 50))
	other.Dataset = "someone-elses"
	if _, err := s.ie.single.CreateFor(ctx, me, other); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("another namespace: %v", err)
	}
	if _, err := s.datasets.Get(ctx, me, ds.ID); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("reading: %v", err)
	}
	if err := svc.Heartbeat(ctx, owner, model.EdgeHeartbeat{}); !errors.Is(err, model.ErrNotADevice) {
		t.Fatalf("a person is not a device: %v", err)
	}

	// The heartbeat shows what waits.
	oldest := time.Now().Add(-time.Hour)
	if err := svc.Heartbeat(ctx, me, model.EdgeHeartbeat{PendingFiles: 3, PendingBytes: 4096, OldestPending: &oldest, AgentVersion: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	devices, err := svc.Devices(ctx, owner)
	if err != nil || devices[0].PendingFiles != 3 || devices[0].LastSeen == nil || devices[0].AgentVersion != "0.1.0" {
		t.Fatalf("devices: %+v %v", devices, err)
	}

	// Confirm says the truth before and after processing.
	sum := sha256Of(textData("run one a", 100))
	if st, _ := svc.Confirm(ctx, me, inside.Path, sum); st.State == "AVAILABLE" {
		t.Fatalf("confirmed before processing: %+v", st)
	}
	s.finishUpload(t, me, a)
	if st, err := svc.Confirm(ctx, me, inside.Path, sum); err != nil || st.State != "AVAILABLE" {
		t.Fatalf("confirm: %+v %v", st, err)
	}
	if st, _ := svc.Confirm(ctx, me, inside.Path, sha256Of([]byte("other bytes"))); st.State != "MISSING" {
		t.Fatalf("other bytes: %+v", st)
	}
	if _, err := svc.Confirm(ctx, me, "elsewhere/x.txt", sum); !errors.Is(err, model.ErrOutsideDevicePath) {
		t.Fatalf("another path: %v", err)
	}

	// A run: incomplete while a file is missing, complete when it arrives.
	run, err := svc.OpenRecording(ctx, me, "run-1", "run", 0)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := svc.OpenRecording(ctx, me, "run-1", "RUN", 0)
	if again.ID != run.ID {
		t.Fatal("opening again made another")
	}
	if _, err := svc.AddChunk(ctx, me, run.ID, model.EdgeChunk{Path: inside.Path, SHA256: sum, SizeBytes: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddChunk(ctx, me, run.ID, model.EdgeChunk{Path: inside.Path, SHA256: sum, SizeBytes: 100}); err != nil {
		t.Fatalf("sent twice counts once: %v", err)
	}
	if _, err := svc.AddChunk(ctx, me, run.ID, model.EdgeChunk{Path: inside.Path, SHA256: sha256Of([]byte("x")), SizeBytes: 1}); !errors.Is(err, model.ErrChunkDiffers) {
		t.Fatalf("other bytes at that path: %v", err)
	}
	second := catalogUpload(dev.PathPrefix+"run-1/b.txt", textData("run one b", 100))
	second.Dataset = ds.Name
	bSum := sha256Of(textData("run one b", 100))
	if _, err := svc.AddChunk(ctx, me, run.ID, model.EdgeChunk{Path: second.Path, SHA256: bSum, SizeBytes: 100}); err != nil {
		t.Fatal(err)
	}
	closed, err := svc.Close(ctx, me, run.ID, 2, nil)
	if err != nil || closed.State != model.RecordingIncomplete || closed.VersionID != "" || !strings.Contains(closed.Missing[0], "not uploaded") {
		t.Fatalf("a run with a file missing: %+v %v", closed.EdgeRecording, err)
	}
	if v, _ := s.datasets.Versions(ctx, owner, ds.ID, 0, 10); len(v) != 0 {
		t.Fatal("snapshotted with a file missing")
	}
	s.finishUpload(t, me, s.startUpload(t, me, second, textData("run one b", 100)))
	closed, err = svc.Close(ctx, me, run.ID, 2, nil)
	if err != nil || closed.State != model.RecordingComplete || closed.VersionID == "" || len(closed.Missing) != 0 {
		t.Fatalf("complete: %+v %v", closed.EdgeRecording, err)
	}
	v, err := s.datasets.Version(ctx, owner, ds.ID, closed.VersionID)
	if err != nil || v.MemberCount != 2 {
		t.Fatalf("the run's snapshot: %+v %v", v, err)
	}
	if done, err := svc.Close(ctx, me, run.ID, 2, nil); err != nil || done.VersionID != closed.VersionID {
		t.Fatalf("closing twice: %+v %v", done.EdgeRecording, err)
	}

	// A recording: a late chunk closes a gap, and says so.
	rec, err := svc.OpenRecording(ctx, me, "drive", "RECORDING", 5)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	window := func(from, to int) (*time.Time, *time.Time) {
		a, b := base.Add(time.Duration(from)*time.Minute), base.Add(time.Duration(to)*time.Minute)
		return &a, &b
	}
	chunk := func(name string, from, to int) model.EdgeChunk {
		start, end := window(from, to)
		return model.EdgeChunk{Path: dev.PathPrefix + name, SHA256: sha256Of([]byte(name)), SizeBytes: 10, StartsAt: start, EndsAt: end}
	}
	if _, err := svc.AddChunk(ctx, me, rec.ID, chunk("c0", 0, 1)); err != nil {
		t.Fatal(err)
	}
	d, err := svc.AddChunk(ctx, me, rec.ID, chunk("c2", 2, 3))
	if err != nil || len(d.Gaps) != 1 || d.Chunks != 2 {
		t.Fatalf("a gap: %+v %v", d, err)
	}
	if d, err = svc.AddChunk(ctx, me, rec.ID, chunk("c1", 1, 2)); err != nil || len(d.Gaps) != 0 {
		t.Fatalf("the backfill: %+v %v", d, err)
	}
	bad := chunk("c9", 5, 4)
	if _, err := svc.AddChunk(ctx, me, rec.ID, bad); !errors.Is(err, model.ErrInvalidEdge) {
		t.Fatalf("a window ending before it starts: %v", err)
	}

	// Revoked: the device is not one any more.
	if err := svc.Revoke(ctx, owner, dev.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Heartbeat(ctx, me, model.EdgeHeartbeat{}); !errors.Is(err, model.ErrNotADevice) {
		t.Fatalf("after revoking: %v", err)
	}
}
