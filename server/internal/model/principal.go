package model

import "errors"

// Principal is the authenticated caller. It comes only from the
// authentication layer, never from request bodies or query parameters.
type Principal struct {
	TenantID string
	UserID   string
}

// Valid reports whether both identifiers are present.
func (p Principal) Valid() bool {
	return p.TenantID != "" && p.UserID != ""
}

var ErrUnauthenticated = errors.New("authentication required")
