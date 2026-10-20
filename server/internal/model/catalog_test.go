package model

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateUserMetadata(t *testing.T) {
	if err := ValidateUserMetadata(map[string]string{"camera": "a7", "shot.date": "2026-10-19", "lens_mm": "35"}, DefaultMetadataLimits); err != nil {
		t.Fatal(err)
	}
	if err := ValidateUserMetadata(nil, DefaultMetadataLimits); err != nil {
		t.Fatal(err)
	}
	many := map[string]string{}
	for i := 0; i < 65; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	for name, m := range map[string]map[string]string{
		"reserved name":    {"Checksum_SHA256": "x"},
		"reserved prefix":  {"System.owner": "x"},
		"bad key":          {"a b": "x"},
		"empty key":        {"": "x"},
		"control in value": {"note": "a\nb"},
		"bidi in value":    {"note": "a\u202eb"},
		"bad UTF-8":        {"note": "\xff"},
		"value too long":   {"note": strings.Repeat("x", 1025)},
		"too many keys":    many,
	} {
		if err := ValidateUserMetadata(m, DefaultMetadataLimits); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	big := map[string]string{}
	for i := 0; i < 10; i++ {
		big[strings.Repeat("k", i+1)] = strings.Repeat("v", 1000)
	}
	if err := ValidateUserMetadata(big, DefaultMetadataLimits); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("total size: %v", err)
	}
}
