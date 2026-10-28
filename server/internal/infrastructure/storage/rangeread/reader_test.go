package rangeread

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

func stored(t *testing.T, size int) (*memory.ObjectStorage, []byte) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	s := memory.NewObjectStorage()
	s.PutObject("k", data)
	return s, data
}

func TestOpenReturnsTheObjectInOrder(t *testing.T) {
	for _, size := range []int{1, 1000, 4096, 4097, 50_000} {
		s, data := stored(t, size)
		cond := interfaces.ReadCondition{IfMatch: s.ETag("k")}
		got, err := io.ReadAll(Open(context.Background(), s, "b", "k", int64(size), cond, 4096, 3))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: %d bytes, err %v; want the object back unchanged", size, len(got), err)
		}
	}
}

func TestOpenOfAnEmptyObjectIsEmpty(t *testing.T) {
	got, err := io.ReadAll(Open(context.Background(), memory.NewObjectStorage(), "b", "k", 0, interfaces.ReadCondition{}, 4096, 3))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d bytes, %v", len(got), err)
	}
}

func TestAFailedRangeEndsTheStream(t *testing.T) {
	s, _ := stored(t, 20_000)
	s.RangeErr = interfaces.ErrStorageThrottled
	_, err := io.ReadAll(Open(context.Background(), s, "b", "k", 20_000, interfaces.ReadCondition{}, 4096, 2))
	if !errors.Is(err, interfaces.ErrStorageThrottled) {
		t.Fatalf("err = %v, want the range's error", err)
	}
}

func TestAnOverwriteDuringTheReadFailsIt(t *testing.T) {
	s, _ := stored(t, 20_000)
	cond := interfaces.ReadCondition{IfMatch: s.ETag("k")}
	rd := Open(context.Background(), s, "b", "k", 20_000, cond, 4096, 1)
	defer rd.Close()
	if _, err := io.ReadFull(rd, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	s.PutObject("k", []byte("other bytes"))
	if _, err := io.ReadAll(rd); !errors.Is(err, interfaces.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want a failed precondition, never the new object's bytes", err)
	}
}

func TestClosingEarlyStopsTheFetches(t *testing.T) {
	s, _ := stored(t, 400_000)
	var open atomic.Int32
	s.WrapRange = func(ctx context.Context, body io.ReadCloser) io.ReadCloser {
		open.Add(1)
		return body
	}
	before := runtime.NumGoroutine()
	rd := Open(context.Background(), s, "b", "k", 400_000, interfaces.ReadCondition{}, 4096, 4)
	if _, err := io.ReadFull(rd, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	rd.Close()
	if _, err := rd.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after close succeeded")
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.NumGoroutine() > before {
		t.Fatalf("%d goroutines left running after Close", runtime.NumGoroutine()-before)
	}
	// Read ahead is bounded: it never ran far past what was read.
	if n := open.Load(); n > 4+2 {
		t.Fatalf("%d ranges fetched for 10 bytes read; read-ahead is not bounded", n)
	}
}

// Chunks of the default size come from a pool. Reading the same object
// again (through WriteTo, as io.Copy does) reuses those buffers and must
// still return exactly the object.
func TestPooledChunksAreReusedWithoutMixingBytes(t *testing.T) {
	size := ChunkBytes*2 + ChunkBytes/2
	s, data := stored(t, size)
	cond := interfaces.ReadCondition{IfMatch: s.ETag("k")}
	for pass := 1; pass <= 3; pass++ {
		var got bytes.Buffer
		n, err := io.Copy(&got, Open(context.Background(), s, "b", "k", int64(size), cond, ChunkBytes, 2))
		if err != nil || n != int64(size) || !bytes.Equal(got.Bytes(), data) {
			t.Fatalf("pass %d: %d bytes, err %v; want the object back unchanged", pass, n, err)
		}
	}
}

// BenchmarkReadAll reports what one pass over a 64 MiB object allocates.
func BenchmarkReadAll(b *testing.B) {
	size := 64 << 20
	data := make([]byte, size)
	s := memory.NewObjectStorage()
	s.PutObject("k", data)
	cond := interfaces.ReadCondition{IfMatch: s.ETag("k")}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	for b.Loop() {
		body := Open(context.Background(), s, "b", "k", int64(size), cond, ChunkBytes, Parallel)
		if _, err := io.Copy(io.Discard, body); err != nil {
			b.Fatal(err)
		}
		body.Close()
	}
}
