package compaction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func sources(contents ...string) []Source {
	var out []Source
	for i, c := range contents {
		data := []byte(c)
		out = append(out, Source{
			AssetID: "asset-" + string(rune('a'+i)), Size: int64(len(data)), SHA256: sum(data),
			Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		})
	}
	return out
}

func manifestOf(t *testing.T, contents ...string) (Manifest, []byte) {
	t.Helper()
	var pack []byte
	m := Manifest{Format: model.PackFormat, FormatVersion: model.PackFormatVersion, PackID: "p1", TenantID: "t1", ConnectionID: "platform"}
	for i, c := range contents {
		m.Entries = append(m.Entries, ManifestEntry{Ordinal: i, AssetID: "asset-" + string(rune('a'+i)), ObjectID: "obj", Version: 1,
			Offset: int64(len(pack)), Length: int64(len(c)), Size: int64(len(c)), SHA256: sum([]byte(c)), Compression: CompressionNone})
		pack = append(pack, c...)
	}
	m.EntryCount, m.PackBytes, m.PackSHA256 = len(m.Entries), int64(len(pack)), sum(pack)
	return m, pack
}

func TestManifestEncodingIsCanonicalAndRoundTrips(t *testing.T) {
	m, _ := manifestOf(t, "first", "", "third entry")
	a, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.Encode()
	if !bytes.Equal(a, b) {
		t.Fatal("the same manifest encoded twice differs")
	}
	parsed, err := ParseManifest(a, 10)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := parsed.Encode()
	if !bytes.Equal(a, again) || parsed.Checksum == "" || parsed.Entries[1].Length != 0 {
		t.Fatalf("round trip: %s", again)
	}
}

func TestManifestsThatMustNotBeTrusted(t *testing.T) {
	good, _ := manifestOf(t, "aaaa", "bb", "c")
	cases := map[string]func(m *Manifest){
		"another format version": func(m *Manifest) { m.FormatVersion = 2 },
		"a gap between entries":  func(m *Manifest) { m.Entries[1].Offset++; m.PackBytes++ },
		"overlapping entries":    func(m *Manifest) { m.Entries[1].Offset-- },
		"an entry past the end":  func(m *Manifest) { m.Entries[2].Length, m.Entries[2].Size = 2, 2 },
		"an overflowing entry": func(m *Manifest) {
			m.Entries[2].Length, m.Entries[2].Size = math.MaxInt64, math.MaxInt64
		},
		"a negative length":       func(m *Manifest) { m.Entries[0].Length, m.Entries[0].Size = -1, -1 },
		"an asset listed twice":   func(m *Manifest) { m.Entries[2].AssetID = m.Entries[0].AssetID },
		"a path as an identifier": func(m *Manifest) { m.Entries[0].AssetID = "../other-tenant" },
		"another tenant's path":   func(m *Manifest) { m.TenantID = "t1/../t2" },
		"a wrong ordinal":         func(m *Manifest) { m.Entries[1].Ordinal = 5 },
		"a miscounted manifest":   func(m *Manifest) { m.EntryCount = 2 },
		"a compressed entry":      func(m *Manifest) { m.Entries[0].Compression = "zstd" },
		"a malformed checksum":    func(m *Manifest) { m.Entries[0].SHA256 = strings.ToUpper(m.Entries[0].SHA256) },
	}
	for name, change := range cases {
		m := good
		m.Entries = append([]ManifestEntry(nil), good.Entries...)
		change(&m)
		if err := m.Validate(10); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := good.Validate(2); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("more entries than allowed: %v", err)
	}

	encoded, _ := good.Encode()
	tampered := bytes.Replace(encoded, []byte(`"size":2`), []byte(`"size":3`), 1)
	if _, err := ParseManifest(tampered, 10); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("a changed byte: %v", err)
	}
	var loose map[string]any
	_ = json.Unmarshal(encoded, &loose)
	pretty, _ := json.MarshalIndent(loose, "", "  ")
	if _, err := ParseManifest(pretty, 10); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("the same content, not canonical: %v", err)
	}
	if _, err := ParseManifest(append(encoded[:len(encoded)-1], []byte(`,"extra":1}`)...), 10); !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("an unknown field: %v", err)
	}
}

