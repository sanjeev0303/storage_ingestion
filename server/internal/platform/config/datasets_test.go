package config

import "testing"

func TestLoadDatasetsDefaults(t *testing.T) {
	c, err := LoadDatasets()
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxPageSize != 1000 || c.ImportBatch != 500 || c.MaxInlineManifestBytes != 8<<20 || c.Lease <= c.ValidationBudget {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestLoadDatasetsRefusesUnboundedLimits(t *testing.T) {
	for key, value := range map[string]string{
		"DATASET_MAX_PAGE_SIZE":             "0",
		"DATASET_DEFAULT_PAGE_SIZE":         "5000",
		"DATASET_MAX_INLINE_MANIFEST_BYTES": "2147483648",
		"DATASET_IMPORT_BATCH":              "50000",
		"DATASET_LINEAGE_MAX_DEPTH":         "500",
		"DATASET_VALIDATION_BUDGET":         "10m",
		"DATASET_LEASE":                     "1s",
		"DATASET_OBJECT_TIMEOUT":            "soon",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := LoadDatasets(); err == nil {
				t.Fatalf("%s=%s was accepted", key, value)
			}
		})
	}
}
