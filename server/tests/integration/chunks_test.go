package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	datasetservice "github.com/sanjeev0303/object-storage-platform/internal/service/dataset"
)

func randomBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// A build with a few bytes inserted shares nearly all its chunks with the one
// before; the index measures that, reads each file once, and lets go of what
// is gone.
func TestChunkIndex(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "chunks")
	dev, owner := acme.developer, acme.owner
	other := s.team(t, "chunks-other").owner

	run := func() {
		t.Helper()
		for {
			found, err := s.datasets.ChunkOnce(ctx, "worker")
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				return
			}
		}
	}

	build1 := randomBytes(1, 12<<20)
	build2 := append(append(append([]byte{}, build1[:3<<20]...), []byte("inserted patch")...), build1[3<<20:]...)
	same := randomBytes(2, 2<<20)
	small := textData("small", 2000)

	d := s.newDataset(t, dev, "chunk-set")
	a1 := s.publishAt(t, dev, "build/game.bin", build1)
	a2 := s.publishAt(t, dev, "build/assets.bin", same)
	a3 := s.publishAt(t, dev, "build/small.txt", small)
	s.addMembers(t, dev, d.ID, memberOf(a1, "", ""), memberOf(a2, "", ""), memberOf(a3, "", ""))
	v1, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	a1b := s.publishAt(t, dev, "build/game.bin", build2)
	if _, err := s.datasets.AddMembers(ctx, dev, d.ID, []datasetservice.MemberInput{memberOf(a1b, "", "")}, model.DuplicateReplace); err != nil {
		t.Fatal(err)
	}
	v2, _, err := s.datasets.Snapshot(ctx, dev, d.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	// Off by default: nothing is read until someone asks.
	run()
	if _, found, err := s.datasets.ChunkJobOf(ctx, dev, d.ID, v1.ID); err != nil || found {
		t.Fatalf("a job with chunking off: %v %v", found, err)
	}
	if err := s.datasets.SetChunking(ctx, dev, d.ID, true); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("turning it on needs datasets.manage: %v", err)
	}
	if err := s.datasets.SetChunking(ctx, owner, d.ID, true); err != nil {
		t.Fatal(err)
	}
	run()
	first, found, err := s.datasets.ChunkJobOf(ctx, dev, d.ID, v1.ID)
	if err != nil || !found || first.State != model.ChunkDone || first.FilesIndexed != 3 || len(first.Skipped) != 0 {
		t.Fatalf("v1: %+v %v", first, err)
	}
	second, _, err := s.datasets.ChunkJobOf(ctx, dev, d.ID, v2.ID)
	if err != nil || second.State != model.ChunkDone {
		t.Fatalf("v2: %+v %v", second, err)
	}
	// The small file is one chunk, known without reading it; each large file
	// is read once, in whichever version came first.
	if want := int64(len(build1) + len(build2) + len(same)); first.BytesRead+second.BytesRead != want {
		t.Fatalf("read %d, want %d", first.BytesRead+second.BytesRead, want)
	}

	// A manifest covers the file exactly, and each chunk hashes as it says.
	sha, chunks, indexed, err := s.datasets.ChunkManifest(ctx, dev, d.ID, v2.ID, "build/game.bin", 0, 0)
	if err != nil || !indexed || sha != sha256Of(build2) || len(chunks) < 3 {
		t.Fatalf("manifest: %d chunks, %v %v", len(chunks), indexed, err)
	}
	var at int64
	for i, c := range chunks {
		sum := sha256.Sum256(build2[c.Offset : c.Offset+int64(c.Length)])
		if c.Ordinal != i || c.Offset != at || c.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("chunk %d: %+v", i, c)
		}
		at += int64(c.Length)
	}
	if at != int64(len(build2)) {
		t.Fatalf("the chunks cover %d of %d bytes", at, len(build2))
	}
	if _, page, _, err := s.datasets.ChunkManifest(ctx, dev, d.ID, v2.ID, "build/game.bin", 1, 1); err != nil || len(page) != 1 || page[0].Ordinal != 1 {
		t.Fatalf("a page: %+v %v", page, err)
	}
	if _, single, _, err := s.datasets.ChunkManifest(ctx, dev, d.ID, v1.ID, "build/small.txt", 0, 0); err != nil || len(single) != 1 || single[0].SHA256 != sha256Of(small) {
		t.Fatalf("a small file: %+v %v", single, err)
	}
	if _, _, _, err := s.datasets.ChunkManifest(ctx, other, d.ID, v2.ID, "build/game.bin", 0, 0); err == nil {
		t.Fatal("another organization read a manifest")
	}

	// The savings report.
	saved, err := s.datasets.ChunkSavings(ctx, dev, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	logical := int64(2*(len(same)+len(small)) + len(build1) + len(build2))
	unique := int64(len(same) + len(small) + len(build1) + len(build2))
	if !saved.Enabled || saved.LogicalBytes != logical || saved.UniqueFileBytes != unique || saved.IndexedFileBytes != unique || saved.NotIndexedFiles != 0 {
		t.Fatalf("savings: %+v", saved)
	}
	// Most of build2 is build1: the chunks hold about one copy of it.
	if saved.WouldSaveBytes < int64(len(build1))*6/10 || saved.UniqueChunkBytes >= unique {
		t.Fatalf("savings: %+v", saved)
	}

	// The delta between the builds.
	delta, err := s.datasets.ChunkDelta(ctx, dev, d.ID, v2.ID, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if delta.ChangedFiles != 1 || delta.UnchangedFiles != 2 || delta.ChangedBytes != int64(len(build2)) || delta.NotIndexed != 0 {
		t.Fatalf("delta: %+v", delta)
	}
	if delta.FetchBytes >= int64(len(build2))*6/10 || delta.FetchBytes <= 0 || delta.FetchBytes+delta.ReusedBytes != delta.ChangedBytes {
		t.Fatalf("delta fetches too much: %+v", delta)
	}
	if len(delta.LargestChanged) != 1 || delta.LargestChanged[0].Path != "build/game.bin" {
		t.Fatalf("delta files: %+v", delta.LargestChanged)
	}

	// A file nobody holds any more goes from the index; those still held stay.
	if _, err := s.db.Pool.Exec(ctx, `UPDATE assets SET status = 'DELETED' WHERE id = $1`, a1b.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.datasets.PruneChunks(ctx); err != nil || n != 1 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	if _, _, ok, err := s.datasets.ChunkManifest(ctx, dev, d.ID, v2.ID, "build/game.bin", 0, 0); err != nil || ok {
		t.Fatalf("the chunks of a deleted file: %v %v", ok, err)
	}
	if _, _, ok, err := s.datasets.ChunkManifest(ctx, dev, d.ID, v1.ID, "build/game.bin", 0, 0); err != nil || !ok {
		t.Fatalf("the chunks of a file still held: %v %v", ok, err)
	}
	if err := s.datasets.IndexVersion(ctx, owner, d.ID, v1.ID); err != nil {
		t.Fatal(err)
	}
	if job, _, _ := s.datasets.ChunkJobOf(ctx, dev, d.ID, v1.ID); job.State != model.ChunkPending {
		t.Fatalf("asked for again: %+v", job)
	}
}
