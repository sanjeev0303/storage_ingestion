package http

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type healthResponse struct {
	Status    string    `json:"status"`
	Timestamp time.Time `json:"timestamp"`
}

// HealthHandler reports liveness: the api process is responding. It checks
// no dependency, so a database outage never gets the process restarted;
// ReadinessHandler is the one to route traffic by.
type HealthHandler struct{}

func NewHealthHandler() *HealthHandler {
	return &HealthHandler{}
}

func (h *HealthHandler) ServeHTTP(
	w http.ResponseWriter,
	r *http.Request,
) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(
			w,
			"method not allowed",
			http.StatusMethodNotAllowed,
		)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := healthResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC(),
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		// At this point the response may already be committed.
		// The server logger will be wired into handlers later.
		return
	}
}

// ReadinessHandler reports whether the api can serve: the database answers
// within two seconds. The cause of a failure stays in the server's logs.
type ReadinessHandler struct {
	check func(context.Context) error
}

func NewReadinessHandler(check func(context.Context) error) *ReadinessHandler {
	return &ReadinessHandler{check: check}
}

type readinessResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (h *ReadinessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status, out := http.StatusOK, readinessResponse{Status: "ready", Checks: map[string]string{"database": "ok"}}
	if err := h.check(ctx); err != nil {
		status, out = http.StatusServiceUnavailable, readinessResponse{Status: "not_ready", Checks: map[string]string{"database": "unavailable"}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
}
