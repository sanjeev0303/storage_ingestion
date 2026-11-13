package ingestion

import (
	"context"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// entryBatcher is the repository's optional batch of entry changes
// (postgres IngestionRepository.TransitionMany). A repository without it,
// such as a test's wrapper, gets every change one at a time.
type entryBatcher interface {
	TransitionMany(ctx context.Context, changes []interfaces.EntryChange) ([]interfaces.EntryOutcome, error)
}

// maxGroupCommit caps how many entry changes share one transaction.
const maxGroupCommit = 100

// groupWriteTimeout bounds one batch's transaction, whichever caller writes it.
const groupWriteTimeout = 30 * time.Second

// coalescer is a group commit for the hot per-file entry changes (an upload
// completed, an entry published). Each one updates its tenant's usage row,
// and those row locks were the per-tenant ceiling: concurrent changes queued
// on the row one commit at a time. Here the changes that arrive while a batch
// is being written wait for it and go in the next batch together, so under
// load many entries share one lock and one commit, and an idle system adds no
// delay at all.
//
// There is no background goroutine. The first caller with nothing running
// leads: it writes the pending changes, its own among them, then hands the
// lead to the oldest change still waiting, whose caller writes the next
// batch. A leader never writes more than the batch holding its own change,
// so no caller is kept working for others indefinitely, and the queue always
// has a leader while anything waits in it.
type coalescer struct {
	mu      sync.Mutex
	pending []*groupedChange
	running bool
}

type groupedChange struct {
	change interfaces.EntryChange
	done   chan struct{} // closed once result is set
	lead   chan struct{} // closed when this caller is to lead the next batch
	leader bool          // set under mu together with closing lead
	result interfaces.EntryOutcome
}

// apply writes one change, through a batch shared with whatever else is
// pending when batch is set.
func (c *coalescer) apply(ctx context.Context, change interfaces.EntryChange, batch entryBatcher,
	single func(context.Context, interfaces.EntryChange) (model.IngestionEntry, error), counted func(n int)) (model.IngestionEntry, error) {
	g := &groupedChange{change: change, done: make(chan struct{}), lead: make(chan struct{})}
	c.mu.Lock()
	c.pending = append(c.pending, g)
	if !c.running {
		c.running = true
		g.leader = true
		close(g.lead)
	}
	c.mu.Unlock()

	select {
	case <-g.done:
		return g.result.Entry, g.result.Err
	case <-g.lead:
	case <-ctx.Done():
		c.mu.Lock()
		if !g.leader {
			// Not leading: take the change back if no batch has taken it yet.
			for i, p := range c.pending {
				if p == g {
					c.pending = append(c.pending[:i:i], c.pending[i+1:]...)
					break
				}
			}
			c.mu.Unlock()
			return model.IngestionEntry{}, ctx.Err()
		}
		c.mu.Unlock()
		// Leadership was handed over just as the caller gave up: it must
		// still write the batch, or the changes behind it would wait forever.
	}

	c.mu.Lock()
	n := min(len(c.pending), maxGroupCommit)
	group := append([]*groupedChange(nil), c.pending[:n]...)
	c.pending = append([]*groupedChange(nil), c.pending[n:]...)
	c.mu.Unlock()

	// The batch is written whatever happens to this caller's request: the
	// other callers in it are waiting for their results.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), groupWriteTimeout)
	c.write(writeCtx, group, batch, single, counted)
	cancel()

	c.mu.Lock()
	if len(c.pending) > 0 {
		next := c.pending[0]
		next.leader = true
		close(next.lead)
	} else {
		c.running = false
	}
	c.mu.Unlock()

	// A leader is always the oldest change waiting, so its own change was in
	// the batch it just wrote.
	<-g.done
	return g.result.Entry, g.result.Err
}

// write applies a group and hands every caller its result. A failed batch is
// retried one change at a time, which is exactly the path without batching.
func (c *coalescer) write(ctx context.Context, group []*groupedChange, batch entryBatcher,
	single func(context.Context, interfaces.EntryChange) (model.IngestionEntry, error), counted func(n int)) {
	if len(group) > 1 {
		changes := make([]interfaces.EntryChange, len(group))
		for i, g := range group {
			changes[i] = g.change
		}
		if outcomes, err := batch.TransitionMany(ctx, changes); err == nil {
			for i, g := range group {
				g.result = outcomes[i]
				close(g.done)
			}
			counted(len(group))
			return
		}
	}
	for _, g := range group {
		entry, err := single(ctx, g.change)
		g.result = interfaces.EntryOutcome{Entry: entry, Err: err}
		close(g.done)
	}
}

// transition applies a hot per-file entry change through the group commit,
// or on its own when the repository cannot batch or group commit is off.
func (s *Service) transition(ctx context.Context, change interfaces.EntryChange) (model.IngestionEntry, error) {
	batch, ok := s.deps.Repo.(entryBatcher)
	if !ok || s.cfg.NoGroupCommit {
		return s.deps.Repo.Transition(ctx, change)
	}
	return s.group.apply(ctx, change, batch, s.deps.Repo.Transition, func(n int) {
		s.count("ingestion_group_commits", 1)
		s.count("ingestion_group_commit_entries", int64(n))
	})
}
