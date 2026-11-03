package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/fastcdc"
	"github.com/sanjeev0303/object-storage-platform/internal/service/deltaupload"
)

func cutChunks(t *testing.T, data []byte) []deltaupload.ChunkInput {
	t.Helper()
	c := fastcdc.New(bytes.NewReader(data))
	out := []deltaupload.ChunkInput{}
	var at int64
	for {
		b, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		out = append(out, deltaupload.ChunkInput{Offset: at, Length: len(b), SHA256: hex.EncodeToString(sum[:])})
		at += int64(len(b))
	}
}

// Only what differs is sent; the file built goes through the ordinary pipeline;
// a client gets at chunks only through a snapshot it can read.
func TestDeltaUpload(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	rig := s.packingRig(t)
	acme := s.team(t, "delta")
	dev, owner := acme.developer, acme.owner
	other := s.team(t, "delta-other").owner
	svc := deltaupload.New(deltaupload.Deps{Store: postgres.NewDeltaUploadRepository(s.db), Datasets: s.datasets, Uploads: s.ie.single,
		Assets: postgres.NewAssetRepository(s.db), Packs: postgres.NewPackRepository(s.db), Router: rig.router, Audit: s.audit, Log: s.log})

	build1 := randomBytes(21, 12<<20)
	build2 := append(append(append([]byte{}, build1[:6<<20]...), []byte("a small patch")...), build1[6<<20:]...)
	d := s.newDataset(t, dev, "delta-set")
	a1 := s.publishAt(t, dev, "game.bin", build1, inNamespace("delta-set"))
	s.addMembers(t, dev, d.ID, memberOf(a1, "", ""))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.datasets.SetChunking(ctx, owner, d.ID, true); err != nil {
		t.Fatal(err)
	}
	for {
		found, err := s.datasets.ChunkOnce(ctx, "worker")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
	}

	input := deltaupload.CreateInput{DatasetID: d.ID, BaseVersionID: v1.ID, Path: "game.bin", SizeBytes: int64(len(build2)), SHA256: sha256Of(build2),
		ContentType: "application/octet-stream", Chunks: cutChunks(t, build2)}

	// Refusals.
	bad := input
	bad.Chunks = append([]deltaupload.ChunkInput{}, input.Chunks...)
	bad.Chunks[1].Offset++
	if _, err := svc.Create(ctx, dev, bad); !errors.Is(err, model.ErrInvalidDelta) {
		t.Fatalf("chunks with a gap: %v", err)
	}
	bad = input
	bad.SizeBytes++
	if _, err := svc.Create(ctx, dev, bad); !errors.Is(err, model.ErrInvalidDelta) {
		t.Fatalf("chunks that do not cover the file: %v", err)
	}
	if _, err := svc.Create(ctx, other, input); err == nil {
		t.Fatal("another organization used this snapshot as a base")
	}

	created, err := svc.Create(ctx, dev, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.ReusedBytes < int64(len(build2))/2 || created.SentBytes > int64(len(build2))/2 || len(created.Missing) == 0 || created.ReusedBytes+created.SentBytes != int64(len(build2)) {
		t.Fatalf("created: %+v", created)
	}

	// Someone else's upload is not found, and a wrong chunk is refused.
	piece := func(ordinal int) []byte {
		c := input.Chunks[ordinal]
		return build2[c.Offset : c.Offset+int64(c.Length)]
	}
	if err := svc.PutChunk(ctx, owner, created.ID, created.Missing[0], bytes.NewReader(piece(created.Missing[0]))); !errors.Is(err, model.ErrDeltaNotFound) {
		t.Fatalf("someone else's upload: %v", err)
	}
	wrong := append([]byte{}, piece(created.Missing[0])...)
	wrong[0] ^= 0xFF
	if err := svc.PutChunk(ctx, dev, created.ID, created.Missing[0], bytes.NewReader(wrong)); !errors.Is(err, model.ErrInvalidDelta) {
		t.Fatalf("a chunk that is not the one promised: %v", err)
	}
	if _, err := svc.Complete(ctx, dev, created.ID); !errors.Is(err, model.ErrDeltaState) {
		t.Fatalf("completing with chunks missing: %v", err)
	}
	reused := -1
	missing := map[int]bool{}
	for _, n := range created.Missing {
		missing[n] = true
	}
	for i := range input.Chunks {
		if !missing[i] {
			reused = i
			break
		}
	}
	if err := svc.PutChunk(ctx, dev, created.ID, reused, bytes.NewReader(piece(reused))); !errors.Is(err, model.ErrInvalidDelta) {
		t.Fatalf("a chunk not asked for: %v", err)
	}
	for _, n := range created.Missing {
		if err := svc.PutChunk(ctx, dev, created.ID, n, bytes.NewReader(piece(n))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Complete(ctx, dev, created.ID); err != nil {
		t.Fatal(err)
	}
	found, err := svc.RunOnce(ctx, "worker")
	if err != nil || !found {
		t.Fatalf("assemble: %v %v", found, err)
	}
	state, _, err := svc.Get(ctx, dev, created.ID)
	if err != nil || state.State != model.DeltaDone || state.UploadID == "" {
		t.Fatalf("state: %+v %v", state, err)
	}
	// Staged chunks are gone, and the ordinary pipeline takes it from here.
	if _, err := s.ie.proc.Intake(ctx, dev.TenantID, state.UploadID); err != nil {
		t.Fatal(err)
	}
	asset, err := postgres.NewAssetRepository(s.db).GetByUpload(ctx, dev.TenantID, state.UploadID)
	if err != nil || asset == nil || asset.Status != model.AssetStatusAvailable || asset.ChecksumSHA256 != sha256Of(build2) {
		t.Fatalf("asset: %+v %v", asset, err)
	}
	s.mustDownload(t, dev, asset.ID, build2)

	// A file promised with the wrong whole hash never becomes an upload.
	liar := input
	liar.Path = "liar.bin"
	liar.SHA256 = sha256Of([]byte("something else"))
	lied, err := svc.Create(ctx, dev, liar)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range lied.Missing {
		if err := svc.PutChunk(ctx, dev, lied.ID, n, bytes.NewReader(piece(n))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Complete(ctx, dev, lied.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOnce(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if st, _, err := svc.Get(ctx, dev, lied.ID); err != nil || st.State != model.DeltaFailed || st.UploadID != "" {
		t.Fatalf("a lying file: %+v %v", st, err)
	}

	// Knowing a hash from a dataset you cannot read gets nothing: with a base
	// that does not hold the chunks, all of them must be sent.
	s2 := s.newDataset(t, dev, "delta-empty")
	small := s.publishAt(t, dev, "other.bin", textData("other", 500), inNamespace("delta-empty"))
	s.addMembers(t, dev, s2.ID, memberOf(small, "", ""))
	v2, _, err := s.datasets.Snapshot(ctx, dev, s2.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	guess := deltaupload.CreateInput{DatasetID: s2.ID, BaseVersionID: v2.ID, Path: "copy.bin", SizeBytes: int64(len(build1)), SHA256: sha256Of(build1), Chunks: cutChunks(t, build1)}
	guessed, err := svc.Create(ctx, dev, guess)
	if err != nil || guessed.ReusedBytes != 0 || len(guessed.Missing) != len(guess.Chunks) {
		t.Fatalf("a base that does not hold the chunks: %+v %v", guessed, err)
	}

	// Abandoned uploads give their chunks' space back.
	if _, err := s.db.Pool.Exec(ctx, `UPDATE delta_uploads SET expires_at = now() - interval '1 minute' WHERE id = $1`, guessed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOnce(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := svc.Get(ctx, dev, guessed.ID); st.State != model.DeltaExpired {
		t.Fatalf("abandoned: %+v", st)
	}
}
