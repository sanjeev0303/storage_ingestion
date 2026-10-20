package config

import (
	"fmt"
	"strconv"
	"time"
)

// Catalog bounds catalog listings.
type Catalog struct {
	DefaultPageSize int
	MaxPageSize     int
	// QueryTimeout cancels a listing statement; SlowQuery logs and counts
	// listings slower than it.
	QueryTimeout       time.Duration
	SlowQuery          time.Duration
	MaxMetadataFilters int
}

// LoadCatalog reads CATALOG_* and refuses values that would let a listing
// run unbounded.
func LoadCatalog() (Catalog, error) {
	var c Catalog
	var err error
	for _, n := range []struct {
		key, fallback string
		target        *int
	}{
		{"CATALOG_DEFAULT_PAGE_SIZE", "100", &c.DefaultPageSize},
		{"CATALOG_MAX_PAGE_SIZE", "1000", &c.MaxPageSize},
		{"CATALOG_MAX_METADATA_FILTERS", "4", &c.MaxMetadataFilters},
	} {
		if *n.target, err = strconv.Atoi(getEnv(n.key, n.fallback)); err != nil || *n.target <= 0 {
			return Catalog{}, fmt.Errorf("%s must be a positive integer", n.key)
		}
	}
	for _, d := range []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"CATALOG_QUERY_TIMEOUT", "5s", &c.QueryTimeout},
		{"CATALOG_SLOW_QUERY", "500ms", &c.SlowQuery},
	} {
		if *d.target, err = time.ParseDuration(getEnv(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return Catalog{}, fmt.Errorf("%s must be a positive duration", d.key)
		}
	}
	switch {
	case c.MaxPageSize > 10000:
		return Catalog{}, fmt.Errorf("CATALOG_MAX_PAGE_SIZE must be at most 10000")
	case c.DefaultPageSize > c.MaxPageSize:
		return Catalog{}, fmt.Errorf("CATALOG_DEFAULT_PAGE_SIZE must not exceed CATALOG_MAX_PAGE_SIZE")
	case c.QueryTimeout > time.Minute:
		return Catalog{}, fmt.Errorf("CATALOG_QUERY_TIMEOUT must be at most 1m")
	case c.MaxMetadataFilters > 16:
		return Catalog{}, fmt.Errorf("CATALOG_MAX_METADATA_FILTERS must be at most 16")
	}
	return c, nil
}
