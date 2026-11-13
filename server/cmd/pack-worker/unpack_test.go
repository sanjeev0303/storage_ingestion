package main

import (
	"strings"
	"testing"
)

func TestParseUnpack(t *testing.T) {
	good := map[string]unpackOptions{
		"-tenant t1 -all":              {tenant: "t1"},
		"-tenant t1 -pack p1":          {tenant: "t1", pack: "p1"},
		"-tenant t1 -pack p1 -dry-run": {tenant: "t1", pack: "p1", dryRun: true},
		"-dry-run -tenant t1 -all":     {tenant: "t1", dryRun: true},
	}
	for args, want := range good {
		got, err := parseUnpack(strings.Fields(args))
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v; want %+v", args, got, err, want)
		}
	}
	for _, args := range []string{
		"-all",                     // no tenant
		"-tenant t1",               // neither -pack nor -all
		"-tenant t1 -pack p1 -all", // both
		"-tenant t1 -all extra",    // stray argument
		"-tenant t1 -all -unknown", // unknown flag
	} {
		if _, err := parseUnpack(strings.Fields(args)); err == nil {
			t.Errorf("%q: accepted", args)
		}
	}
}
