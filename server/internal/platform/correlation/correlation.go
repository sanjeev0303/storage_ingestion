// Package correlation carries one ID through a request and every event and
// job it leads to, so their log lines can be joined.
package correlation

import "context"

type key struct{}

// With returns ctx carrying id.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// From returns the correlation ID in ctx, or "".
func From(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}

type sourceKey struct{}

// WithSource returns ctx carrying the request's client address, as the
// server determined it, for audit records.
func WithSource(ctx context.Context, clientIP string) context.Context {
	return context.WithValue(ctx, sourceKey{}, clientIP)
}

// Source returns the client address in ctx, or "".
func Source(ctx context.Context) string {
	ip, _ := ctx.Value(sourceKey{}).(string)
	return ip
}
