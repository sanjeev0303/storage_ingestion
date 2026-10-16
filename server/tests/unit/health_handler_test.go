package unit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	httpapi "github.com/sanjeev0303/object-storage-platform/internal/handler/http"
)

func TestHealthHandler(t *testing.T) {
	handler := httpapi.NewHealthHandler()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusOK,
			recorder.Code,
		)
	}

	var response struct {
		Status    string    `json:"status"`
		Timestamp time.Time `json:"timestamp"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if response.Status != "ok" {
		t.Errorf("expected status ok, got %q", response.Status)
	}

	if response.Timestamp.IsZero() {
		t.Error("expected a non-zero timestamp")
	}
}

func TestHealthHandlerRejectsPost(t *testing.T) {
	handler := httpapi.NewHealthHandler()

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf(
			"expected status %d, got %d",
			http.StatusMethodNotAllowed,
			recorder.Code,
		)
	}

	if got := recorder.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("expected Allow: GET, got %q", got)
	}
}
