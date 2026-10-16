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
