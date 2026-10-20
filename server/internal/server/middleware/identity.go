package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Resolver authenticates a request and returns its principal.
// BearerResolver is the one the API uses; tests may supply their own.
type Resolver func(r *http.Request) (model.Principal, error)

type principalKey struct{}

// Authenticate rejects requests without a valid principal and passes the
// principal to next through the request context. A nil resolver rejects
// everything: no authentication configured means no access, never
// anonymous access.
//
// Bad or missing credentials answer 401. A valid caller who is not an
// active member of the organization they named answers 403, the same
// whether the organization exists or not.
func Authenticate(resolve Resolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if resolve == nil {
			unauthorized(w)
			return
		}
		principal, err := resolve(r)
		switch {
		case errors.Is(err, model.ErrForbidden):
			writeProblem(w, http.StatusForbidden, "not_a_member", "you are not an active member of that organization")
			return
		case errors.Is(err, model.ErrOrgInactive):
			writeProblem(w, http.StatusForbidden, "organization_inactive", "the organization is suspended or deactivated")
			return
		case err != nil || !principal.Valid():
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
	})
}

// RequireTenant refuses a request whose principal has no organization:
// the caller belongs to several (or none) and named none.
func RequireTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok || p.TenantID == "" {
			writeProblem(w, http.StatusBadRequest, "organization_required",
				"name the organization with the X-Organization-ID header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WithPrincipal stores the authenticated principal in ctx.
func WithPrincipal(ctx context.Context, principal model.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalFrom returns the principal Authenticate stored, if any.
func PrincipalFrom(ctx context.Context) (model.Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(model.Principal)
	return principal, ok
}

// BearerToken returns the request's bearer token, or "".
func BearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="object-storage"`)
	writeProblem(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
}

// writeProblem is the error body every middleware answer uses.
func writeProblem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}
