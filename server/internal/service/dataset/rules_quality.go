package dataset

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Rules that read more than the aggregates: the version's column profile,
// a metadata key's values, and members' subjects. Their load runs once,
// when the dataset-level checks run, and returns the check itself.

func loadFailed(err error) func(model.VersionAggregates) ([]issue, string) {
	return func(model.VersionAggregates) ([]issue, string) { return nil, notAssessed(err.Error()) }
}

func pendingLoad(model.VersionAggregates) ([]issue, string) {
	return nil, notAssessed("the rule's data was not loaded")
}

// compileColumnDrift: the version's columns against its parent's (or the
// version before it), past the dataset's thresholds unless the rule sets
// its own.
func compileColumnDrift(c *check, config json.RawMessage) error {
	var cfg struct {
		MaxPSI        float64 `json:"max_psi"`
		MaxNullChange float64 `json:"max_null_change"`
		MaxRowChange  float64 `json:"max_row_change"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if cfg.MaxPSI < 0 || cfg.MaxNullChange < 0 || cfg.MaxNullChange > 1 || cfg.MaxRowChange < 0 {
		return fmt.Errorf("thresholds must not be negative, and max_null_change at most 1")
	}
	c.dataset = pendingLoad
	c.load = func(ctx context.Context, s *Service, tenantID, versionID string) func(model.VersionAggregates) ([]issue, string) {
		q := s.deps.Quality
		if q == nil {
			return loadFailed(ErrQualityOff)
		}
		p, found, err := q.Profile(ctx, tenantID, versionID)
		if err != nil {
			return loadFailed(err)
		}
		if !found {
			return loadFailed(fmt.Errorf("the version was not profiled (its dataset does not profile snapshots)"))
		}
		if p.State != model.ProfileDone {
			return loadFailed(fmt.Errorf("the column profile is %s; validate again when it is done", strings.ToLower(p.State)))
		}
		if p.BaselineVersionID == "" {
			return func(model.VersionAggregates) ([]issue, string) {
				return nil, "no earlier version to compare with"
			}
		}
		set, err := q.Settings(ctx, tenantID, p.DatasetID)
		if err != nil {
			return loadFailed(err)
		}
		lim := limitsOf(set)
		if cfg.MaxPSI > 0 {
			lim.MaxPSI = cfg.MaxPSI
		}
		if cfg.MaxNullChange > 0 {
			lim.MaxNullChange = cfg.MaxNullChange
		}
		if cfg.MaxRowChange > 0 {
			lim.MaxRowChange = cfg.MaxRowChange
		}
		base, found, err := q.Profile(ctx, tenantID, p.BaselineVersionID)
		if err != nil {
			return loadFailed(err)
		}
		if !found || base.State != model.ProfileDone {
			return loadFailed(fmt.Errorf("the baseline version %s has no finished profile", p.BaselineVersionID))
		}
		// Compared again rather than read from the profile: the rule may
		// set its own thresholds.
		report := compareProfiles(p.BaselineVersionID, versionID, base.Tables, p.Tables, lim)
		return func(model.VersionAggregates) ([]issue, string) {
			return driftIssues(report), driftNote(p, report)
		}
	}
	return nil
}

func driftNote(p model.VersionProfile, r model.DriftReport) string {
	note := fmt.Sprintf("compared %d tables with version %s", len(r.Tables), r.BaselineVersionID)
	if len(r.NotCompared) > 0 {
		note += "; not compared: " + strings.Join(r.NotCompared, ", ")
	}
	if len(p.Skipped) > 0 {
		note += fmt.Sprintf("; %d files were not profiled (see the profile)", len(p.Skipped))
	}
	return note
}

func driftIssues(r model.DriftReport) []issue {
	var out []issue
	for _, t := range r.Tables {
		for _, b := range t.Breached {
			is := issue{Code: "TABLE_DRIFT", Key: t.Table + "/" + b, Message: "a table changed past the drift threshold: " + b}
			switch b {
			case "rows":
				is.Expected = fmt.Sprintf("rows within %.0f%% of %d", r.Thresholds.MaxRowChange*100, t.RowsBefore)
				is.Observed = fmt.Sprintf("%s: %d rows (%+.1f%%)", t.Table, t.RowsAfter, t.RowChange*100)
			default:
				is.Expected = "the baseline's columns"
				is.Observed = fmt.Sprintf("%s: removed %s", t.Table, strings.Join(t.ColumnsRemoved, ", "))
			}
			is.Remediation = "check the new data's source; if the change is meant, record it in the release notes"
			out = append(out, is)
		}
		for _, c := range t.Columns {
			for _, b := range c.Breached {
				is := issue{Code: "COLUMN_DRIFT", Key: t.Table + "/" + c.Name + "/" + b, Message: "a column changed past the drift threshold: " + b}
				switch b {
				case "distribution":
					is.Expected = fmt.Sprintf("PSI <= %.2f", r.Thresholds.MaxPSI)
					is.Observed = fmt.Sprintf("%s.%s PSI %.3f", t.Table, c.Name, *c.PSI)
				case "nulls":
					is.Expected = fmt.Sprintf("null rate within %.0f points of %.1f%%", r.Thresholds.MaxNullChange*100, c.NullRateBefore*100)
					is.Observed = fmt.Sprintf("%s.%s null rate %.1f%%", t.Table, c.Name, c.NullRateAfter*100)
				case "kind":
					is.Expected, is.Observed = c.KindBefore, fmt.Sprintf("%s.%s is %s", t.Table, c.Name, c.KindAfter)
				}
				is.Remediation = "compare the column's values with the baseline's; a reprocessing, unit or schema change upstream is the usual cause"
				out = append(out, is)
			}
		}
	}
	return out
}

// compileUniformMetadata: every member carries the same value of a
// metadata key, such as a processing baseline, a scanner model or a
// software version, so one snapshot never mixes them.
func compileUniformMetadata(c *check, config json.RawMessage) error {
	var cfg struct {
		Key             string `json:"key"`
		RequirePresence bool   `json:"require_presence"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	cfg.Key = strings.TrimSpace(cfg.Key)
	if cfg.Key == "" || len(cfg.Key) > 128 || hasControl(cfg.Key) {
		return fmt.Errorf("key: a metadata key of 1 to 128 bytes")
	}
	c.dataset = pendingLoad
	c.load = func(ctx context.Context, s *Service, tenantID, versionID string) func(model.VersionAggregates) ([]issue, string) {
		if s.deps.Quality == nil {
			return loadFailed(ErrQualityOff)
		}
		values, distinct, missing, err := s.deps.Quality.MetadataValues(ctx, tenantID, versionID, cfg.Key, 10)
		if err != nil {
			return loadFailed(err)
		}
		return func(model.VersionAggregates) ([]issue, string) {
			var out []issue
			if distinct > 1 {
				var seen []string
				for _, v := range values {
					seen = append(seen, fmt.Sprintf("%s (%d)", v.Value, v.Count))
				}
				out = append(out, issue{Code: "MIXED_METADATA", Key: cfg.Key,
					Message:     fmt.Sprintf("members carry %d different values of %s", distinct, cfg.Key),
					Expected:    "one value",
					Observed:    strings.Join(seen, ", "),
					Remediation: "keep each value in its own branch or dataset, or reprocess the older members so all match"})
			}
			if missing > 0 && cfg.RequirePresence {
				out = append(out, issue{Code: "METADATA_MISSING", Key: cfg.Key + "/missing",
					Message: fmt.Sprintf("%d members have no %s", missing, cfg.Key), Expected: "a value on every member",
					Observed: fmt.Sprintf("%d without", missing)})
			}
			note := fmt.Sprintf("%d distinct values", distinct)
			if missing > 0 && !cfg.RequirePresence {
				note += fmt.Sprintf("; %d members without %s were not judged", missing, cfg.Key)
			}
			return out, note
		}
	}
	return nil
}

// compileSubjectLeakage: no subject (a patient, a user, a scene) appears
// in more than one split of the version. The subject is a metadata key's
// value, or the first group of a pattern on the path; members without one
// are not assessed.
func compileSubjectLeakage(c *check, config json.RawMessage) error {
	var cfg struct {
		Key     string `json:"key"`
		Pattern string `json:"pattern"`
	}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	if (cfg.Key == "") == (cfg.Pattern == "") {
		return fmt.Errorf("key or pattern: exactly one")
	}
	var re *regexp.Regexp
	if cfg.Pattern != "" {
		var err error
		if re, err = subjectRegexp(cfg.Pattern); err != nil {
			return err
		}
	}
	c.dataset = pendingLoad
	c.load = func(ctx context.Context, s *Service, tenantID, versionID string) func(model.VersionAggregates) ([]issue, string) {
		if s.deps.Quality == nil {
			return loadFailed(ErrQualityOff)
		}
		splits := map[string]map[string]bool{}
		first := map[string]string{}
		var with, without int64
		err := s.deps.Quality.ScanMembers(ctx, tenantID, versionID, cfg.Key, func(m model.QualityMember) error {
			sub := subjectOf(m, re)
			if sub == "" || m.Split == "" {
				without++
				return nil
			}
			with++
			if splits[sub] == nil {
				splits[sub] = map[string]bool{}
				first[sub] = m.Path
			}
			splits[sub][m.Split] = true
			return nil
		})
		if err != nil {
			return loadFailed(err)
		}
		var leaked []string
		for sub, in := range splits {
			if len(in) > 1 {
				leaked = append(leaked, sub)
			}
		}
		slices.Sort(leaked)
		return func(model.VersionAggregates) ([]issue, string) {
			var out []issue
			for i, sub := range leaked {
				if i == 50 {
					out = append(out, issue{Code: "SUBJECT_LEAKAGE_MORE", Key: "more", Message: fmt.Sprintf("%d more subjects are not listed", len(leaked)-50)})
					break
				}
				var names []string
				for sp := range splits[sub] {
					names = append(names, sp)
				}
				slices.Sort(names)
				out = append(out, issue{Code: "SUBJECT_LEAKAGE", Key: sub, Message: "one subject's files are in more than one split",
					Expected: "each subject in one split", Observed: fmt.Sprintf("subject %s in %s (e.g. %s)", sub, strings.Join(names, ", "), first[sub]),
					Remediation: "move all of the subject's files into one split"})
			}
			note := fmt.Sprintf("compared %d subjects over %d members", len(splits), with)
			if without > 0 {
				note += fmt.Sprintf("; %d members without a subject or split were not assessed", without)
			}
			return out, note
		}
	}
	return nil
}
