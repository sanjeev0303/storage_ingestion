// Package concurrency bounds how much work runs at once.
package concurrency

import "context"

// Limiter lets at most n operations run at a time. Callers wait for a free
// slot or for their context to end; nothing is queued in memory beyond the
// waiting goroutines themselves.
type Limiter struct {
	slots chan struct{}
}

func NewLimiter(n int) *Limiter {
	if n < 1 {
		n = 1
	}
	return &Limiter{slots: make(chan struct{}, n)}
}

// Do runs fn once a slot is free. It returns ctx's error if the context
// ends first.
func (l *Limiter) Do(ctx context.Context, fn func(context.Context) error) error {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.slots }()
	return fn(ctx)
}

// InUse is how many operations hold a slot now.
func (l *Limiter) InUse() int { return len(l.slots) }
