package retry

import (
	"errors"
	"math/rand/v2"
	"strings"
	"time"
)

// Policy describes bounded exponential backoff.
type Policy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
}

// Delay returns the wait before the next try after `attempt` failed tries
// (attempt starts at 1). The delay doubles each attempt, is capped at
// MaxDelay, and is jittered into [d/2, d] so workers do not retry in step.
func (p Policy) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	d := p.BaseDelay
	for i := 1; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if d <= 1 {
		return d
	}

	half := d / 2
	return half + time.Duration(rand.Int64N(int64(d-half)+1))
}

// Exhausted reports whether no try is left after `attempt` tries.
func (p Policy) Exhausted(attempt int) bool {
	return attempt >= p.MaxAttempts
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as one that retrying cannot fix: bad input,
// authorization or configuration.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent reports whether err, or anything it wraps, was marked Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Sanitize turns an error into the short, single-line text that is stored
// with a task, job or event and logged. Errors reaching it carry provider
// codes and operation names, never presigned URLs, credentials or file
// content. As a second line of defence, a URL's query string and any
// signature parameter are cut out, so a signed URL that slipped into an
// error is stored without its signature.
func Sanitize(err error) string {
	if err == nil {
		return ""
	}
	fields := strings.Fields(err.Error())
	for i, field := range fields {
		if strings.Contains(field, "://") && strings.Contains(field, "?") {
			base, _, _ := strings.Cut(field, "?")
			fields[i] = base + "?[redacted]"
		} else if strings.Contains(field, "X-Amz-") || strings.Contains(field, "Signature=") {
			fields[i] = "[redacted]"
		}
	}
	text := strings.Join(fields, " ")
	const limit = 500
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}
