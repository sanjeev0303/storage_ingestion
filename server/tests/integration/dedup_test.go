package integration

import (
	"context"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
)

// Identical bytes of one organization are stored once, and deleting either
// copy first never loses the other's bytes.
func TestDedupStoresIdenticalBytesOnce(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	acme := s.team(t, "dedup-a")
	globex := s.team(t, "dedup-b")
	dev := acme.developer
	data := []byte("the same twelve kilobytes, give or take")

	first := s.availableAsset(t, dev, "first.txt", data)
	second := s.availableAsset(t, dev, "second.txt", data)
	if first.SharedFrom != "" || second.SharedFrom != first.ID || second.ObjectKey != first.ObjectKey {
		t.Fatalf("second copy: shared from %q key %q, first key %q", second.SharedFrom, second.ObjectKey, first.ObjectKey)
	}
	if s.stored("published/" + acme.org.ID + "/" + second.ID) {
		t.Fatal("a copy was written for identical bytes")
	}
	s.mustDownload(t, dev, second.ID, data)
	if other := s.availableAsset(t, dev, "other.txt", []byte("different bytes")); other.SharedFrom != "" {
		t.Fatalf("different bytes were shared: %+v", other)
	}
	if theirs := s.availableAsset(t, globex.developer, "same.txt", data); theirs.SharedFrom != "" {
		t.Fatal("bytes were shared across organizations")
	}

	deleteNow := func(a model.Asset) {
		t.Helper()
		if _, err := s.lifecycle.RequestDeletion(ctx, dev, a.ID); err != nil {
			t.Fatal(err)
		}
		var cycle assetservice.CycleStats
		if err := s.lifecycle.ProcessDeletions(ctx, &cycle); err != nil || cycle.Completed != 1 {
			t.Fatalf("deletion of %s: %+v %v", a.ID, cycle, err)
		}
	}

	// The original goes first: the bytes stay for the copy that shares them.
	deleteNow(first)
	if !s.stored(first.ObjectKey) {
		t.Fatal("deleting the original removed bytes another asset still uses")
	}
	got, err := s.ie.assets.GetByID(ctx, acme.org.ID, second.ID)
	if err != nil || got.Status != model.AssetStatusAvailable {
		t.Fatalf("the sharing asset after the original's deletion: %+v %v", got, err)
	}
	s.mustDownload(t, dev, second.ID, data)

	// A new copy now finds the sharer as its twin; it still names the asset
	// whose key holds the bytes.
	third := s.availableAsset(t, dev, "third.txt", data)
	if third.SharedFrom != first.ID || third.ObjectKey != first.ObjectKey {
		t.Fatalf("third copy: shared from %q key %q", third.SharedFrom, third.ObjectKey)
	}
	s.mustDownload(t, dev, third.ID, data)

	// Its last users gone, the bytes go too.
	deleteNow(second)
	if !s.stored(first.ObjectKey) {
		t.Fatal("deleting one sharer removed bytes another still uses")
	}
	deleteNow(third)
	if s.stored(first.ObjectKey) {
		t.Fatal("bytes outlived their last user")
	}

	// The other order: the sharing copy first, then the original.
	data2 := []byte("a second pair of identical files")
	original := s.availableAsset(t, dev, "pair-1.txt", data2)
	sharer := s.availableAsset(t, dev, "pair-2.txt", data2)
	if sharer.SharedFrom != original.ID {
		t.Fatalf("pair not shared: %+v", sharer)
	}
	deleteNow(sharer)
	if !s.stored(original.ObjectKey) {
		t.Fatal("deleting the sharing copy removed the original's bytes")
	}
	deleteNow(original)
	if s.stored(original.ObjectKey) {
		t.Fatal("the original's bytes outlived it")
	}

	// A new upload of the same bytes now stores them again.
	again := s.availableAsset(t, dev, "again.txt", data)
	if again.SharedFrom != "" || !s.stored(again.ObjectKey) {
		t.Fatalf("after every copy was deleted: %+v", again)
	}
}

// Identical files of one batch are promoted at the same moment; they still
// end up stored once.
func TestDedupSharesIdenticalFilesPromotedTogether(t *testing.T) {
	s := openSecurityStack(t, nil)
	ctx := context.Background()
	dev := s.team(t, "dedup-batch").developer
	data := []byte("four identical files in one batch")

	var uploads []*model.UploadSession
	for _, name := range []string{"b1.txt", "b2.txt", "b3.txt", "b4.txt"} {
		u := s.upload(t, dev, name, data)
		if _, err := s.ie.single.CompleteFor(ctx, dev, u.ID); err != nil {
			t.Fatal(err)
		}
		uploads = append(uploads, u)
	}

	errs := make(chan error, len(uploads))
	for _, u := range uploads {
		go func() {
			_, err := s.ie.proc.Intake(ctx, dev.TenantID, u.ID)
			errs <- err
		}()
	}
	for range uploads {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	originals, shared := 0, 0
	var key string
	for _, u := range uploads {
		a, err := s.ie.assets.GetByUpload(ctx, dev.TenantID, u.ID)
		if err != nil || a.Status != model.AssetStatusAvailable {
			t.Fatalf("asset of %s: %+v %v", u.ID, a, err)
		}
		if key == "" {
			key = a.ObjectKey
		}
		if a.ObjectKey != key {
			t.Fatalf("identical files stored under %s and %s", key, a.ObjectKey)
		}
		if a.SharedFrom == "" {
			originals++
		} else {
			shared++
		}
	}
	if originals != 1 || shared != 3 {
		t.Fatalf("%d stored, %d shared; want 1 and 3", originals, shared)
	}
}
