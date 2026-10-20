package retry

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDelayGrowsExponentiallyWithinJitterAndCap(t *testing.T) {
	p := Policy{BaseDelay: time.Second, MaxDelay: 10 * time.Second, MaxAttempts: 5}

	cases := []struct {
		attempt int
		ceiling time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 10 * time.Second}, // capped
		{50, 10 * time.Second},
	}
	for _, c := range cases {
		for i := 0; i < 100; i++ {
			d := p.Delay(c.attempt)
			if d < c.ceiling/2 || d > c.ceiling {
				t.Fatalf("attempt %d: delay %s outside [%s, %s]", c.attempt, d, c.ceiling/2, c.ceiling)
			}
		}
	}
}

func TestExhausted(t *testing.T) {
	p := Policy{MaxAttempts: 3}
	if p.Exhausted(2) || !p.Exhausted(3) {
		t.Fatal("exhaustion must start at MaxAttempts")
	}
}

func TestPermanentSurvivesWrapping(t *testing.T) {
	base := errors.New("AccessDenied")
	wrapped := fmt.Errorf("begin upload: %w", Permanent(base))

	if !IsPermanent(wrapped) || !errors.Is(wrapped, base) {
		t.Fatal("a wrapped permanent error must stay permanent and keep its cause")
	}
	if IsPermanent(errors.New("timeout")) || Permanent(nil) != nil {
		t.Fatal("ordinary errors are transient and Permanent(nil) is nil")
	}
}

func TestSanitizeCutsSignedURLs(t *testing.T) {
	err := errors.New("get object: https://bucket.s3.amazonaws.com/a/b?X-Amz-Credential=AKIA123&X-Amz-Signature=deadbeef failed;\n" +
		"retry with X-Amz-Signature=cafe and Signature=abc")

	got := Sanitize(err)
	for _, secret := range []string{"AKIA123", "deadbeef", "cafe", "abc"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitized text still holds %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "https://bucket.s3.amazonaws.com/a/b?[redacted]") || strings.Contains(got, "\n") {
		t.Fatalf("expected the URL without its query, on one line: %s", got)
	}
}
