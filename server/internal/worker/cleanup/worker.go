// Package cleanup runs lifecycle cycles: retention evaluation, deletions,
// inventory scans, orphan sweeps and missing-object checks.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	assetservice "github.com/sanjeev0303/object-storage-platform/internal/service/asset"
	cleanupservice "github.com/sanjeev0303/object-storage-platform/internal/service/cleanup"
)

// Phases selects what a cycle runs.
type Phases struct {
	Retention bool
	Deletions bool
	Scan      bool
	Sweep     bool
	Missing   bool
}

// AllPhases runs everything.
var AllPhases = Phases{Retention: true, Deletions: true, Scan: true, Sweep: true, Missing: true}

// ParsePhases reads a comma-separated list such as "retention,deletions".
func ParsePhases(list string) (Phases, error) {
	if strings.TrimSpace(list) == "" || list == "all" {
		return AllPhases, nil
	}
	var p Phases
	for _, name := range strings.Split(list, ",") {
		switch strings.TrimSpace(name) {
		case "retention":
			p.Retention = true
		case "deletions":
			p.Deletions = true
		case "scan":
			p.Scan = true
		case "sweep":
			p.Sweep = true
		case "missing":
			p.Missing = true
		default:
			return Phases{}, fmt.Errorf("unknown phase %q (use retention, deletions, scan, sweep, missing or all)", name)
		}
	}
	return p, nil
}

// Report is what one cycle did.
type Report struct {
	Lifecycle assetservice.CycleStats
	Cleanup   cleanupservice.CycleStats
	Duration  time.Duration
}

// RunCycle runs the selected phases once. A failing phase is reported but
// does not stop the others.
func RunCycle(ctx context.Context, lifecycle *assetservice.Service, cleanup *cleanupservice.Service, phases Phases) (Report, error) {
	started := time.Now()
	var report Report
	var errs []error

	steps := []struct {
		on  bool
		run func() error
	}{
		{phases.Retention, func() error { return lifecycle.EvaluateRetention(ctx, &report.Lifecycle) }},
		{phases.Deletions, func() error { return lifecycle.ProcessDeletions(ctx, &report.Lifecycle) }},
		{phases.Scan, func() error { return cleanup.Scan(ctx, &report.Cleanup) }},
		{phases.Sweep, func() error { return cleanup.Sweep(ctx, &report.Cleanup) }},
		{phases.Missing, func() error { return cleanup.CheckMissing(ctx, &report.Cleanup) }},
	}
	for _, step := range steps {
		if !step.on || ctx.Err() != nil {
			continue
		}
		errs = append(errs, step.run())
	}
	report.Duration = time.Since(started)
	return report, errors.Join(errs...)
}

// Run repeats cycles every interval until ctx is cancelled. A failed cycle
// is logged and retried on the next tick.
func Run(ctx context.Context, lifecycle *assetservice.Service, cleanup *cleanupservice.Service, phases Phases, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		report, err := RunCycle(ctx, lifecycle, cleanup, phases)
		if err != nil && ctx.Err() == nil {
			log.Error("lifecycle cycle failed", "error", err)
		}
		l, c := report.Lifecycle, report.Cleanup
		log.Info("lifecycle cycle",
			"evaluated", l.Evaluated, "expired", l.Expired, "held_from_expiry", l.HeldFromExpiry,
			"claimed", l.Claimed, "completed", l.Completed, "protected", l.Protected, "retried", l.Retried,
			"failed", l.Failed, "cancelled", l.Cancelled, "lease_lost", l.LeaseLost,
			"pages_scanned", c.PagesScanned, "objects_seen", c.ObjectsSeen, "marked", c.Marked,
			"orphans_deleted", c.Deleted, "cleared", c.Cleared, "kept", c.Kept, "sweep_failed", c.SweepFailed,
			"missing", c.Missing, "duration_ms", report.Duration.Milliseconds(),
		)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
