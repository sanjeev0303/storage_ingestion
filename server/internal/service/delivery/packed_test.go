package delivery

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// fakePacks answers PackedLocation from a fixed table.
type fakePacks struct {
	at  map[string]model.PackedLocation
	err error
}

func (f *fakePacks) PackedLocation(_ context.Context, tenantID, assetID string) (model.PackedLocation, bool, error) {
	if f.err != nil {
		return model.PackedLocation{}, false, f.err
	}
	loc, ok := f.at[tenantID+"/"+assetID]
	return loc, ok, nil
}

const packKey = "assets/tenant-1/packs/p1.pack"

// packedEnv packs a1 between two neighbours and deletes its standalone
// object, so every byte served must come from its entry in the pack.
func packedEnv(t *testing.T, cdn *CDNConfig) (*env, *fakePacks) {
	t.Helper()
	e := newEnv(t, cdn)
	e.asset(t, "a1", model.AssetStatusAvailable)
	pack := append(append([]byte("AAAA"), content...), []byte("ZZZZ")...)
	e.storage.PutObject(packKey, pack)
	if err := e.storage.DeleteObject(context.Background(), "private-bucket", "assets/tenant-1/a1"); err != nil {
		t.Fatal(err)
	}
	packs := &fakePacks{at: map[string]model.PackedLocation{"tenant-1/a1": {
		PackID: "p1", ConnectionID: model.PlatformConnectionID, Bucket: "private-bucket", ObjectKey: packKey,
		StorageETag: e.storage.ETag(packKey), PackBytes: int64(len(pack)), Offset: 4, Length: int64(len(content)),
	}}}
	e.svc.Packed(packs)
	return e, packs
}

func TestPackedAssetIsReadFromItsEntryOnly(t *testing.T) {
	e, _ := packedEnv(t, nil)
	ctx := context.Background()

	d, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, d); got != string(content) || d.TotalSize != int64(len(content)) || d.ETag != entityTag(model.Asset{ChecksumSHA256: contentSum}) {
		t.Fatalf("a whole read is exactly the asset: %q %+v", got, d)
	}

	ranges := map[string]string{
		"bytes=0-3":  "0123",
		"bytes=2-5":  "2345",
		"bytes=15-":  "fghij", // to the entry's end, not the pack's
		"bytes=-3":   "hij",
		"bytes=0-99": string(content),
	}
	for header, want := range ranges {
		d, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{Range: header})
		if err != nil {
			t.Fatalf("%s: %v", header, err)
		}
		if got := readAll(t, d); got != want {
			t.Fatalf("%s: got %q, want %q", header, got, want)
		}
	}
	if _, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{Range: "bytes=20-23"}); !errors.Is(err, ErrRangeNotSatisfiable) {
		t.Fatalf("a range past the asset never reaches its neighbour: %v", err)
	}

	head, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{HeadOnly: true})
	if err != nil || head.Body != nil || head.TotalSize != int64(len(content)) {
		t.Fatalf("HEAD checks the pack and reads nothing: %+v %v", head, err)
	}
}

func TestPackedAssetGetsATokenLinkNeverAPackURL(t *testing.T) {
	signer := &recordingSigner{}
	e, _ := packedEnv(t, &CDNConfig{BaseURL: "https://cdn.example.test", Signer: signer})
	for _, mode := range []string{"", ModeStorage} {
		link, err := e.svc.CreateDownloadURL(context.Background(), owner, "a1", URLRequest{Mode: mode})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if link.Via != ModeToken {
			t.Fatalf("mode %q: a URL to the pack would expose its other entries: %+v", mode, link)
		}
	}
	if len(e.storage.Presigns) != 0 || len(signer.signed) != 0 {
		t.Fatal("nothing may sign a URL to a pack")
	}
}

func TestDamagedPackFailsTheReadInsteadOfServingWrongBytes(t *testing.T) {
	e, packs := packedEnv(t, nil)
	ctx := context.Background()

	// The pack's bytes changed under an unpinned entry: a whole read ends
	// with an error, never a clean end of the wrong bytes.
	damaged := append(append([]byte("AAAA"), []byte("0123456789abcdefghiX")...), []byte("ZZZZ")...)
	e.storage.PutObject(packKey, damaged)
	loc := packs.at["tenant-1/a1"]
	loc.StorageETag = ""
	packs.at["tenant-1/a1"] = loc
	d, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(d.Body)
	d.Body.Close()
	if !errors.Is(err, model.ErrPackCorrupt) {
		t.Fatalf("the checksum catches it: %v", err)
	}

	// Pinned to the pack as written, a replaced pack is not read at all.
	loc.StorageETag = `"written"`
	packs.at["tenant-1/a1"] = loc
	if _, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{}); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("a replaced pack: %v", err)
	}

	// An entry that disagrees with its asset is refused.
	loc.StorageETag, loc.Length = e.storage.ETag(packKey), 19
	packs.at["tenant-1/a1"] = loc
	if _, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{}); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("a mismatched entry: %v", err)
	}

	packs.err = errors.New("database down")
	if _, err := e.svc.Open(ctx, owner, "a1", DownloadRequest{}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("a failed lookup is unavailable, not a read of the standalone key: %v", err)
	}
}
