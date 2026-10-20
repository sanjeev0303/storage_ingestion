package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Compactor removes metadata that is no longer needed, one named rule at a
// time, in bounded and ordered batches. Each batch is its own short
// transaction, so a stopped run simply continues on the next one.
type Compactor interface {
	// Count returns how many rows the rule would affect for cutoff,
	// counting at most limit (dry runs).
	Count(ctx context.Context, rule string, cutoff time.Time, limit int) (int64, error)
	// CompactBatch affects at most limit rows of the rule older than
	// cutoff, oldest first, and returns how many it affected.
	CompactBatch(ctx context.Context, rule string, cutoff time.Time, limit int) (int64, error)
	RecordRun(ctx context.Context, run model.CompactionRun) error
	RecentRuns(ctx context.Context, limit int) ([]model.CompactionRun, error)
}
