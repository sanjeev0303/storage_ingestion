package osp

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

// The server's chunker (internal/platform/fastcdc), so that chunks of a new
// build match chunks it already holds. Both are held to one test vector.
const (
	chunkMin = 256 << 10
	chunkAvg = 1 << 20
	chunkMax = 8 << 20

	chunkMaskStrict = uint64(0xFFFFFC0000000000)
	chunkMaskLoose  = uint64(0xFFFC000000000000)
)

var chunkGear = func() (table [256]uint64) {
	for i := range table {
		sum := sha256.Sum256([]byte{'g', 'e', 'a', 'r', byte(i)})
		table[i] = binary.LittleEndian.Uint64(sum[:8])
	}
	return table
}()

func chunkCut(data []byte) int {
	n := len(data)
	if n <= chunkMin {
		return n
	}
	n = min(n, chunkMax)
	normal := min(chunkAvg, n)
	var hash uint64
	i := chunkMin
	for ; i < normal; i++ {
		hash = hash<<1 + chunkGear[data[i]]
		if hash&chunkMaskStrict == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		hash = hash<<1 + chunkGear[data[i]]
		if hash&chunkMaskLoose == 0 {
			return i + 1
		}
	}
	return n
}

// eachChunk calls fn with each chunk of r, valid only during the call.
func eachChunk(r io.Reader, fn func(offset int64, data []byte) error) error {
	buf := make([]byte, 2*chunkMax)
	start, end := 0, 0
	done := false
	var offset int64
	for {
		if !done && end-start < chunkMax {
			copy(buf, buf[start:end])
			end -= start
			start = 0
			for !done && end < len(buf) {
				n, err := r.Read(buf[end:])
				end += n
				if errors.Is(err, io.EOF) {
					done = true
				} else if err != nil {
					return err
				}
			}
		}
		if start == end {
			return nil
		}
		n := chunkCut(buf[start:end])
		if err := fn(offset, buf[start:start+n]); err != nil {
			return err
		}
		start += n
		offset += int64(n)
	}
}
