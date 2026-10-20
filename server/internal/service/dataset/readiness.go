package dataset

import (
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// A readiness assessment judges one dataset version against the profile
// the caller chose, dimension by dimension. It reports what the profile's
// checks and the platform's records show, and says plainly what was not
// assessed. There is no overall score: no number here measures data
// quality, legal compliance, bias, or how well a model will train.

// Dimension statuses.
const (
	DimensionPass        = "PASS"
	DimensionWarn        = "WARN"
	DimensionFail        = "FAIL"
	DimensionNotAssessed = "NOT_ASSESSED"
)

type dimensionResult struct {
	Dimension string `json:"dimension"`
	Status    string `json:"status"`
	// Checks are the profile's rules this dimension was judged by.
	Checks []string `json:"checks"`
	// Coverage: FULL, SAMPLED, PARTIAL (some members not assessed) or
	// NONE.
	Coverage string   `json:"coverage"`
	Notes    []string `json:"notes,omitempty"`
}

type readinessReport struct {
	Profile    string            `json:"profile"`
	Dimensions []dimensionResult `json:"dimensions"`
	Disclaimer string            `json:"disclaimer"`
}

const readinessDisclaimer = "Assessed against the selected profile from catalog records, processing evidence and the checks it lists. " +
	"It does not judge semantic quality, label correctness, legal compliance, bias or suitability for a particular model."

// assess turns check results into dimension statuses: FAIL when an ERROR
// check failed, WARN when a WARNING check failed or a check passed only
// where it was assessed (a sample, or members it could not read),
// NOT_ASSESSED when no check of the dimension could judge the version,
// PASS otherwise. INFO checks never change a status; they add notes.
func assess(results []checkResult, job model.ValidationJob, v model.DatasetVersion, agg model.VersionAggregates) *readinessReport {
	report := &readinessReport{Profile: fmt.Sprintf("%s@%d", job.ProfileID, job.ProfileVersion), Disclaimer: readinessDisclaimer}
	for _, dim := range Dimensions {
		if dim == DimReproducibility {
			report.Dimensions = append(report.Dimensions, reproducibility(job, v, agg))
			continue
		}
		d := dimensionResult{Dimension: dim, Status: DimensionNotAssessed, Coverage: "NONE"}
		assessed, partial, sampledOnly := 0, false, false
		for _, r := range results {
			if r.Dimension != dim {
				continue
			}
			d.Checks = append(d.Checks, r.RuleID)
			if r.Status == CheckNotAssessed {
				d.Notes = append(d.Notes, fmt.Sprintf("%s not assessed%s", r.RuleID, suffix(r.Note)))
				continue
			}
			assessed++
			if r.Status == CheckPartial {
				partial = true
				if job.Mode == model.ModeSampled && r.Type == "content_checksum" {
					sampledOnly = true
				}
				d.Notes = append(d.Notes, fmt.Sprintf("%s assessed %d, not assessed %d%s", r.RuleID, r.Checked, r.NotAssessed, suffix(r.Note)))
			}
			if r.Failed > 0 {
				d.Notes = append(d.Notes, fmt.Sprintf("%s failed for %d (%s)", r.RuleID, r.Failed, r.Severity))
				switch r.Severity {
				case string(model.SeverityError):
					d.Status = DimensionFail
				case string(model.SeverityWarning):
					if d.Status != DimensionFail {
						d.Status = DimensionWarn
					}
				}
			}
		}
		if assessed == 0 {
			if len(d.Checks) == 0 {
				d.Notes = append(d.Notes, "the profile has no check for this dimension")
			}
			d.Status = DimensionNotAssessed
			report.Dimensions = append(report.Dimensions, d)
			continue
		}
		switch {
		case sampledOnly:
			d.Coverage = "SAMPLED"
		case partial:
			d.Coverage = "PARTIAL"
		default:
			d.Coverage = "FULL"
		}
		if d.Status == DimensionNotAssessed {
			d.Status = DimensionPass
			if partial {
				// Passing where assessed is not passing everywhere.
				d.Status = DimensionWarn
			}
		}
		report.Dimensions = append(report.Dimensions, d)
	}
	return report
}

func suffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

// reproducibility is judged from the version itself: a finalized
// snapshot pins every member to an exact object version and has a
// manifest hash, and the job recorded the profile version. It warns when
// some of those versions can no longer be read, so the snapshot cannot
// be reproduced byte for byte.
func reproducibility(job model.ValidationJob, v model.DatasetVersion, agg model.VersionAggregates) dimensionResult {
	d := dimensionResult{Dimension: DimReproducibility, Status: DimensionPass, Coverage: "FULL", Checks: []string{}}
	d.Notes = append(d.Notes, fmt.Sprintf("version %d (%s) pins %d members to exact object versions; manifest sha256 %s; profile %s@%d",
		v.Version, v.Status, v.MemberCount, v.ManifestSHA256, job.ProfileID, job.ProfileVersion))
	if v.Status != "FINALIZED" || v.ManifestSHA256 == "" {
		d.Status = DimensionFail
		d.Notes = append(d.Notes, "the version is not a finalized snapshot with a manifest hash")
		return d
	}
	var unavailable int64
	for _, c := range agg.AssetStatuses {
		if c.Value != string(model.AssetStatusAvailable) {
			unavailable += c.Count
		}
	}
	if unavailable > 0 {
		d.Status = DimensionWarn
		d.Notes = append(d.Notes, fmt.Sprintf("%d members' versions are no longer available", unavailable))
	}
	return d
}
