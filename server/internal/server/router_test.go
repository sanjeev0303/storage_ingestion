package server

import "testing"

func TestIngestionEntryCallsHaveTheirOwnLimit(t *testing.T) {
	cases := map[string]string{
		"POST /api/v1/ingestion/jobs/{id}/claims":                           "ingestion_entry",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/upload-url":    "ingestion_entry",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/parts/urls":    "ingestion_entry",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/parts/confirm": "ingestion_entry",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/heartbeat":     "ingestion_entry",
		"POST /api/v1/ingestion/jobs/{id}/entries/{position}/complete":      "ingestion_entry",
		"POST /api/v1/ingestion/jobs":                                       "job_create",
		"PUT /api/v1/ingestion/jobs/{id}/manifest":                          "manifest",
		"GET /api/v1/ingestion/jobs/{id}":                                   "listing",
		"GET /api/v1/ingestion/jobs/{id}/entries/{position}":                "listing",
		"POST /api/v1/ingestion/jobs/{id}/cancel":                           "upload_finish",
		"POST /api/v1/ingestion/jobs/{id}/finalize":                         "upload_finish",
	}
	for pattern, want := range cases {
		if got := ingestionOperation(pattern); got != want {
			t.Errorf("%s counts against %s, want %s", pattern, got, want)
		}
	}
}
