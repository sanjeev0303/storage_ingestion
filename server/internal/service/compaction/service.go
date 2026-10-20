// Package compaction removes metadata that has outlived its purpose:
// finished operational records after a short retention, and audit records
// only after a long, mandatory one. Records needed for recovery,
// ownership, retrieval or proof of deletion are never touched.
package compaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// Retention classes decide each rule's cutoff.
const (
	ClassOperational = "operational"
	ClassIdempotency = "idempotency"
	ClassAudit       = "audit"
)

// Mandatory floors: no configuration may keep records for less.
const (
	MinAuditRetention       = 90 * 24 * time.Hour
	MinOperationalRetention = 24 * time.Hour
	MinIdempotencyTTL       = 24 * time.Hour
)

// Rule is one kind of record compaction may remove.
type Rule struct {
	Name        string
	Class       string
	Description string
}

// Rules lists every rule, in run order. Not listed, and so never
// compacted: assets (tombstones prove deletion), deletion operations,
// processing jobs, quarantine records, legal holds, retention policies,
// upload sessions and live tasks.
var Rules = []Rule{
	{"reconciliation_tasks", ClassOperational, "reconciliation tasks that succeeded"},
	{"upload_parts", ClassOperational, "part rows of verified uploads, no longer needed for recovery"},
	{"upload_idempotency_keys", ClassIdempotency, "idempotency keys of old upload sessions (the key is cleared; the session is kept)"},
	{"download_tokens", ClassOperational, "expired download links (only their hashes were stored)"},
	{"orphan_candidates", ClassOperational, "resolved orphan candidates (their outcome stays in the lifecycle events)"},
	{"inventory_scans", ClassOperational, "finished scans, except each prefix's latest completed scan and any scan an open candidate refers to"},
	{"compaction_runs", ClassOperational, "compaction run records"},
	{"outbox_events", ClassOperational, "published events, except each aggregate's latest (its sequence continues from it) and any an open dead letter refers to"},
	{"event_inbox", ClassOperational, "consumer records of completed or skipped events (the queue keeps no message this long)"},
	{"reconciliation_findings", ClassAudit, "reconciliation findings"},
	{"delivery_audit_events", ClassAudit, "delivery audit events"},
	{"processing_events", ClassAudit, "processing audit events"},
	{"lifecycle_events", ClassAudit, "lifecycle audit events"},
	{"dead_letters", ClassAudit, "closed dead letters, with the operator's resolution"},
	{"event_discrepancies", ClassAudit, "event reconciliation findings"},
}

type Config struct {
	// BatchSize rows per batch, at most MaxBatches batches per rule per run.
	BatchSize  int
	MaxBatches int
	// BatchPause waits between batches so other traffic gets the table.
	BatchPause time.Duration

	AuditRetention       time.Duration
	OperationalRetention time.Duration
	IdempotencyTTL       time.Duration

	// Only limits a run to these rules; empty runs every rule.
	Only []string
	// DryRun counts what would be removed and writes nothing.
	DryRun bool
}

type Service struct {
	store   interfaces.Compactor
	cfg     Config
	log     *slog.Logger
	metrics *metrics.Registry
	now     func() time.Time
}

