// Package fastcdc cuts a stream into content-defined chunks (Xia et al., USENIX
// ATC 2016): a cut depends only on the bytes just before it, so inserting data
// moves the chunks after it without changing them.
package fastcdc

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

// The sizes this platform cuts at. Changing them or the gear table changes
// every chunk, so a new setting needs a new Params name.
const (
	MinSize = 256 << 10
	AvgSize = 1 << 20
	MaxSize = 8 << 20
	Params  = "fastcdc-v1-256k-1m-8m"
)

// Normalized chunking: harder to cut before the average, easier after, which
// narrows the spread of sizes. A gear hash carries its window in its high bits,
// so the masks look there.
const (
	maskStrict = uint64(0xFFFFFC0000000000) // 22 bits
	maskLoose  = uint64(0xFFFC000000000000) // 18 bits
)

var gear = func() (table [256]uint64) {
	for i := range table {
		sum := sha256.Sum256([]byte{'g', 'e', 'a', 'r', byte(i)})
		table[i] = binary.LittleEndian.Uint64(sum[:8])
	}
	return table
}()

// cut answers where the first chunk of data ends.
func cut(data []byte) int {
	n := len(data)
	if n <= MinSize {
		return n
	}
	if n > MaxSize {
		n = MaxSize
	}
	normal := min(AvgSize, n)
	var hash uint64
	i := MinSize
	for ; i < normal; i++ {
		hash = hash<<1 + gear[data[i]]
		if hash&maskStrict == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		hash = hash<<1 + gear[data[i]]
		if hash&maskLoose == 0 {
			return i + 1
		}
	}
	return n
}

// Chunker reads a stream a chunk at a time, holding at most two maximum chunks.
type Chunker struct {
	r          io.Reader
	buf        []byte
	start, end int
	done       bool
}

func New(r io.Reader) *Chunker {
	return &Chunker{r: r, buf: make([]byte, 2*MaxSize)}
}

// Next answers the next chunk, valid until the next call, or io.EOF.
func (c *Chunker) Next() ([]byte, error) {
	if !c.done && c.end-c.start < MaxSize {
		copy(c.buf, c.buf[c.start:c.end])
		c.end -= c.start
		c.start = 0
		for !c.done && c.end < len(c.buf) {
			n, err := c.r.Read(c.buf[c.end:])
			c.end += n
			if errors.Is(err, io.EOF) {
				c.done = true
			} else if err != nil {
				return nil, err
			}
		}
	}
	if c.start == c.end {
		return nil, io.EOF
	}
	n := cut(c.buf[c.start:c.end])
	out := c.buf[c.start : c.start+n]
	c.start += n
	return out, nil
}
