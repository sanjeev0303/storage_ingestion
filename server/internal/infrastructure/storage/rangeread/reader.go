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

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// ChunkBytes and Parallel bound a reader's memory: at most Parallel+1
// chunks are held at once (72 MiB with the defaults).
const (
	ChunkBytes = 8 << 20
	Parallel   = 8
)

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
	err  error
}

type reader struct {
	ctx    context.Context
	cancel context.CancelFunc
	// pending holds one result channel per chunk, in object order. Its
	// buffer is what bounds how far ahead the fetches run.
	pending chan chan chunk
	current []byte
	err     error
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
	data := make([]byte, last-first+1)
	if _, err := io.ReadFull(body, data); err != nil {
		return chunk{err: fmt.Errorf("read bytes %d-%d: %w", first, last, err)}
	}
	return chunk{data: data}
}

func (rd *reader) Read(p []byte) (int, error) {
	for len(rd.current) == 0 {
		if rd.err != nil {
			return 0, rd.err
		}
		next, ok := <-rd.pending
		if !ok {
			// Every chunk was handed out, unless the read was cancelled.
			rd.err = io.EOF
			if err := rd.ctx.Err(); err != nil {
				rd.err = err
			}
			continue
		}
		got := <-next
		if got.err != nil {
			rd.err = got.err
			rd.cancel()
			continue
		}
		rd.current = got.data
	}
	n := copy(p, rd.current)
	rd.current = rd.current[n:]
	return n, nil
}

// Close stops every fetch still running. Their results go to buffered
// channels nobody reads, so no goroutine is left blocked.
func (rd *reader) Close() error {
	rd.cancel()
	rd.current = nil
	if rd.err == nil {
		rd.err = io.ErrClosedPipe
	}
	return nil
}
