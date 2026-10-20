package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Compactor runs compaction rules in PostgreSQL. Each batch is one short
// statement over an indexed, ordered selection of at most $2 rows older
// than $1; SKIP LOCKED leaves rows another transaction holds to a later
// batch instead of waiting on them.
type Compactor struct {
	db *DB
}

var _ interfaces.Compactor = (*Compactor)(nil)

func NewCompactor(db *DB) *Compactor {
	return &Compactor{db: db}
}

// compactionRule pairs a rule's batch statement with the selection it
// works on, which Count also uses.
type compactionRule struct {
	// selection returns the rows a batch may affect, oldest first.
	selection string
	// batch affects the rows of selection (its result as "batch").
	batch string
}

var compactionRules = map[string]compactionRule{
	"reconciliation_tasks": {
		selection: `SELECT id FROM reconciliation_tasks
			WHERE state = 'SUCCEEDED' AND updated_at < $1
			ORDER BY updated_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM reconciliation_tasks WHERE id IN (SELECT id FROM batch)`,
	},
	"upload_parts": {
		selection: `SELECT p.upload_id, p.part_number FROM upload_parts AS p
			JOIN upload_sessions AS s ON s.id = p.upload_id
			WHERE s.status = 'VERIFIED' AND s.updated_at < $1
			ORDER BY s.updated_at, p.upload_id, p.part_number LIMIT $2 FOR UPDATE OF p SKIP LOCKED`,
		batch: `DELETE FROM upload_parts WHERE (upload_id, part_number) IN (SELECT upload_id, part_number FROM batch)`,
	},
	// Only finished sessions lose their key, so a client retry of a
	// session still in flight can never create a duplicate.
	"upload_idempotency_keys": {
		selection: `SELECT id FROM upload_sessions
			WHERE idempotancy_key IS NOT NULL AND created_at < $1
			  AND status IN ('COMPLETED', 'VERIFIED', 'ABORTED', 'FAILED', 'EXPIRED')
			ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `UPDATE upload_sessions SET idempotancy_key = NULL WHERE id IN (SELECT id FROM batch)`,
	},
	"download_tokens": {
		selection: `SELECT id FROM download_tokens
			WHERE expires_at < $1
			ORDER BY expires_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM download_tokens WHERE id IN (SELECT id FROM batch)`,
	},
	"orphan_candidates": {
		selection: `SELECT id FROM orphan_candidates
			WHERE status IN ('DELETED', 'CLEARED') AND resolved_at < $1
			ORDER BY resolved_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM orphan_candidates WHERE id IN (SELECT id FROM batch)`,
	},
	// Keeps each prefix's latest completed scan (the time and scope of the
	// last successful scan) and every scan an open candidate refers to.
	"inventory_scans": {
		selection: `SELECT s.id FROM inventory_scans AS s
			WHERE s.status <> 'RUNNING' AND s.updated_at < $1
			  AND s.id <> COALESCE((
				SELECT l.id FROM inventory_scans AS l
				WHERE l.bucket = s.bucket AND l.prefix = s.prefix AND l.status = 'COMPLETED'
				ORDER BY l.completed_at DESC LIMIT 1), '')
			  AND NOT EXISTS (
				SELECT 1 FROM orphan_candidates AS c
				WHERE c.last_scan_id = s.id AND c.status IN ('MARKED', 'FAILED'))
			ORDER BY s.updated_at, s.id LIMIT $2`,
		batch: `DELETE FROM inventory_scans WHERE id IN (SELECT id FROM batch)`,
	},
	"compaction_runs": {
		selection: `SELECT id FROM compaction_runs
			WHERE started_at < $1
			ORDER BY started_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM compaction_runs WHERE id IN (SELECT id FROM batch)`,
	},
	// The latest event of each aggregate stays: the next event's sequence
	// number continues from it.
	"outbox_events": {
		selection: `SELECT o.id FROM outbox_events AS o
			WHERE o.status = 'PUBLISHED' AND o.published_at < $1
			  AND EXISTS (
				SELECT 1 FROM outbox_events AS newer
				WHERE newer.aggregate_type = o.aggregate_type AND newer.aggregate_id = o.aggregate_id
				  AND newer.aggregate_sequence > o.aggregate_sequence)
			  AND NOT EXISTS (SELECT 1 FROM dead_letters AS d WHERE d.event_id = o.id AND d.status = 'OPEN')
			ORDER BY o.published_at, o.id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM outbox_events WHERE id IN (SELECT id FROM batch)`,
	},
	"event_inbox": {
		selection: `SELECT consumer, event_id FROM event_inbox
			WHERE status IN ('COMPLETED', 'SKIPPED') AND completed_at < $1
			ORDER BY completed_at, event_id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM event_inbox WHERE (consumer, event_id) IN (SELECT consumer, event_id FROM batch)`,
	},
	"dead_letters": {
		selection: `SELECT id FROM dead_letters
			WHERE status <> 'OPEN' AND resolved_at < $1
			ORDER BY resolved_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM dead_letters WHERE id IN (SELECT id FROM batch)`,
	},
	"event_discrepancies": {
		selection: `SELECT id FROM event_discrepancies
			WHERE created_at < $1
			ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM event_discrepancies WHERE id IN (SELECT id FROM batch)`,
	},
	"reconciliation_findings": {
		selection: `SELECT id FROM reconciliation_findings
			WHERE created_at < $1
			ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM reconciliation_findings WHERE id IN (SELECT id FROM batch)`,
	},
	"delivery_audit_events": {
		selection: `SELECT id FROM delivery_audit_events
			WHERE created_at < $1
			ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM delivery_audit_events WHERE id IN (SELECT id FROM batch)`,
	},
	"processing_events": {
		selection: `SELECT id FROM processing_events
			WHERE created_at < $1
			ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM processing_events WHERE id IN (SELECT id FROM batch)`,
	},
	"lifecycle_events": {
		selection: `SELECT id FROM lifecycle_events
			WHERE created_at < $1
			ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`,
		batch: `DELETE FROM lifecycle_events WHERE id IN (SELECT id FROM batch)`,
	},
}

func (c *Compactor) rule(name string) (compactionRule, error) {
	rule, ok := compactionRules[name]
	if !ok {
		return compactionRule{}, fmt.Errorf("unknown compaction rule %q", name)
	}
	return rule, nil
}

func (c *Compactor) Count(ctx context.Context, name string, cutoff time.Time, limit int) (int64, error) {
	rule, err := c.rule(name)
	if err != nil {
		return 0, err
	}
	// Counting must not lock anything, so the row locks are dropped.
	selection := stripLocking(rule.selection)
	var n int64
	if err := c.db.Pool.QueryRow(ctx, `SELECT count(*) FROM (`+selection+`) AS batch`, cutoff, limit).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", name, err)
	}
	return n, nil
}

func (c *Compactor) CompactBatch(ctx context.Context, name string, cutoff time.Time, limit int) (int64, error) {
	rule, err := c.rule(name)
	if err != nil {
		return 0, err
	}
	tag, err := c.db.Pool.Exec(ctx, `WITH batch AS (`+rule.selection+`) `+rule.batch, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("compact %s: %w", name, err)
	}
	return tag.RowsAffected(), nil
}

func (c *Compactor) RecordRun(ctx context.Context, run model.CompactionRun) error {
	_, err := c.db.Pool.Exec(ctx, `
		INSERT INTO compaction_runs (id, rule, status, rows_affected, batches, cutoff, started_at, finished_at, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''))
	`, run.ID, run.Rule, run.Status, run.RowsAffected, run.Batches, run.Cutoff, run.StartedAt, run.FinishedAt, run.LastError)
	if err != nil {
		return fmt.Errorf("record compaction run: %w", err)
	}
	return nil
}

func (c *Compactor) RecentRuns(ctx context.Context, limit int) ([]model.CompactionRun, error) {
	rows, err := c.db.Pool.Query(ctx, `
		SELECT id, rule, status, rows_affected, batches, cutoff, started_at, finished_at, COALESCE(last_error, '')
		FROM compaction_runs
		ORDER BY started_at DESC, id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list compaction runs: %w", err)
	}
	defer rows.Close()

	var runs []model.CompactionRun
	for rows.Next() {
		var run model.CompactionRun
		if err := rows.Scan(&run.ID, &run.Rule, &run.Status, &run.RowsAffected, &run.Batches, &run.Cutoff,
			&run.StartedAt, &run.FinishedAt, &run.LastError); err != nil {
			return nil, fmt.Errorf("scan compaction run: %w", err)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// stripLocking removes the trailing row-lock clause of a selection.
func stripLocking(selection string) string {
	for _, clause := range []string{" FOR UPDATE OF p SKIP LOCKED", " FOR UPDATE SKIP LOCKED"} {
		if len(selection) > len(clause) && selection[len(selection)-len(clause):] == clause {
			return selection[:len(selection)-len(clause)]
		}
	}
	return selection
}
