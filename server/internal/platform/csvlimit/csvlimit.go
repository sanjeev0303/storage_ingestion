// Package csvlimit bounds how much of a CSV stream one record may take.
//
// encoding/csv buffers a whole record before returning it, so a stream with
// no line break, or with a quote that never closes, makes it hold the whole
// stream in memory. Reading through Reader stops such a stream with
// ErrRecordTooLong instead.
package csvlimit

import (
	"errors"
	"fmt"
	"io"
)

// ErrRecordTooLong is returned once one record passes the limit.
var ErrRecordTooLong = errors.New("csv record too long")

type reader struct {
	r       io.Reader
	max     int
	run     int  // bytes since the last record break
	inQuote bool // inside a quoted field, where line breaks do not end the record
}

// Reader returns r, failing once a record grows past max bytes. Line breaks
// inside quotes belong to the record, as encoding/csv reads them.
func Reader(r io.Reader, max int) io.Reader {
	return &reader{r: r, max: max}
}

func (l *reader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	for i := 0; i < n; i++ {
		switch p[i] {
		case '"':
			l.inQuote = !l.inQuote // "" inside quotes flips twice
		case '\n':
			if !l.inQuote {
				l.run = 0
				continue
			}
		}
		l.run++
		if l.run > l.max {
			return i, fmt.Errorf("%w: more than %d bytes", ErrRecordTooLong, l.max)
		}
	}
	return n, err
}