func NewService(store interfaces.Compactor, cfg Config, log *slog.Logger, registry *metrics.Registry) (*Service, error) {
	if store == nil || log == nil {
		return nil, errors.New("compaction dependencies are required")
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > 10000 || cfg.MaxBatches <= 0 || cfg.BatchPause < 0 {
		return nil, errors.New("compaction batch size (at most 10000) and batch count must be positive")
	}
	if cfg.AuditRetention < MinAuditRetention {
		return nil, fmt.Errorf("audit retention must be at least %s", MinAuditRetention)
	}
	if cfg.OperationalRetention < MinOperationalRetention {
		return nil, fmt.Errorf("operational retention must be at least %s", MinOperationalRetention)
	}
	if cfg.IdempotencyTTL < MinIdempotencyTTL {
		return nil, fmt.Errorf("idempotency key lifetime must be at least %s", MinIdempotencyTTL)
	}
	for _, name := range cfg.Only {
		if !slices.ContainsFunc(Rules, func(r Rule) bool { return r.Name == name }) {
			return nil, fmt.Errorf("unknown compaction rule %q", name)
		}
	}
	return &Service{store: store, cfg: cfg, log: log, metrics: registry, now: func() time.Time { return time.Now().UTC() }}, nil
}

// RuleResult is what one rule did, or would do on a dry run.
type RuleResult struct {
	Rule     string
	Cutoff   time.Time
	Rows     int64
	Batches  int
	Duration time.Duration
	// Status is COMPLETED, FAILED, CANCELLED or DRY_RUN. A run that hit
	// MaxBatches is COMPLETED; the next run continues.
	Status string
	Error  string
}

// RunOnce runs every selected rule. A failing rule is reported but does
// not stop the others.
func (s *Service) RunOnce(ctx context.Context) ([]RuleResult, error) {
	var results []RuleResult
	var errs []error
	for _, rule := range Rules {
		if len(s.cfg.Only) > 0 && !slices.Contains(s.cfg.Only, rule.Name) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		result, err := s.runRule(ctx, rule)
		results = append(results, result)
		errs = append(errs, err)
	}
	return results, errors.Join(errs...)
}

func (s *Service) cutoff(rule Rule) time.Time {
	retention := s.cfg.OperationalRetention
	switch rule.Class {
	case ClassAudit:
		retention = s.cfg.AuditRetention
	case ClassIdempotency:
		retention = s.cfg.IdempotencyTTL
	}
	return s.now().Add(-retention)
}

func (s *Service) runRule(ctx context.Context, rule Rule) (RuleResult, error) {
	started := s.now()
	result := RuleResult{Rule: rule.Name, Cutoff: s.cutoff(rule)}

	if s.cfg.DryRun {
		rows, err := s.store.Count(ctx, rule.Name, result.Cutoff, s.cfg.BatchSize*s.cfg.MaxBatches)
		result.Rows, result.Status, result.Duration = rows, "DRY_RUN", s.now().Sub(started)
		if err != nil {
			result.Status, result.Error = "FAILED", retry.Sanitize(err)
		}
		return result, err
	}

	var runErr error
	result.Status = "COMPLETED"
	for result.Batches < s.cfg.MaxBatches {
		if ctx.Err() != nil {
			result.Status = "CANCELLED"
			break
		}
		rows, err := s.store.CompactBatch(ctx, rule.Name, result.Cutoff, s.cfg.BatchSize)
		if err != nil {
			if ctx.Err() != nil {
				result.Status = "CANCELLED"
				break
			}
			result.Status, result.Error, runErr = "FAILED", retry.Sanitize(err), fmt.Errorf("compact %s: %w", rule.Name, err)
			break
		}
		result.Batches++
		result.Rows += rows
		if rows < int64(s.cfg.BatchSize) {
			break // nothing more is old enough
		}
		if s.cfg.BatchPause > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(s.cfg.BatchPause):
			}
		}
	}
	result.Duration = s.now().Sub(started)

	s.metrics.Add("compaction_rows_"+rule.Name, result.Rows)
	s.metrics.Set("compaction_last_ms_"+rule.Name, result.Duration.Milliseconds())
	if result.Status == "FAILED" {
		s.metrics.Add("compaction_failures", 1)
	}

	// The run record outlives a cancelled context.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	recordErr := s.store.RecordRun(recordCtx, model.CompactionRun{
		ID: newID(), Rule: rule.Name, Status: result.Status, RowsAffected: result.Rows, Batches: result.Batches,
		Cutoff: result.Cutoff, StartedAt: started, FinishedAt: s.now(), LastError: result.Error,
	})
	if recordErr != nil {
		s.log.Warn("record compaction run", "rule", rule.Name, "error", recordErr)
	}

	log := s.log.With("rule", rule.Name, "rows", result.Rows, "batches", result.Batches,
		"cutoff", result.Cutoff, "duration_ms", result.Duration.Milliseconds(), "status", result.Status)
	if runErr != nil {
		log.Error("compaction rule failed; the next run continues", "error", result.Error)
	} else if result.Rows > 0 {
		log.Info("compaction rule ran")
	}
	return result, runErr
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
