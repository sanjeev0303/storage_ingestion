// Package jsonl reads JSON Lines streams one line at a time. Memory is
// bounded by the longest allowed line, whatever the stream's size, and
// the reader knows each line's number and where the next one starts, so
// reading can resume from a checkpoint.
package jsonl

import (
	"bufio"
	"errors"
	"io"
)

// Reader reads lines of at most a maximum length.
type Reader struct {
	r   *bufio.Reader
	max int
	buf []byte
	// line is the number of the last line read; offset the bytes consumed
	// so far, counted from the start of the stream.
	line   int64
	offset int64
}

// NewReader reads r, whose first byte is at offset startOffset of the
// stream and whose first line is line startLine+1.
func NewReader(r io.Reader, maxLineBytes int, startLine, startOffset int64) *Reader {
	return &Reader{
		r:      bufio.NewReaderSize(r, 64*1024),
		max:    maxLineBytes,
		buf:    make([]byte, 0, maxLineBytes+1),
		line:   startLine,
		offset: startOffset,
	}
}

// Line is the number of the last line read.
func (r *Reader) Line() int64 { return r.line }

// Offset is the byte offset just after the last line read.
func (r *Reader) Offset() int64 { return r.offset }

// Next returns the next line, with its newline if it has one, or io.EOF
// after the last. A line longer than the maximum is read to its end but
// not kept: tooLong is set instead. The returned slice is valid until the
// next call.
func (r *Reader) Next() (line []byte, tooLong bool, err error) {
	r.buf = r.buf[:0]
	for {
		chunk, err := r.r.ReadSlice('\n')
		r.offset += int64(len(chunk))
		if !tooLong {
			if len(r.buf)+len(chunk) > r.max+1 {
				tooLong = true
				r.buf = r.buf[:0]
			} else {
				r.buf = append(r.buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		if err != nil && len(r.buf) == 0 && !tooLong {
			return nil, false, io.EOF
		}
		r.line++
		return r.buf, tooLong, nil
	}
}
