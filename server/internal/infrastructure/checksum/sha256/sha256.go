// Package sha256 hashes streams with bounded memory.
package sha256

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// bufferSize bounds memory: one buffer per stream, whatever the object size.
const bufferSize = 64 * 1024

// Sum reads r to the end and returns its lowercase hex SHA-256.
// It stops with ctx.Err() as soon as ctx is cancelled.
func Sum(ctx context.Context, r io.Reader) (string, error) {
	h := sha256.New()

	// A source that hands out its own buffers (the parallel range reader)
	// is hashed straight from them: no copy through buf.
	if wt, ok := r.(io.WriterTo); ok {
		if _, err := wt.WriteTo(cancellable{ctx: ctx, w: h}); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	buf := make([]byte, bufferSize)

	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		n, err := r.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

// cancellable stops a WriteTo between writes once ctx is cancelled.
type cancellable struct {
	ctx context.Context
	w   io.Writer
}

func (c cancellable) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}
