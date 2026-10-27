package rangeread

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrRangeNotHonoured is a provider answering a ranged read with other
// bytes than the ones asked for: the whole object, another range, or a
// different length. Treated as a read failure, never as data.
var ErrRangeNotHonoured = errors.New("the storage provider did not return the requested byte range")

// CheckContentRange checks a "bytes first-last/total" Content-Range header
// against the range that was asked for.
func CheckContentRange(header string, first, last int64) error {
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes ")
	if !ok {
		return fmt.Errorf("%w: Content-Range %q", ErrRangeNotHonoured, header)
	}
	span, _, _ := strings.Cut(spec, "/")
	from, to, ok := strings.Cut(span, "-")
	if !ok {
		return fmt.Errorf("%w: Content-Range %q", ErrRangeNotHonoured, header)
	}
	gotFirst, err1 := strconv.ParseInt(from, 10, 64)
	gotLast, err2 := strconv.ParseInt(to, 10, 64)
	if err1 != nil || err2 != nil || gotFirst != first || gotLast != last {
		return fmt.Errorf("%w: asked for %d-%d, got %q", ErrRangeNotHonoured, first, last, header)
	}
	return nil
}

// Exact passes on exactly n bytes of body and fails the read if body has
// fewer or more. A packed object's range is a slice of a larger object, so
// a body that runs on past n would be a neighbour's bytes.
func Exact(body io.ReadCloser, n int64) io.ReadCloser {
	return &exactReader{body: body, left: n}
}

type exactReader struct {
	body io.ReadCloser
	left int64
	err  error
}

func (r *exactReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.left == 0 {
		// Nothing more is due: anything still in the body is too much.
		var extra [1]byte
		if n, _ := io.ReadFull(r.body, extra[:]); n > 0 {
			r.err = fmt.Errorf("%w: more bytes than the range", ErrRangeNotHonoured)
			return 0, r.err
		}
		r.err = io.EOF
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.body.Read(p)
	r.left -= int64(n)
	if err == io.EOF && r.left > 0 {
		r.err = fmt.Errorf("%w: %d bytes short", ErrRangeNotHonoured, r.left)
		return n, r.err
	}
	if err == io.EOF {
		err = nil // the final check happens on the next Read
	}
	return n, err
}

func (r *exactReader) Close() error { return r.body.Close() }
