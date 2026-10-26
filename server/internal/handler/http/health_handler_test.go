package http

import (
	"context"
	"encoding/json"
	"errors"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadinessChecksTheDatabaseAndHidesTheCause(t *testing.T) {
	var down error
	h := NewReadinessHandler(func(context.Context) error { return down })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, "/ready", nil))
	var out readinessResponse
	if rec.Code != nethttp.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Status != "ready" {
		t.Fatalf("ready: %d %s", rec.Code, rec.Body)
	}

	down = errors.New(`dial tcp 10.0.0.5:5432: password authentication failed for user "app"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, "/ready", nil))
	if rec.Code != nethttp.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &out) != nil ||
		out.Status != "not_ready" || out.Checks["database"] != "unavailable" {
		t.Fatalf("not ready: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("the cause leaked: %s", rec.Body)
	}

	// Liveness never looks at the database.
	rec = httptest.NewRecorder()
	NewHealthHandler().ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, "/health", nil))
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("liveness: %d", rec.Code)
	}
}
