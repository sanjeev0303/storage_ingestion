// Package rangeread streams one stored object while fetching several
// ranges of it at once.
//
// A single GET from S3, GCS or Azure runs at a fraction of what the
// provider can serve; a handful of concurrent ranged GETs fill the link.
// The bytes still come out strictly in order, so the reader drops in
// wherever a sequential body was read: hashing, scanning, copying.
package rangeread

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// ChunkBytes and Parallel bound a reader's memory: at most Parallel+1
// chunks are held at once (72 MiB with the defaults).
const (
	ChunkBytes = 8 << 20
	Parallel   = 8
)

// chunkPool reuses ChunkBytes buffers across reads and readers. Without it
// every pass over an object allocates the object's whole size in chunks
// that live a few milliseconds: a 4 GiB run read three times is 12 GiB of
// garbage for the collector. Buffers of another chunk size are not pooled.
var chunkPool = sync.Pool{New: func() any {
	buf := make([]byte, ChunkBytes)
	return &buf
}}

// Ranger serves byte ranges of an object.
type Ranger interface {
	OpenRange(ctx context.Context, bucket, objectKey string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error)
}

// Opener serves an object as one stream.
type Opener interface {
	OpenObject(ctx context.Context, bucket, objectKey string) (io.ReadCloser, error)
}

// OpenObject reads the object described by meta (from HeadObject) in
// parallel ranges when storage serves ranges and the object is bigger than
// one chunk, and as one stream otherwise. Every range is pinned to meta's
// ETag and version, so an overwrite during the read fails it instead of
// mixing two objects' bytes.
func OpenObject(ctx context.Context, storage Opener, bucket, key string, meta interfaces.ObjectMetadata) (io.ReadCloser, error) {
	ranger, ok := storage.(Ranger)
	if !ok || meta.SizeBytes <= ChunkBytes || meta.ETag == "" {
		return storage.OpenObject(ctx, bucket, key)
	}
	cond := interfaces.ReadCondition{IfMatch: meta.ETag, VersionID: meta.VersionID}
	return Open(ctx, ranger, bucket, key, meta.SizeBytes, cond, ChunkBytes, Parallel), nil
}

type chunk struct {
	data []byte
	// pooled is the chunkPool buffer data lives in, given back once read.
	pooled *[]byte
	err    error
}

type reader struct {
	ctx    context.Context
	cancel context.CancelFunc
	// pending holds one result channel per chunk, in object order. Its
	// buffer is what bounds how far ahead the fetches run.
	pending chan chan chunk
	current []byte
	held    *[]byte
	err     error
}

// release gives the chunk being read back to the pool.
func (rd *reader) release() {
	if rd.held != nil {
		chunkPool.Put(rd.held)
		rd.held = nil
	}
	rd.current = nil
}

// Open returns bytes [0, size) of an object, read as chunkSize ranges with
// up to parallel of them in flight, all under cond.
func Open(ctx context.Context, r Ranger, bucket, key string, size int64, cond interfaces.ReadCondition, chunkSize int64, parallel int) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	rd := &reader{ctx: ctx, cancel: cancel, pending: make(chan chan chunk, parallel)}

	go func() {
		defer close(rd.pending)
		for first := int64(0); first < size; first += chunkSize {
			last := min(first+chunkSize, size) - 1
			result := make(chan chunk, 1)
			select {
			case rd.pending <- result:
			case <-ctx.Done():
				return
			}
			go func() { result <- fetch(ctx, r, bucket, key, first, last, cond) }()
		}
	}()
	return rd
}

func fetch(ctx context.Context, r Ranger, bucket, key string, first, last int64, cond interfaces.ReadCondition) chunk {
	body, err := r.OpenRange(ctx, bucket, key, first, last, cond)
	if err != nil {
		return chunk{err: err}
	}
	defer body.Close()
	size := last - first + 1
	var pooled *[]byte
	var data []byte
	if size <= ChunkBytes {
		pooled = chunkPool.Get().(*[]byte)
		data = (*pooled)[:size]
	} else {
		data = make([]byte, size)
	}
	if _, err := io.ReadFull(body, data); err != nil {
		if pooled != nil {
			chunkPool.Put(pooled)
		}
		return chunk{err: fmt.Errorf("read bytes %d-%d: %w", first, last, err)}
	}
	return chunk{data: data, pooled: pooled}
}

// next makes the following chunk current, giving the finished one back to
// the pool. It returns false when there is nothing more to read (rd.err).
func (rd *reader) next() bool {
	rd.release()
	if rd.err != nil {
		return false
	}
	result, ok := <-rd.pending
	if !ok {
		// Every chunk was handed out, unless the read was cancelled.
		rd.err = io.EOF
		if err := rd.ctx.Err(); err != nil {
			rd.err = err
		}
		return false
	}
	got := <-result
	if got.err != nil {
		rd.err = got.err
		rd.cancel()
		return false
	}
	rd.current, rd.held = got.data, got.pooled
	return true
}

func (rd *reader) Read(p []byte) (int, error) {
	for len(rd.current) == 0 {
		if !rd.next() {
			return 0, rd.err
		}
	}
	n := copy(p, rd.current)
	rd.current = rd.current[n:]
	return n, nil
}

// WriteTo hands whole chunks to w, so io.Copy into a hasher or a socket
// skips copying every byte through a small intermediate buffer. w must not
// keep the slice (the io.Writer contract): its buffer is reused.
func (rd *reader) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for {
		if len(rd.current) > 0 {
			n, err := w.Write(rd.current)
			total += int64(n)
			rd.current = rd.current[n:]
			if err != nil {
				return total, err
			}
		}
		if !rd.next() {
			if rd.err == io.EOF {
				return total, nil
			}
			return total, rd.err
		}
	}
}

// Close stops every fetch still running. Their results go to buffered
// channels nobody reads, so no goroutine is left blocked; those chunks are
// left to the collector rather than the pool.
func (rd *reader) Close() error {
	rd.cancel()
	rd.release()
	if rd.err == nil {
		rd.err = io.ErrClosedPipe
	}
	return nil
}
