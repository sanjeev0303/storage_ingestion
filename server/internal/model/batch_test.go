package model

import "testing"

func TestDatasetStatusOf(t *testing.T) {
	done := DatasetSummary{Jobs: 1, Valid: 10, Published: 10}
	optionalFailed := DatasetSummary{Jobs: 1, Valid: 10, Published: 9, Failed: 1}
	requiredFailed := DatasetSummary{Jobs: 1, Valid: 10, Published: 9, Failed: 1, RequiredFailed: 1}
	nothing := DatasetSummary{Jobs: 1, Valid: 10, Failed: 10, RequiredFailed: 10}
	running := DatasetSummary{Jobs: 2, ActiveJobs: 1, Valid: 10, Published: 10}
	invalidLine := DatasetSummary{Jobs: 1, Valid: 10, Published: 10, Invalid: 1, RequiredFailed: 1}

	cases := []struct {
		name    string
		policy  CompletionPolicy
		summary DatasetSummary
		want    DatasetStatus
	}{
		{"no jobs", CompleteAll, DatasetSummary{}, DatasetOpen},
		{"a job still running", CompletePartial, running, DatasetIngesting},
		{"all published", CompleteAll, done, DatasetReady},
		{"all published, required policy", CompleteRequired, done, DatasetReady},
		{"all published, partial policy", CompletePartial, done, DatasetReady},
		{"an optional entry failed", CompleteAll, optionalFailed, DatasetIncomplete},
		{"an optional entry failed, required policy", CompleteRequired, optionalFailed, DatasetReady},
		{"an optional entry failed, partial policy", CompletePartial, optionalFailed, DatasetPartiallyReady},
		{"a required entry failed", CompleteRequired, requiredFailed, DatasetIncomplete},
		{"a required entry failed, partial policy", CompletePartial, requiredFailed, DatasetPartiallyReady},
		{"an invalid manifest line", CompleteAll, invalidLine, DatasetIncomplete},
		{"nothing published", CompletePartial, nothing, DatasetIncomplete},
		{"nothing published, required policy", CompleteRequired, nothing, DatasetIncomplete},
	}
	for _, c := range cases {
		if got := DatasetStatusOf(c.policy, c.summary); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
