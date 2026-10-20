package unit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/correlation"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/ratelimit"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
)

var reached = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem body: %v: %s", err, rec.Body)
	}
	return body.Code
}

func TestAuthenticateFailsClosed(t *testing.T) {
	req := func() *http.Request { return httptest.NewRequest(http.MethodGet, "/x", nil) }

	// No resolver configured: nothing gets through, never anonymously.
	rec := serve(middleware.Authenticate(nil, reached), req())
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("nil resolver: %d %v", rec.Code, rec.Header())
	}

	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{model.ErrSessionInvalid, http.StatusUnauthorized, "unauthenticated"},
		{errors.New("database down"), http.StatusUnauthorized, "unauthenticated"},
		{model.ErrForbidden, http.StatusForbidden, "not_a_member"},
		{model.ErrOrgInactive, http.StatusForbidden, "organization_inactive"},
	} {
		resolve := func(*http.Request) (model.Principal, error) { return model.Principal{}, c.err }
		rec := serve(middleware.Authenticate(resolve, reached), req())
		if rec.Code != c.status || problemCode(t, rec) != c.code {
			t.Fatalf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}

	// A principal without a user is not authenticated either.
	empty := func(*http.Request) (model.Principal, error) { return model.Principal{TenantID: "t1"}, nil }
	if rec := serve(middleware.Authenticate(empty, reached), req()); rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty principal: %d", rec.Code)
	}

	// No organization: tenant routes refuse.
	noOrg := func(*http.Request) (model.Principal, error) { return model.Principal{UserID: "u1"}, nil }
	rec = serve(middleware.Authenticate(noOrg, middleware.RequireTenant(reached)), req())
	if rec.Code != http.StatusBadRequest || problemCode(t, rec) != "organization_required" {
		t.Fatalf("no organization: %d %s", rec.Code, rec.Body)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer sat_abc": "sat_abc", "bearer  sat_abc ": "sat_abc", "Basic dXNlcjpwYXNz": "", "sat_abc": "", "": "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Authorization", header)
		if got := middleware.BearerToken(r); got != want {
			t.Fatalf("%q: got %q, want %q", header, got, want)
		}
	}
}

func TestRateLimitAnswers429WithRetryAfter(t *testing.T) {
	limiter := ratelimit.New()
	h := middleware.RateLimit(limiter, "listing", ratelimit.Rule{Requests: 2, Window: time.Minute}, middleware.ByAddress, reached)
	req := func(ip string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		return r.WithContext(correlation.WithSource(r.Context(), ip))
	}
	for i := 0; i < 2; i++ {
		if rec := serve(h, req("198.51.100.1")); rec.Code != http.StatusNoContent {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rec := serve(h, req("198.51.100.1"))
	retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != http.StatusTooManyRequests || err != nil || retry < 1 || retry > 60 || problemCode(t, rec) != "rate_limited" {
		t.Fatalf("third request: %d Retry-After=%q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	// Counted per key: another address is not affected.
	if rec := serve(h, req("198.51.100.2")); rec.Code != http.StatusNoContent {
		t.Fatalf("another address: %d", rec.Code)
	}
}

func TestClientAddressIgnoresForwardedHeadersUnlessTrusted(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "203.0.113.5:4444"
	r.Header.Set("X-Forwarded-For", "10.9.9.9, 203.0.113.5")
	if got := middleware.RemoteIP(r, false); got != "203.0.113.5" {
		t.Fatalf("untrusted: %s", got)
	}
	if got := middleware.RemoteIP(r, true); got != "10.9.9.9" {
		t.Fatalf("trusted: %s", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-address")
	if got := middleware.RemoteIP(r, true); got != "203.0.113.5" {
		t.Fatalf("a malformed header falls back to the connection: %s", got)
	}
}

func TestTenantRequestQuotaFailsClosed(t *testing.T) {
	limiter := ratelimit.New()
	withTenant := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		return r.WithContext(middleware.WithPrincipal(r.Context(), model.Principal{TenantID: "t1", UserID: "u1"}))
	}
	broken := func(context.Context, string) (int, error) { return 0, errors.New("database down") }
	if rec := serve(middleware.TenantRequests(limiter, broken, reached), withTenant()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unknown quota must not let requests through: %d", rec.Code)
	}

	one := func(context.Context, string) (int, error) { return 1, nil }
	h := middleware.TenantRequests(limiter, one, reached)
	if rec := serve(h, withTenant()); rec.Code != http.StatusNoContent {
		t.Fatalf("first request: %d", rec.Code)
	}
	if rec := serve(h, withTenant()); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the tenant's quota: %d", rec.Code)
	}
}
