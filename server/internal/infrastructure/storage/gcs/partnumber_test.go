package gcs

import "testing"

// A part object's number past int32 is not one of ours: it is skipped, not
// wrapped round to a part number that is.
func TestPartNumberRefusesANumberPastInt32(t *testing.T) {
	for _, s := range []string{"4294967297", "99999999999", "x1", ""} {
		if n, ok := partNumber(s); ok {
			t.Fatalf("%q was read as part %d", s, n)
		}
	}
	for s, want := range map[string]int32{"1": 1, "00042": 42, "10000": 10000} {
		if n, ok := partNumber(s); !ok || n != want {
			t.Fatalf("%q decoded as %d %v", s, n, ok)
		}
	}
}
