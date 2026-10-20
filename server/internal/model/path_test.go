package model

import (
	"errors"
	"strings"
	"testing"
)

var testPathPolicy = PathPolicy{MaxBytes: 1024, MaxSegmentBytes: 255, MaxDepth: 8}

func TestNormalizePathAcceptsAndNormalizes(t *testing.T) {
	cases := map[string]string{
		"a.txt":               "a.txt",
		"images/2024/cat.jpg": "images/2024/cat.jpg",
		"a//b/./c.txt":        "a/b/c.txt",
		"./a/b.txt":           "a/b.txt",
		"données/été.csv":     "données/été.csv",
		"a b/c-d_e (1).txt":   "a b/c-d_e (1).txt",
		"Reports/Q1.PDF":      "Reports/Q1.PDF",
	}
	for raw, want := range cases {
		got, key, err := NormalizePath(raw, testPathPolicy)
		if err != nil || got != want || key != strings.ToLower(want) {
			t.Errorf("%q: got %q %q %v, want %q", raw, got, key, err, want)
		}
	}
}

func TestNormalizePathRejectsUnsafePaths(t *testing.T) {
	unsafe := []string{
		"",
		"/etc/passwd",
		"../secret.txt",
		"a/../../b.txt",
		"a/..",
		`C:\Windows\x.dll`,
		"C:/data/x.txt",
		`a\b.txt`,
		"a/b\x00.txt",
		"a/\x07bell.txt",
		"dir/",
		"./",
		"\xff\xfe.txt",
		" leading.txt",
		"trailing.txt ",
		"a/b<c>.txt",
		"what?.txt",
		"_system/x.txt",
		"_Ingestion/x.txt",
		".well-known/x.txt",
		strings.Repeat("a", 256) + ".txt",
		strings.Repeat("d/", 8) + "deep.txt",
		strings.Repeat("x", 1025),
		"invoice\u202Etxt.exe", // a bidi override disguises the extension
	}
	for _, raw := range unsafe {
		got, _, err := NormalizePath(raw, testPathPolicy)
		if err == nil {
			t.Errorf("%q must be rejected, got %q", raw, got)
		}
		if err != nil && !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%q: %v is not ErrInvalidPath", raw, err)
		}
	}
}

func TestNormalizedFormsShareOneKey(t *testing.T) {
	// Different spellings of one path, names differing only in case, and
	// the composed and decomposed forms of one character collide on the
	// key, so the duplicate is caught.
	_, a, _ := NormalizePath("Photos//2024/./IMG.jpg", testPathPolicy)
	_, b, _ := NormalizePath("photos/2024/img.JPG", testPathPolicy)
	if a != b {
		t.Fatalf("keys differ: %q %q", a, b)
	}

	composed, composedKey, err := NormalizePath("caf\u00e9/menu.txt", testPathPolicy)       // é as one code point
	decomposed, decomposedKey, err2 := NormalizePath("cafe\u0301/menu.txt", testPathPolicy) // e + combining accent
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if composed != decomposed || composedKey != decomposedKey || composed != "caf\u00e9/menu.txt" {
		t.Fatalf("NFC: %q / %q, keys %q / %q", composed, decomposed, composedKey, decomposedKey)
	}
	if _, upper, _ := NormalizePath("CAF\u00c9/MENU.TXT", testPathPolicy); upper != composedKey {
		t.Fatalf("case of accented letters: %q vs %q", upper, composedKey)
	}
}

func TestBaseNameAndMetadataKeys(t *testing.T) {
	if BaseName("a/b/c.txt") != "c.txt" || BaseName("c.txt") != "c.txt" {
		t.Fatal("base name")
	}
	for k, want := range map[string]bool{"camera": true, "a.b_c-1": true, "": false, "with space": false, "é": false,
		strings.Repeat("k", 64): true, strings.Repeat("k", 65): false} {
		if ValidMetadataKey(k) != want {
			t.Errorf("%q: want %v", k, want)
		}
	}
}
