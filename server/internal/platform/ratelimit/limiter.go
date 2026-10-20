// Package ratelimit counts requests per key in memory. It uses a sliding
// window counter: the previous window's count, weighted by how much of it
// still overlaps the sliding window, plus the current window's count. That
// avoids a fixed window's double burst at the boundary without keeping a
// timestamp per request.
//
// The counts live in one process. Several API instances would each allow
// the full rate; a shared store (PostgreSQL or Redis) would be needed to
// enforce one limit across them.
package ratelimit

import (
	"sync"
	"time"
)

// Rule allows Requests per Window.
type Rule struct {
	Requests int
	Window   time.Duration
}

type counter struct {
	window   time.Duration
	start    time.Time // start of the current window
	current  int
	previous int
}

// Limiter holds a counter per key. Counters idle for two windows are
// dropped, so memory follows the active keys.
type Limiter struct {
	mu        sync.Mutex
	counters  map[string]*counter
	now       func() time.Time
	lastSweep time.Time
}

func New() *Limiter {
	return &Limiter{counters: map[string]*counter{}, now: time.Now}
}

// Allow counts one request under key and reports whether it is within
// rule. When it is not, it says how long until a request would be.
func (l *Limiter) Allow(key string, rule Rule) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, now := l.counter(key, rule), l.now()
	if wait := l.wait(c, rule, now); wait > 0 {
		return false, wait
	}
	c.current++
	return true, 0
}

// Exceeded reports, without counting anything, whether key has used up
// rule, and if so how long until it has not.
func (l *Limiter) Exceeded(key string, rule Rule) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	wait := l.wait(l.counter(key, rule), rule, l.now())
	return wait > 0, wait
}

// Add counts one event under key without checking the rule: a failed
// sign-in, for example.
func (l *Limiter) Add(key string, rule Rule) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counter(key, rule).current++
}

// Reset forgets key, after a successful sign-in for example.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.counters, key)
}

// counter returns key's counter, rolled forward to the current window.
// The caller holds the lock.
func (l *Limiter) counter(key string, rule Rule) *counter {
	now := l.now()
	l.sweep(now)
	c := l.counters[key]
	if c == nil || c.window != rule.Window {
		c = &counter{window: rule.Window, start: now.Truncate(rule.Window)}
		l.counters[key] = c
	}
	elapsed := now.Sub(c.start)
	switch {
	case elapsed >= 2*c.window:
		c.start, c.previous, c.current = now.Truncate(c.window), 0, 0
	case elapsed >= c.window:
		c.start, c.previous, c.current = c.start.Add(c.window), c.current, 0
	}
	return c
}

// wait is how long until one more request fits rule; 0 if it fits now.
func (l *Limiter) wait(c *counter, rule Rule, now time.Time) time.Duration {
	into := now.Sub(c.start)
	overlap := 1 - float64(into)/float64(c.window)
	estimate := float64(c.previous)*overlap + float64(c.current)
	if estimate < float64(rule.Requests) {
		return 0
	}
	// The estimate falls as the previous window slides out; past the
	// current window's end only the current count remains.
	return max(c.window-into, time.Second)
}

// sweep drops counters idle for two windows, at most once a minute.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for key, c := range l.counters {
		if now.Sub(c.start) >= 2*c.window {
			delete(l.counters, key)
		}
	}
}

// Size is the number of keys held, for metrics.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.counters)
}
