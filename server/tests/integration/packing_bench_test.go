package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/compaction"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// TestPackingBenchmark measures, on the compose MinIO, what packing costs and
// what reads cost before and after: writing and verifying one pack with the
// primitives the pack worker uses, and reading objects standalone versus as
// ranges of the pack. Opt-in: PACKING_BENCH=1 with TEST_S3_BUCKET. It writes
// about PACKING_BENCH_OBJECTS × PACKING_BENCH_BYTES twice and deletes it all.
func TestPackingBenchmark(t *testing.T) {
	if os.Getenv("PACKING_BENCH") != "1" {
		t.Skip("PACKING_BENCH=1 runs the packing benchmark against the compose MinIO")
	}
	storage, bucket := openTestProvider(t)
	n := benchSetting(t, "PACKING_BENCH_OBJECTS", 500)
	size := benchSetting(t, "PACKING_BENCH_BYTES", 4096)
	ctx := context.Background()
	prefix := fmt.Sprintf("packing-bench/%d", time.Now().UnixNano())
	packKey := prefix + "/packs/bench.pack"

	keys := make([]string, n)
	etags := make([]string, n)
	sources := make([]compaction.Source, n)
	t.Cleanup(func() {
		for _, k := range append(keys, packKey) {
			if k != "" {
				_ = storage.DeleteObject(context.Background(), bucket, k)
			}
		}
	})
	start := time.Now()
	for i := range keys {
		data := bytes.Repeat([]byte{byte('a' + i%26)}, size)
		copy(data, strconv.Itoa(i))
		keys[i] = fmt.Sprintf("%s/src/%06d", prefix, i)
		meta, err := storage.WriteObject(ctx, bucket, keys[i], bytes.NewReader(data), int64(size), "application/octet-stream")
		if err != nil {
			t.Fatal(err)
		}
		etags[i] = meta.ETag
		sum := sha256.Sum256(data)
		key, cond := keys[i], interfaces.ReadCondition{IfMatch: meta.ETag}
		sources[i] = compaction.Source{AssetID: fmt.Sprintf("a%06d", i), Size: int64(size), SHA256: hex.EncodeToString(sum[:]),
			Open: func(ctx context.Context) (io.ReadCloser, error) {
				return storage.OpenRange(ctx, bucket, key, 0, int64(size)-1, cond)
			}}
	}
	t.Logf("setup: %d objects of %d bytes written in %s", n, size, time.Since(start).Round(time.Millisecond))

	// Pack: stream every source into one object, then read it back and
	// check every entry, as the worker does.
	memory := sampleHeap()
	start = time.Now()
	stream := compaction.NewStream(ctx, sources, 16)
	total := int64(n * size)
	packMeta, err := storage.WriteObject(ctx, bucket, packKey, stream, total, "application/octet-stream")
	if err != nil {
		stream.Close()
		t.Fatal(err)
	}
	layout, packSum, err := stream.Result()
	stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	written := time.Since(start)
	m := compaction.Manifest{Format: "ospack", FormatVersion: 1, PackID: "bench", TenantID: "bench", ConnectionID: "platform",
		PackBytes: total, PackSHA256: packSum, EntryCount: n}
	for i, l := range layout {
		m.Entries = append(m.Entries, compaction.ManifestEntry{Ordinal: i, AssetID: sources[i].AssetID, Version: 1,
			Offset: l.Offset, Length: l.Length, Size: l.Length, SHA256: sources[i].SHA256, Compression: compaction.CompressionNone})
	}
	start = time.Now()
	body, err := storage.OpenRange(ctx, bucket, packKey, 0, total-1, interfaces.ReadCondition{IfMatch: packMeta.ETag})
	if err != nil {
		t.Fatal(err)
	}
	err = compaction.VerifyPack(body, m)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	verified := time.Since(start)
	peak, allocated := memory()
	mib := float64(total) / (1 << 20)
	t.Logf("pack: wrote %d entries (%.1f MiB) in %s (%.0f objects/s, %.1f MiB/s); verified in %s",
		n, mib, written.Round(time.Millisecond), float64(n)/written.Seconds(), mib/written.Seconds(), verified.Round(time.Millisecond))
	t.Logf("pack memory: peak heap in use %.1f MiB above the start, %.1f MiB allocated in all (sampled every 5ms)",
		float64(peak)/(1<<20), float64(allocated)/(1<<20))

	standalone := func(i int) (io.ReadCloser, error) {
		return storage.OpenRange(ctx, bucket, keys[i], 0, int64(size)-1, interfaces.ReadCondition{IfMatch: etags[i]})
	}
	packed := func(i int) (io.ReadCloser, error) {
		first := layout[i].Offset
		return storage.OpenRange(ctx, bucket, packKey, first, first+layout[i].Length-1, interfaces.ReadCondition{IfMatch: packMeta.ETag})
	}
	for _, c := range []struct {
		name string
		open func(int) (io.ReadCloser, error)
	}{{"standalone", standalone}, {"packed", packed}} {
		for _, workers := range []int{1, 16} {
			p50, p95, elapsed := benchReads(t, n, workers, int64(size), c.open)
			t.Logf("read %-10s workers=%-2d %d reads in %s (%.0f reads/s), p50 %s, p95 %s", c.name, workers, n,
				elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds(), p50.Round(10*time.Microsecond), p95.Round(10*time.Microsecond))
		}
	}
}

// sampleHeap watches the heap until the returned function is called, which
// stops it and returns the peak heap in use above the starting point and the
// bytes allocated meanwhile. ReadMemStats briefly stops the world, so it
// slows what it measures a little.
func sampleHeap() func() (peak, allocated uint64) {
	runtime.GC()
	var start runtime.MemStats
	runtime.ReadMemStats(&start)
	done := make(chan struct{})
	result := make(chan uint64)
	go func() {
		var top uint64
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapInuse > start.HeapInuse && m.HeapInuse-start.HeapInuse > top {
				top = m.HeapInuse - start.HeapInuse
			}
			select {
			case <-done:
				result <- top
				return
			case <-tick.C:
			}
		}
	}()
	return func() (uint64, uint64) {
		close(done)
		peak := <-result
		var end runtime.MemStats
		runtime.ReadMemStats(&end)
		return peak, end.TotalAlloc - start.TotalAlloc
	}
}

func benchSetting(t *testing.T, name string, fallback int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 2 {
		t.Fatalf("%s must be a whole number of at least 2", name)
	}
	return n
}

// benchReads reads every object once with workers at a time and returns
// the median and 95th percentile of one read, and the wall time.
func benchReads(t *testing.T, n, workers int, size int64, open func(int) (io.ReadCloser, error)) (time.Duration, time.Duration, time.Duration) {
	t.Helper()
	took := make([]time.Duration, n)
	next := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	start := time.Now()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				began := time.Now()
				body, err := open(i)
				if err == nil {
					var read int64
					read, err = io.Copy(io.Discard, body)
					body.Close()
					if err == nil && read != size {
						err = fmt.Errorf("read %d of %d bytes", read, size)
					}
				}
				took[i] = time.Since(began)
				if err != nil {
					mu.Lock()
					failed = err
					mu.Unlock()
				}
			}
		}()
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
	elapsed := time.Since(start)
	if failed != nil {
		t.Fatal(failed)
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	return took[n/2], took[n*95/100], elapsed
}
