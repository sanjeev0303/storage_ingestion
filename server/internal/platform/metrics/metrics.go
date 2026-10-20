// Package metrics keeps process counters and gauges without a dependency.
// A Registry is created per process and passed in explicitly; Publish
// exposes it through the standard library's expvar (/debug/vars). Names
// are low-cardinality: never tenant, upload or task IDs.
package metrics

import (
	"expvar"
	"sync"
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
