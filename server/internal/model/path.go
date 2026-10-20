package model

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// PathPolicy limits logical paths: the paths of catalog objects and of
// bulk-ingestion entries, which become catalog objects.
type PathPolicy struct {
	MaxBytes        int
	MaxSegmentBytes int
	MaxDepth        int
}

// DefaultPathPolicy is the catalog's default.
var DefaultPathPolicy = PathPolicy{MaxBytes: 1024, MaxSegmentBytes: 255, MaxDepth: 64}

// reservedTopLevel are first segments the platform keeps for itself.
var reservedTopLevel = []string{"_system", "_ingestion", ".well-known"}

// ErrInvalidPath refuses a logical path.
var ErrInvalidPath = errors.New("invalid path")

// NormalizePath turns a client-supplied logical path into its normal form,
// and returns the key duplicates are detected on. The path is untrusted:
// it is never used as a filesystem path or a storage key, only stored as
// metadata.
//
// Normal form: Unicode NFC, "/"-separated, no empty, "." or trailing
// segments. Rejected outright rather than resolved: absolute and
// drive-letter paths, "..", backslashes, NUL, control and bidirectional
// override characters,
// invalid UTF-8, characters Windows forbids in names, segments with
// leading or trailing spaces, and reserved top-level namespaces.
//
// The key is the lower-cased normal form (NFC again after lower-casing),
// so names differing only in case conflict, as they would on a
// case-insensitive filesystem, and so do the composed and decomposed
// spellings of one name ("é" as one code point or as "e" plus an accent).
// The normal form keeps the client's case for display.
func NormalizePath(raw string, p PathPolicy) (normalized, key string, err error) {
	if raw == "" {
		return "", "", fmt.Errorf("%w: empty", ErrInvalidPath)
	}
	if !utf8.ValidString(raw) {
		return "", "", fmt.Errorf("%w: not valid UTF-8", ErrInvalidPath)
	}
	raw = norm.NFC.String(raw)
	if len(raw) > p.MaxBytes {
		return "", "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidPath, p.MaxBytes)
	}
	if strings.HasPrefix(raw, "/") {
		return "", "", fmt.Errorf("%w: absolute paths are not allowed", ErrInvalidPath)
	}
	if strings.HasSuffix(raw, "/") {
		return "", "", fmt.Errorf("%w: names a directory", ErrInvalidPath)
	}
	if len(raw) >= 2 && raw[1] == ':' && isASCIILetter(raw[0]) {
		return "", "", fmt.Errorf("%w: drive-letter paths are not allowed", ErrInvalidPath)
	}

	var segments []string
	for _, segment := range strings.Split(raw, "/") {
		switch segment {
		case "", ".":
			continue // a//b and a/./b are a/b
		case "..":
			return "", "", fmt.Errorf("%w: parent-directory segments are not allowed", ErrInvalidPath)
		}
		if err := checkSegment(segment, p.MaxSegmentBytes); err != nil {
			return "", "", err
		}
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		return "", "", fmt.Errorf("%w: no file name", ErrInvalidPath)
	}
	if len(segments) > p.MaxDepth {
		return "", "", fmt.Errorf("%w: deeper than %d segments", ErrInvalidPath, p.MaxDepth)
	}
	for _, reserved := range reservedTopLevel {
		if strings.EqualFold(segments[0], reserved) {
			return "", "", fmt.Errorf("%w: %q is reserved", ErrInvalidPath, reserved)
		}
	}

	normalized = strings.Join(segments, "/")
	return normalized, PathKey(normalized), nil
}

// PathKey is the duplicate-detection key of an already normalized path,
// and the form path prefixes are matched in.
func PathKey(normalized string) string {
	return norm.NFC.String(strings.ToLower(normalized))
}

func checkSegment(segment string, maxBytes int) error {
	if len(segment) > maxBytes {
		return fmt.Errorf("%w: a segment is longer than %d bytes", ErrInvalidPath, maxBytes)
	}
	if strings.TrimSpace(segment) != segment {
		return fmt.Errorf("%w: a segment starts or ends with whitespace", ErrInvalidPath)
	}
	for _, r := range segment {
		if r == 0 || unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return fmt.Errorf("%w: control characters are not allowed", ErrInvalidPath)
		}
		if strings.ContainsRune(`\<>:"|?*`, r) {
			return fmt.Errorf("%w: %q is not allowed in a name", ErrInvalidPath, r)
		}
	}
	return nil
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

// BaseName is the last segment of a normalized path.
func BaseName(normalized string) string {
	if i := strings.LastIndexByte(normalized, '/'); i >= 0 {
		return normalized[i+1:]
	}
	return normalized
}

// ValidMetadataKey reports whether k is a user metadata key: 1-64
// characters of letters, digits, '.', '_' and '-'. Keys are
// case-sensitive.
func ValidMetadataKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
