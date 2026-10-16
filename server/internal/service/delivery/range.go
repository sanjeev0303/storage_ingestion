package delivery

import (
	"strconv"
	"strings"
)

// parseRange reads a single "bytes=" range against an object of size bytes
// and returns the inclusive bounds. Partial is false when the whole object
// should be sent: no header, a malformed one, or several ranges (RFC 9110
// lets a server ignore those). A well-formed range that starts past the
// end is ErrRangeNotSatisfiable.
func parseRange(header string, size int64) (first, last int64, partial bool, err error) {
	whole := func() (int64, int64, bool, error) { return 0, size - 1, false, nil }

	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if header == "" || !ok || strings.Contains(spec, ",") {
		return whole()
	}

	start, end, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return whole()
	}

	if start == "" {
		// Suffix range: the last n bytes.
		n, err := strconv.ParseInt(end, 10, 64)
		if err != nil || n < 0 {
			return whole()
		}
		if n == 0 {
			return 0, 0, false, ErrRangeNotSatisfiable
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, nil
	}

	first, err = strconv.ParseInt(start, 10, 64)
	if err != nil || first < 0 {
		return whole()
	}
	last = size - 1
	if end != "" {
		last, err = strconv.ParseInt(end, 10, 64)
		if err != nil || last < first {
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
