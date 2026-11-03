// Package chunkread reads a file kept as chunks. Each chunk is read whole and
// checked against its SHA-256, even for a read of a few bytes of it, so a damaged
// chunk is an error and never wrong bytes.
package chunkread

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// readAhead is how many chunks are fetched at once. One read at a time would
// cap a file at what one chunk's round trip allows.
const readAhead = 4

// RangeOpener is what storage has to offer.
type RangeOpener interface {
	OpenRange(ctx context.Context, bucket, objectKey string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error)
}

// Reader reads a chunked file through the same call as an object, for code
// that reads objects. Its bucket and key arguments are ignored: the extents
// say where the bytes are.
type Reader struct {
	Storage RangeOpener
	Bucket  string
	Extents []model.ChunkExtent
}

func (r Reader) OpenRange(ctx context.Context, _, _ string, first, last int64, _ interfaces.ReadCondition) (io.ReadCloser, error) {
	return Open(ctx, r.Storage, r.Bucket, r.Extents, first, last)
}

type fetched struct {
	data []byte
	err  error
}

type reader struct {
	cancel  context.CancelFunc
	results <-chan chan fetched
	current []byte
	err     error
}

// Cache keeps chunks recently read and checked, for small reads of large files: a
// loader reading 64 KiB at a time would otherwise fetch and hash the same megabyte
// sixteen times. Chunks are named by their hash, so one can never be stale. They
// leave after a few minutes, so that erased bytes do not linger in memory.
type Cache struct {
	mu       sync.Mutex
	maxBytes int
	maxAge   time.Duration
	used     int
	order    *list.List // oldest use at the front
	items    map[string]*list.Element
}

type cached struct {
	key  string
	data []byte
	at   time.Time
}

func NewCache(maxBytes int, maxAge time.Duration) *Cache {
	return &Cache{maxBytes: maxBytes, maxAge: maxAge, order: list.New(), items: map[string]*list.Element{}}
}

func (c *Cache) get(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil
	}
	item := el.Value.(*cached)
	if time.Since(item.at) > c.maxAge {
		c.remove(el)
		return nil
	}
	c.order.MoveToBack(el)
	return item.data
}

func (c *Cache) put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok || len(data) > c.maxBytes {
		return
	}
	c.items[key] = c.order.PushBack(&cached{key: key, data: data, at: time.Now()})
	c.used += len(data)
	for c.used > c.maxBytes {
		c.remove(c.order.Front())
	}
}

func (c *Cache) remove(el *list.Element) {
	item := el.Value.(*cached)
	c.order.Remove(el)
	delete(c.items, item.key)
	c.used -= len(item.data)
}

// Open reads bytes first to last of the file, in order; last -1 means its end.
func Open(ctx context.Context, storage RangeOpener, bucket string, extents []model.ChunkExtent, first, last int64) (io.ReadCloser, error) {
	return OpenCached(ctx, nil, storage, bucket, extents, first, last)
}

// OpenCached is Open with a cache of checked chunks; cache may be nil.
func OpenCached(ctx context.Context, cache *Cache, storage RangeOpener, bucket string, extents []model.ChunkExtent, first, last int64) (io.ReadCloser, error) {
	var size int64
	if n := len(extents); n > 0 {
		size = extents[n-1].Offset + int64(extents[n-1].Length)
	}
	if last < 0 {
		last = size - 1
	}
	if first < 0 || first > last || last >= size {
		return nil, fmt.Errorf("bytes %d-%d are outside a %d-byte file", first, last, size)
	}
	// The first chunk holding byte first.
	start := sort.Search(len(extents), func(i int) bool {
		return extents[i].Offset+int64(extents[i].Length) > first
	})
	ctx, cancel := context.WithCancel(ctx)
	results := make(chan chan fetched, readAhead)
	go func() {
		defer close(results)
		for i := start; i < len(extents) && extents[i].Offset <= last; i++ {
			e := extents[i]
			one := make(chan fetched, 1)
			select {
			case results <- one:
			case <-ctx.Done():
				return
			}
			go func() {
				data, err := fetchCached(ctx, cache, storage, bucket, e)
				if err == nil {
					from := max(first-e.Offset, 0)
					to := min(last-e.Offset+1, int64(e.Length))
					data = data[from:to]
				}
				one <- fetched{data, err}
			}()
		}
	}()
	return &reader{cancel: cancel, results: results}, nil
}

func fetchCached(ctx context.Context, cache *Cache, storage RangeOpener, bucket string, e model.ChunkExtent) ([]byte, error) {
	if cache == nil {
		return fetch(ctx, storage, bucket, e)
	}
	key := bucket + "/" + e.Key
	if data := cache.get(key); data != nil {
		return data, nil
	}
	data, err := fetch(ctx, storage, bucket, e)
	if err == nil {
		cache.put(key, data)
	}
	return data, err
}

func fetch(ctx context.Context, storage RangeOpener, bucket string, e model.ChunkExtent) ([]byte, error) {
	body, err := storage.OpenRange(ctx, bucket, e.Key, 0, -1, interfaces.ReadCondition{})
	if err != nil {
		return nil, fmt.Errorf("read chunk %s: %w", e.SHA256, err)
	}
	defer body.Close()
	var buf bytes.Buffer
	buf.Grow(e.Length)
	if _, err := io.Copy(&buf, io.LimitReader(body, int64(e.Length)+1)); err != nil {
		return nil, fmt.Errorf("read chunk %s: %w", e.SHA256, err)
	}
	sum := sha256.Sum256(buf.Bytes())
	if buf.Len() != e.Length || hex.EncodeToString(sum[:]) != e.SHA256 {
		return nil, fmt.Errorf("%w: %s", model.ErrChunkCorrupt, e.SHA256)
	}
	return buf.Bytes(), nil
}

func (r *reader) Read(b []byte) (int, error) {
	for len(r.current) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		one, ok := <-r.results
		if !ok {
			r.err = io.EOF
			return 0, io.EOF
		}
		got := <-one
		if got.err != nil {
			r.err = got.err
			return 0, got.err
		}
		r.current = got.data
	}
	n := copy(b, r.current)
	r.current = r.current[n:]
	return n, nil
}

func (r *reader) Close() error {
	r.cancel()
	return nil
}
