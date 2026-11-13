package middleware

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// BodyDeadline gives a request body a read deadline that keeps pace with
// it: grace to start, and more time only as bytes arrive at minRate bytes
// a second or faster. A client trickling a body to hold a connection (and
// a handler) open is cut off after grace, while a large body that keeps
// moving, such as an inline manifest, gets as long as it needs. The
// server-wide ReadTimeout cannot do both. Object bytes do not pass through
// the API at all: they go to presigned URLs.
//
// The deadline ends with the body, so a handler that works for long after
// reading its body is not cut off.
func BodyDeadline(grace time.Duration, minRate int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		start := time.Now()
		// A writer that cannot take deadlines (a test recorder) is served
		// without one.
		if err := rc.SetReadDeadline(start.Add(grace)); err == nil {
			r.Body = &pacedBody{ReadCloser: r.Body, rc: rc, start: start, grace: grace, minRate: minRate}
		}
		next.ServeHTTP(w, r)
	})
}

type pacedBody struct {
	io.ReadCloser
	rc      *http.ResponseController
	start   time.Time
	grace   time.Duration
	minRate int64
	read    int64
	done    bool
}

func (b *pacedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.done {
		return n, err
	}
	b.read += int64(n)
	if errors.Is(err, io.EOF) {
		// net/http lifts the deadline itself at the end of the body. One
		// set now would cancel a handler still working after it.
		b.done = true
		return n, err
	}
	earned := time.Duration(float64(b.read) / float64(b.minRate) * float64(time.Second))
	_ = b.rc.SetReadDeadline(b.start.Add(b.grace + earned))
	return n, err
}
