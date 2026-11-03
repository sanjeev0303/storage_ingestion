package chunkread

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

type store map[string][]byte

func (s store) OpenRange(_ context.Context, _, key string, _, _ int64, _ interfaces.ReadCondition) (io.ReadCloser, error) {
	data, ok := s[key]
	if !ok {
		return nil, interfaces.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func build(pieces ...string) (store, []model.ChunkExtent, []byte) {
	s := store{}
	var extents []model.ChunkExtent
	var whole []byte
	for _, p := range pieces {
		sum := sha256.Sum256([]byte(p))
		h := hex.EncodeToString(sum[:])
		s["k"+h] = []byte(p)
		extents = append(extents, model.ChunkExtent{Offset: int64(len(whole)), Length: len(p), SHA256: h, Key: "k" + h})
		whole = append(whole, p...)
	}
	return s, extents, whole
}

func readAll(t *testing.T, s store, e []model.ChunkExtent, first, last int64) ([]byte, error) {
	t.Helper()
	r, err := Open(context.Background(), s, "b", e, first, last)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func TestEveryRangeGivesExactlyThoseBytes(t *testing.T) {
	s, e, whole := build("aaaa", "bb", "cccccc", "d", "ee", "ffff", "g", "hhh")
	for first := 0; first < len(whole); first++ {
		for last := first; last < len(whole); last++ {
			got, err := readAll(t, s, e, int64(first), int64(last))
			if err != nil || !bytes.Equal(got, whole[first:last+1]) {
				t.Fatalf("%d-%d: %q %v", first, last, got, err)
			}
		}
	}
	got, err := readAll(t, s, e, 0, -1)
	if err != nil || !bytes.Equal(got, whole) {
		t.Fatalf("whole: %q %v", got, err)
	}
}

type countingStore struct {
	store
	opens int
}

func (s *countingStore) OpenRange(ctx context.Context, b, key string, first, last int64, c interfaces.ReadCondition) (io.ReadCloser, error) {
	s.opens++
	return s.store.OpenRange(ctx, b, key, first, last, c)
}

func TestSmallReadsOfOneChunkFetchItOnce(t *testing.T) {
	s, e, whole := build("aaaaaaaa", "bbbbbbbb")
	counted := &countingStore{store: s}
	cache := NewCache(1<<20, time.Minute)
	for first := int64(0); first < 4; first++ {
		r, err := OpenCached(context.Background(), cache, counted, "b", e, first, first+1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(got, whole[first:first+2]) {
			t.Fatalf("%d: %q %v", first, got, err)
		}
	}
	if counted.opens != 1 {
		t.Fatalf("%d fetches", counted.opens)
	}
}

func TestTheCacheStaysWithinItsBoundsAndForgets(t *testing.T) {
	c := NewCache(10, time.Hour)
	c.put("a", make([]byte, 6))
	c.put("b", make([]byte, 6))
	if c.get("a") != nil || c.get("b") == nil || c.used != 6 {
		t.Fatalf("used %d", c.used)
	}
	c.put("huge", make([]byte, 11))
	if c.get("huge") != nil {
		t.Fatal("kept something larger than the cache")
	}
	old := NewCache(10, time.Nanosecond)
	old.put("a", []byte{1})
	time.Sleep(time.Millisecond)
	if old.get("a") != nil || old.used != 0 {
		t.Fatal("an old chunk stayed")
	}
}

func TestADamagedChunkIsNotCached(t *testing.T) {
	s, e, _ := build("aaaa")
	s[e[0].Key] = []byte("axaa")
	cache := NewCache(1<<20, time.Minute)
	for i := 0; i < 2; i++ {
		r, err := OpenCached(context.Background(), cache, s, "b", e, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(r)
		r.Close()
		if !errors.Is(err, model.ErrChunkCorrupt) {
			t.Fatalf("damaged: %v", err)
		}
	}
}

func TestOutsideTheFileIsRefused(t *testing.T) {
	s, e, whole := build("aaaa", "bb")
	if _, err := Open(context.Background(), s, "b", e, 0, int64(len(whole))); err == nil {
		t.Fatal("a read past the end was opened")
	}
}

func TestADamagedChunkIsAnErrorEvenForAFewBytesOfIt(t *testing.T) {
	s, e, _ := build("aaaa", "bbbb", "cccc")
	s[e[1].Key] = []byte("bxbb")
	if _, err := readAll(t, s, e, 5, 5); !errors.Is(err, model.ErrChunkCorrupt) {
		t.Fatalf("damaged: %v", err)
	}
	s[e[1].Key] = []byte("bbb")
	if _, err := readAll(t, s, e, 0, -1); !errors.Is(err, model.ErrChunkCorrupt) {
		t.Fatalf("short: %v", err)
	}
	s[e[1].Key] = []byte("bbbbb")
	if _, err := readAll(t, s, e, 0, -1); !errors.Is(err, model.ErrChunkCorrupt) {
		t.Fatalf("long: %v", err)
	}
	delete(s, e[1].Key)
	if _, err := readAll(t, s, e, 0, -1); !errors.Is(err, interfaces.ErrObjectNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestClosingEarlyStopsReading(t *testing.T) {
	s, e, _ := build("aaaa", "bbbb", "cccc", "dddd", "eeee", "ffff", "gggg", "hhhh")
	r, err := Open(context.Background(), s, "b", e, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	r.Close()
	done := make(chan struct{})
	go func() {
		// The producer goroutine exits and closes its channel.
		for range r.(*reader).results {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reading went on after Close")
	}
}
