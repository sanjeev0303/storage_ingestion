package middleware

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
)

// ClientIP records the client's address in the request context, for
// sign-in throttling and audit records. The connection's address is used
// unless trustProxy is set, in which case the first X-Forwarded-For entry
// is: only correct behind a proxy that sets that header itself.
func ClientIP(trustProxy bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := RemoteIP(r, trustProxy)
		next.ServeHTTP(w, r.WithContext(correlation.WithSource(r.Context(), ip)))
	})
}

// RemoteIP is the client address ClientIP records.
func RemoteIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first, _, _ := strings.Cut(forwarded, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Identity is what the bearer resolver needs from the identity service.
type Identity[I any] interface {
	Authenticate(ctx context.Context, accessToken string) (I, error)
	Principal(ctx context.Context, id I, orgID string) (model.Principal, error)
}

// BearerResolver authenticates the request's bearer access token and
// resolves the organization named by X-Organization-ID against the
// caller's memberships. The header only names a choice: the membership
// check decides.
func BearerResolver[I any](svc Identity[I]) Resolver {
	return func(r *http.Request) (model.Principal, error) {
		token := BearerToken(r)
		if token == "" {
			return model.Principal{}, model.ErrUnauthenticated
		}
		id, err := svc.Authenticate(r.Context(), token)
		if err != nil {
			return model.Principal{}, err
		}
		return svc.Principal(r.Context(), id, strings.TrimSpace(r.Header.Get("X-Organization-ID")))
	}
}

// RateLimit refuses requests over rule with 429, counting them under the
// key key returns (a tenant, a user or an address). An empty key is not
// limited here.
func RateLimit(limiter *ratelimit.Limiter, name string, rule ratelimit.Rule, key func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := key(r)
		if k != "" {
			if ok, wait := limiter.Allow(name+":"+k, rule); !ok {
				TooManyRequests(w, wait)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// TooManyRequests answers 429 with when to try again.
func TooManyRequests(w http.ResponseWriter, wait time.Duration) {
	seconds := int(wait.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeProblem(w, http.StatusTooManyRequests, "rate_limited", "too many requests; retry after "+strconv.Itoa(seconds)+"s")
}

// ByTenant keys a limit on the request's organization; ByUser on its
// user; ByAddress on its client address.
func ByTenant(r *http.Request) string {
	p, _ := PrincipalFrom(r.Context())
	return p.TenantID
}

func ByUser(r *http.Request) string {
	p, _ := PrincipalFrom(r.Context())
	return p.UserID
}

func ByAddress(r *http.Request) string {
	return correlation.Source(r.Context())
}

// TenantRequests applies each organization's own request-rate quota:
// policy returns its requests per minute.
func TenantRequests(limiter *ratelimit.Limiter, policy func(ctx context.Context, tenantID string) (int, error), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		if p.TenantID != "" {
			perMinute, err := policy(r.Context(), p.TenantID)
			if err != nil {
				// Fail closed: without the policy the request is not let
				// through unmetered.
				writeProblem(w, http.StatusServiceUnavailable, "quota_unavailable", "the request quota could not be checked")
				return
			}
			if ok, wait := limiter.Allow("tenant_requests:"+p.TenantID, ratelimit.Rule{Requests: perMinute, Window: time.Minute}); !ok {
				TooManyRequests(w, wait)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
