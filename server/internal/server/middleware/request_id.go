package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
)

// RequestID gives every request a correlation ID: the caller's
// X-Request-ID when it is short and plain, otherwise a new one. It is
// echoed in the response and passed on through the context, where events
// written while handling the request pick it up as their correlation ID.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !plainID(id) {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(correlation.With(r.Context(), id)))
	})
}

// RequestIDFrom returns the request's correlation ID, if any.
func RequestIDFrom(ctx context.Context) string {
	return correlation.From(ctx)
}

// plainID accepts 1-64 characters of [A-Za-z0-9._-], so a caller cannot
// inject anything into logs or headers through it.
func plainID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
