package ratelimit

import (
	"testing"
	"time"
)

func TestLimiterAllowsTheRuleThenRefuses(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New()
	l.now = func() time.Time { return now }
	rule := Rule{Requests: 3, Window: time.Minute}

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k", rule); !ok {
			t.Fatalf("request %d refused", i+1)
		}
	}
	ok, wait := l.Allow("k", rule)
	if ok || wait <= 0 || wait > time.Minute {
		t.Fatalf("fourth request: %v, wait %s", ok, wait)
	}
	if ok, _ := l.Allow("other", rule); !ok {
		t.Fatal("keys are independent")
	}

	// Half a window into the next window, half the old one still counts:
	// 1.5 of 3, so two more fit (1.5, then 2.5) but not a third.
	now = now.Add(90 * time.Second)
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k", rule); !ok {
			t.Fatalf("request %d refused after the window slid", i+1)
		}
	}
	if ok, _ := l.Allow("k", rule); ok {
		t.Fatal("the previous window must still count")
	}
	// Two windows later everything is forgotten.
	now = now.Add(3 * time.Minute)
	if ok, _ := l.Allow("k", rule); !ok {
		t.Fatal("refused after two idle windows")
	}
}

func TestLimiterCountsFailuresSeparately(t *testing.T) {
	l := New()
	rule := Rule{Requests: 2, Window: time.Minute}
	if over, _ := l.Exceeded("login", rule); over {
		t.Fatal("nothing counted yet")
	}
	l.Add("login", rule)
	l.Add("login", rule)
	if over, wait := l.Exceeded("login", rule); !over || wait <= 0 {
		t.Fatal("two failures reach the limit")
	}
	l.Reset("login")
	if over, _ := l.Exceeded("login", rule); over {
		t.Fatal("reset forgets the key")
	}
}
