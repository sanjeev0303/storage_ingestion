package rangeread

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCheckContentRange(t *testing.T) {
	tests := []struct {
		header string
		ok     bool
	}{
		{"bytes 10-19/100", true},
		{"bytes 10-19/*", true},
		{"", false},                // no header: the provider ignored Range
		{"bytes 0-99/100", false},  // the whole object
		{"bytes 10-20/100", false}, // one byte too many
		{"bytes 11-19/100", false},
		{"items 10-19/100", false},
		{"bytes ten-19/100", false},
	}
	for _, tt := range tests {
		err := CheckContentRange(tt.header, 10, 19)
		if tt.ok && err != nil {
			t.Errorf("%q: %v", tt.header, err)
		}
		if !tt.ok && !errors.Is(err, ErrRangeNotHonoured) {
			t.Errorf("%q: got %v, want ErrRangeNotHonoured", tt.header, err)
		}
	}
}

func TestExactPassesTheRightLength(t *testing.T) {
	got, err := io.ReadAll(Exact(io.NopCloser(strings.NewReader("0123456789")), 10))
	if err != nil || string(got) != "0123456789" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestExactFailsAShortBody(t *testing.T) {
	_, err := io.ReadAll(Exact(io.NopCloser(strings.NewReader("01234")), 10))
	if !errors.Is(err, ErrRangeNotHonoured) {
		t.Fatalf("got %v, want ErrRangeNotHonoured", err)
	}
}

func TestExactFailsALongBody(t *testing.T) {
	// A provider that sends the whole object: the first bytes are right, the
	// rest belongs to a neighbour and must never be passed on as success.
	got, err := io.ReadAll(Exact(io.NopCloser(bytes.NewReader([]byte("0123456789neighbour"))), 10))
	if !errors.Is(err, ErrRangeNotHonoured) {
		t.Fatalf("got %v, want ErrRangeNotHonoured", err)
	}
	if len(got) > 10 {
		t.Fatalf("%d bytes passed on; at most the 10 asked for", len(got))
	}
}
