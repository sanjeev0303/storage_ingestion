package middleware

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Resolver authenticates a request and returns its principal. It is the
// seam a real authentication scheme (JWT, sessions, mTLS) plugs into.
type Resolver func(r *http.Request) (model.Principal, error)

type principalKey struct{}

// Authenticate rejects requests without a valid principal with 401 and
// passes the principal to next through the request context. A nil
// resolver rejects everything: no authentication configured means no
// access, never anonymous access.
func Authenticate(resolve Resolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if resolve == nil {
			unauthorized(w)
			return
		}
		principal, err := resolve(r)
		if err != nil || !principal.Valid() {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
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

// DevelopmentIdentity is the local placeholder identity the rest of the
// API already uses. It authenticates nobody: wire it only when
// APP_ENV=development.
func DevelopmentIdentity(tenantID, userID string) Resolver {
	return func(*http.Request) (model.Principal, error) {
		return model.Principal{TenantID: tenantID, UserID: userID}, nil
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
}
