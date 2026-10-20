package config

import (
	"testing"
	"time"
)

func TestCatalogDefaultsAndLimits(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil || c.DefaultPageSize != 100 || c.MaxPageSize != 1000 || c.QueryTimeout != 5*time.Second {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for key, value := range map[string]string{
		"CATALOG_MAX_PAGE_SIZE":        "50000",
		"CATALOG_DEFAULT_PAGE_SIZE":    "5000",
		"CATALOG_QUERY_TIMEOUT":        "10m",
		"CATALOG_SLOW_QUERY":           "-1s",
		"CATALOG_MAX_METADATA_FILTERS": "0",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := LoadCatalog(); err == nil {
				t.Fatalf("%s=%s must be refused", key, value)
			}
		})
	}
}