func TestStreamConcatenatesInOrderWithBoundedFetches(t *testing.T) {
	contents := []string{"one", "", "three", strings.Repeat("x", 10_000), "five"}
	srcs := sources(contents...)
	var open, peak atomic.Int64
	for i := range srcs {
		inner := srcs[i].Open
		srcs[i].Open = func(ctx context.Context) (io.ReadCloser, error) {
			if n := open.Add(1); n > peak.Load() {
				peak.Store(n)
			}
			time.Sleep(5 * time.Millisecond)
			defer open.Add(-1)
			return inner(ctx)
		}
	}
	s := NewStream(context.Background(), srcs, 2)
	defer s.Close()
	got := make([]byte, TotalSize(srcs))
	if _, err := io.ReadFull(s, got); err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(contents, ""); string(got) != want {
		t.Fatal("the pack is not the entries in order")
	}
	layout, packSum, err := s.Result()
	if err != nil || packSum != sum(got) || len(layout) != len(contents) {
		t.Fatalf("result: %v %v", layout, err)
	}
	if layout[2] != (Layout{Offset: 3, Length: 5}) || layout[1].Length != 0 {
		t.Fatalf("layout %v", layout)
	}
	if peak.Load() > 2 {
		t.Fatalf("%d sources were open at once, at most 2 allowed", peak.Load())
	}
}

func TestStreamEndsWithTheFailingSource(t *testing.T) {
	srcs := sources("good", "changed", "never read")
	srcs[1].SHA256 = sum([]byte("what was recorded"))
	s := NewStream(context.Background(), srcs, 4)
	defer s.Close()
	_, err := io.ReadAll(s)
	var se *SourceError
	if !errors.As(err, &se) || se.Index != 1 || !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("expected source 1's mismatch: %v", err)
	}
	if _, _, err := s.Result(); err == nil {
		t.Fatal("a failed stream has no result")
	}

	short := sources("abc")
	short[0].Size = 4
	if _, err := io.ReadAll(NewStream(context.Background(), short, 1)); !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("a short source: %v", err)
	}
}

func TestVerifyPackChecksEveryEntryAndTheWhole(t *testing.T) {
	m, pack := manifestOf(t, "alpha", "beta", "gamma")
	if err := VerifyPack(bytes.NewReader(pack), m); err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(pack)
	flipped[6] ^= 1
	if err := VerifyPack(bytes.NewReader(flipped), m); !errors.Is(err, model.ErrPackCorrupt) {
		t.Fatalf("a flipped byte: %v", err)
	}
	if err := VerifyPack(bytes.NewReader(append(bytes.Clone(pack), 'x')), m); !errors.Is(err, model.ErrPackCorrupt) {
		t.Fatalf("trailing bytes: %v", err)
	}
	if err := VerifyPack(bytes.NewReader(pack[:len(pack)-1]), m); !errors.Is(err, model.ErrPackCorrupt) {
		t.Fatalf("a short pack: %v", err)
	}
}

func TestVerifiedEntryReadsExactlyTheRecordedBytes(t *testing.T) {
	data := []byte("the entry's bytes")
	read := func(body []byte, size int64, want string) error {
		_, err := io.ReadAll(VerifiedEntry(io.NopCloser(bytes.NewReader(body)), size, want))
		return err
	}
	if err := read(data, int64(len(data)), sum(data)); err != nil {
		t.Fatalf("a good entry: %v", err)
	}
	if err := read(data[:5], int64(len(data)), sum(data)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a short entry: %v", err)
	}
	if err := read(append(bytes.Clone(data), 'x'), int64(len(data)), sum(data)); !errors.Is(err, model.ErrPackCorrupt) {
		t.Fatalf("a long entry: %v", err)
	}
	changed := bytes.Clone(data)
	changed[0] ^= 1
	if err := read(changed, int64(len(data)), sum(data)); !errors.Is(err, model.ErrPackCorrupt) {
		t.Fatalf("a changed entry: %v", err)
	}
	if err := read(nil, 0, sum(nil)); err != nil {
		t.Fatalf("an empty entry: %v", err)
	}
}

func TestPackStatesMoveOnlyForward(t *testing.T) {
	allowed := [][2]model.PackState{
		{model.PackPlanned, model.PackWriting}, {model.PackWriting, model.PackVerifying}, {model.PackVerifying, model.PackPublished},
		{model.PackPublished, model.PackRetiring}, {model.PackRetiring, model.PackDeleted}, {model.PackWriting, model.PackFailed},
		{model.PackFailed, model.PackDeleted},
	}
	for _, p := range allowed {
		if !model.CanTransitionPack(p[0], p[1]) {
			t.Errorf("%s -> %s must be allowed", p[0], p[1])
		}
	}
	for _, p := range [][2]model.PackState{{model.PackPublished, model.PackWriting}, {model.PackPublished, model.PackFailed},
		{model.PackDeleted, model.PackPublished}, {model.PackRetiring, model.PackPublished}} {
		if model.CanTransitionPack(p[0], p[1]) {
			t.Errorf("%s -> %s must be refused", p[0], p[1])
		}
	}
}
