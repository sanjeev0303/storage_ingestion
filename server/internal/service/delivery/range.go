package delivery

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseRange reads a single "bytes=" range against an object of size bytes
// and returns the inclusive bounds. Partial is false when the whole object
// should be sent: no header, a malformed one, or several ranges (RFC 9110
// lets a server ignore those; multipart/byteranges is not supported). A
// well-formed range that cannot be satisfied (it starts past the end, is a
// zero-length suffix, or the object is empty) is ErrRangeNotSatisfiable.
func parseRange(header string, size int64) (first, last int64, partial bool, err error) {
	whole := func() (int64, int64, bool, error) { return 0, size - 1, false, nil }

	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if header == "" || !ok || strings.Contains(spec, ",") {
		return whole()
	}

	start, end, ok := strings.Cut(strings.TrimSpace(spec), "-")
	start, end = strings.TrimSpace(start), strings.TrimSpace(end)
	if !ok || (start == "" && end == "") {
		return whole()
	}

	if start == "" {
		// Suffix range: the last n bytes.
		n, ok := parseDigits(end)
		if !ok {
			return whole()
		}
		if n == 0 || size == 0 {
			return 0, 0, false, ErrRangeNotSatisfiable
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, nil
	}

	first, ok = parseDigits(start)
	if !ok {
		return whole()
	}
	last = size - 1
	if end != "" {
		last, ok = parseDigits(end)
		if !ok || last < first {
			return whole()
		}
		if last > size-1 {
			last = size - 1
		}
	}
	if first >= size {
		return 0, 0, false, ErrRangeNotSatisfiable
	}
	return first, last, true, nil
}

// parseDigits accepts only ASCII digits: no sign, no spaces, no overflow.
func parseDigits(text string) (int64, bool) {
	if text == "" || len(text) > 18 {
		return 0, false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	return n, err == nil
}

func parseHTTPTime(text string) (time.Time, error) {
	return http.ParseTime(text)
}
