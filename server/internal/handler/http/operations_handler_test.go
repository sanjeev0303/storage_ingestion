package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/server/middleware"
)

func TestWorkerHealthStates(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	beats := []model.WorkerHeartbeat{
		{Kind: "processing", Instance: "a", LastSeenAt: now.Add(-10 * time.Second)},
		{Kind: "processing", Instance: "b", LastSeenAt: now.Add(-time.Hour)},
		{Kind: "upload", Instance: "c", LastSeenAt: now.Add(-5 * time.Minute)},
		{Kind: "repack", Instance: "d", LastSeenAt: now},
	}
	got := map[string]map[string]any{}
	for _, w := range workerHealth(beats, now, false) {
		got[w["kind"].(string)] = w
	}

	if got["processing"]["state"] != "RUNNING" || got["processing"]["last_seen_at"] != now.Add(-10*time.Second) {
		t.Fatalf("processing: %+v", got["processing"])
	}
	if got["upload"]["state"] != "STALE" {
		t.Fatalf("upload: %+v", got["upload"])
	}
	if got["webhook"]["state"] != "NOT_SEEN" || got["webhook"]["last_seen_at"] != nil {
		t.Fatalf("webhook: %+v", got["webhook"])
	}
	if got["repack"]["state"] != "RUNNING" {
		t.Fatalf("a kind outside the known list is still reported: %+v", got)
	}
	if len(got) != len(model.WorkerKinds)+1 {
		t.Fatalf("kinds: %d", len(got))
	}
	if _, ok := got["processing"]["instances"]; ok {
		t.Fatal("members see no instances")
	}

	detailed := workerHealth(beats, now, true)
	for _, w := range detailed {
		if w["kind"] == "processing" && len(w["instances"].([]map[string]any)) != 2 {
			t.Fatalf("admin instances: %+v", w)
		}
	}
}

func TestOperationsStatusShowsInstancesOnlyToPlatformAdmins(t *testing.T) {
	now := time.Now().UTC()
	status := func(context.Context, string, time.Time) (model.OperationsStatus, error) {
		return model.OperationsStatus{At: now, Workers: []model.WorkerHeartbeat{
			{Kind: "processing", Instance: "host-1", StartedAt: now, LastSeenAt: now},
		}}, nil
	}
	h := NewOperationsHandler(status, slog.New(slog.NewTextHandler(io.Discard, nil)))

	call := func(p model.Principal) (int, []map[string]any) {
		req := httptest.NewRequest(nethttp.MethodGet, "/api/v1/operations/status", nil)
		req = req.WithContext(middleware.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		h.Status(rec, req)
		var out struct {
			Workers []map[string]any `json:"workers"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Workers
	}

	member := model.Principal{TenantID: "t1", UserID: "u1", Permissions: model.PermIngestionRead}
	code, workers := call(member)
	if code != nethttp.StatusOK || len(workers) != len(model.WorkerKinds) {
		t.Fatalf("member: %d %+v", code, workers)
	}
	for _, w := range workers {
		if _, ok := w["instances"]; ok {
			t.Fatalf("a member sees instances: %+v", w)
		}
	}

	admin := member
	admin.PlatformRole = model.PlatformAdmin
	_, workers = call(admin)
	if workers[1]["kind"] != "processing" || workers[1]["state"] != "RUNNING" || workers[1]["instances"] == nil {
		t.Fatalf("admin: %+v", workers[1])
	}

	if code, _ := call(model.Principal{TenantID: "t1", UserID: "u2"}); code != nethttp.StatusForbidden {
		t.Fatalf("without ingestion.read: %d", code)
	}
}
