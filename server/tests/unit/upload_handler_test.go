package unit

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	httpapi "github.com/sanjeev0303/object-storage-platform/internal/handler/http"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/repository/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
	uploadservice "github.com/sanjeev0303/object-storage-platform/internal/service/upload"
)

func newTestUploadHandler() *httpapi.UploadHandler {
	repo := memory.NewUploadRepository()
	svc := uploadservice.NewService(
		repo,
		&fakeObjectStorage{},
		"test-bucket",
		1024*1024,
		time.Minute,
	)

	return httpapi.NewUploadHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// asMember adds the principal the authentication middleware would.
func asMember(r *http.Request) *http.Request {
	p := model.Principal{TenantID: "tenant-1", UserID: "user-1", Permissions: model.PermUploadsWrite}
	return r.WithContext(middleware.WithPrincipal(r.Context(), p))
}

func TestCreateUploadHandler(t *testing.T) {
	handler := newTestUploadHandler()

	body := []byte(`{
		"file_name": "report.pdf",
		"content_type": "application/pdf",
		"size_bytes": 1024,
		"idempotency_key": "request-1"
	}`)

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/uploads",
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	handler.Create(recorder, asMember(req))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s",
			recorder.Code, recorder.Body.String())
	}

	var response struct {
		UploadID  string `json:"upload_id"`
		Status    string `json:"status"`
		UploadURL string `json:"upload_url"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if response.UploadID == "" {
		t.Fatal("expected upload_id in response")
	}

	if response.Status != "PENDING" {
		t.Fatalf("expected PENDING, got %q", response.Status)
	}

	if response.UploadURL == "" {
		t.Fatal("expected upload_url in response")
	}
}

func TestCreateUploadHandlerRejectsInvalidJSON(t *testing.T) {
	handler := newTestUploadHandler()

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/uploads",
		bytes.NewBufferString(`{"file_name":`),
	)
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	handler.Create(recorder, asMember(req))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestCreateUploadHandlerRejectsUnknownFields(t *testing.T) {
	handler := newTestUploadHandler()

	body := []byte(`{
		"file_name": "report.pdf",
		"content_type": "application/pdf",
		"size_bytes": 1024,
		"idempotency_key": "request-1",
		"tenant_id": "attacker-selected-tenant"
	}`)

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/uploads",
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	handler.Create(recorder, asMember(req))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}
