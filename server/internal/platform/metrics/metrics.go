// Package metrics keeps process counters and gauges without a dependency.
// A Registry is created per process and passed in explicitly; Publish
// exposes it through the standard library's expvar (/debug/vars). Names
// are low-cardinality: never tenant, upload or task IDs.
package metrics

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type Registry struct {
	mu     sync.Mutex
	values map[string]int64
}

func New() *Registry {
	return &Registry{values: make(map[string]int64)}
}

// Add increases a counter. A nil registry ignores it, so metrics are optional.
func (r *Registry) Add(name string, delta int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.values[name] += delta
	r.mu.Unlock()
}

// Set records a gauge's current value.
func (r *Registry) Set(name string, value int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.values[name] = value
	r.mu.Unlock()
}

// Snapshot copies the current values.
func (r *Registry) Snapshot() map[string]int64 {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out
}

// Publish exposes the registry under name at /debug/vars. Call it once per
// process.
func (r *Registry) Publish(name string) {
	expvar.Publish(name, expvar.Func(func() any { return r.Snapshot() }))
}

// Serve serves a worker's operational endpoints at addr in the
// background: the published registries (/debug/vars), liveness (/health:
// the process answers) and readiness (/ready: ready, usually a database
// ping, answers within two seconds). stop shuts the listener down and
// lets a request in progress finish.
func Serve(addr string, ready func(context.Context) error, log *slog.Logger) (stop func()) {
	mux := http.NewServeMux()
	mux.Handle("/debug/vars", expvar.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, "not ready: database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener stopped", "error", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	}
}
