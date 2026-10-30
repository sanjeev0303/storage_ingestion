package processing

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// sniffBytes is how much of the object the type checks read.
const sniffBytes = 512

// UnknownType is what detection reports when the bytes match no format.
const UnknownType = "application/octet-stream"

// alwaysDenied are types that are dangerous to store and serve back. No
// tenant or configuration can allow them.
var alwaysDenied = map[string]bool{
	"text/html":                 true,
	"text/xml":                  true,
	"application/xml":           true,
	"image/svg+xml":             true,
	"application/xhtml+xml":     true,
	"application/javascript":    true,
	"text/javascript":           true,
	"application/x-msdownload":  true,
	"application/x-executable":  true,
	"application/x-mach-binary": true,
	"application/java-vm":       true,
	"text/x-shellscript":        true,
}

// extensionTypes maps file extensions to the type they imply. A name whose
// extension implies a denied type is refused, and so is one whose type
// family contradicts the declared type (photo.exe declared image/png).
var extensionTypes = map[string]string{
	".html": "text/html", ".htm": "text/html", ".xhtml": "application/xhtml+xml",
	".xml": "application/xml", ".svg": "image/svg+xml", ".svgz": "image/svg+xml",
	".js": "application/javascript", ".mjs": "application/javascript",
	".exe": "application/x-msdownload", ".dll": "application/x-msdownload", ".scr": "application/x-msdownload",
	".com": "application/x-msdownload", ".msi": "application/x-msdownload",
	".bat": "text/x-shellscript", ".cmd": "text/x-shellscript", ".ps1": "text/x-shellscript", ".sh": "text/x-shellscript",
	".class": "application/java-vm", ".jar": "application/java-archive",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg",
	".pdf": "application/pdf", ".zip": "application/zip",
	".txt": "text/plain", ".csv": "text/csv", ".json": "application/json",
}

// signedTypes are binary formats whose files start with a signature that
// DetectType recognises, so their bytes are never detected as plain text.
var signedTypes = map[string]bool{
	"application/pdf": true, "application/zip": true,
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true,
	"video/mp4": true, "video/webm": true, "audio/mpeg": true, "audio/wav": true,
}

// RunValidators runs the chain in order and stops at the first rejection.
// The result carries the detected type from the type check, and every
// finding the policy does not allow, with its action decided. A finding
// that rejects is a rejection.
func RunValidators(ctx context.Context, validators []interfaces.FileValidator, sample interfaces.ContentSample, policy model.ContentPolicy) (interfaces.ValidationResult, error) {
	var combined interfaces.ValidationResult
	for _, validator := range validators {
		result, err := validator.Validate(ctx, sample, policy)
		if err != nil {
			return combined, err
		}
		if result.DetectedType != "" {
			combined.DetectedType = result.DetectedType
			sample.DetectedType = result.DetectedType
		}
		combined.Notes = append(combined.Notes, result.Notes...)
		for _, f := range result.Findings {
			if f.Source == "" {
				f.Source = validator.Name()
			}
			f.Action = model.FindingAction(f.Code, f.Action, policy.FindingActions)
			if f.Action == model.FindingAllow {
				continue
			}
			combined.Findings = append(combined.Findings, f)
			if f.Action == model.FindingReject && result.RejectReason == "" {
				result.RejectReason = model.RejectContentCheck
				result.Detail = f.Code + ": " + f.Detail
			}
		}
		if result.RejectReason != "" {
			combined.RejectReason = result.RejectReason
			combined.Detail = validator.Name() + ": " + result.Detail
			return combined, nil
		}
	}
	return combined, nil
}

// BasicValidator checks size and the file name.
type BasicValidator struct{}

func (BasicValidator) Name() string { return "basic" }

func (BasicValidator) Validate(_ context.Context, sample interfaces.ContentSample, policy model.ContentPolicy) (interfaces.ValidationResult, error) {
	if sample.SizeBytes == 0 {
		return interfaces.ValidationResult{RejectReason: model.RejectEmptyFile, Detail: "empty files are not accepted"}, nil
	}
	if policy.MaxSizeBytes > 0 && sample.SizeBytes > policy.MaxSizeBytes {
		return interfaces.ValidationResult{RejectReason: model.RejectFileTooLarge, Detail: "larger than the content policy allows"}, nil
	}
	if !safeFileName(sample.FileName) {
		return interfaces.ValidationResult{RejectReason: model.RejectUnsafeFileName, Detail: "file name contains path or control characters"}, nil
	}
	return interfaces.ValidationResult{}, nil
}

// safeFileName refuses names that could act as a path or hide their real
// extension when shown or downloaded.
func safeFileName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return false
		}
		// Bidirectional overrides make "gpj.exe" display as "exe.jpg".
		if (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') {
			return false
		}
	}
	return true
}

// TypeValidator decides by the file's own bytes. The declared type and
// the file name are untrusted and only checked against what the bytes show.
type TypeValidator struct{}

func (TypeValidator) Name() string { return "type" }

func (TypeValidator) Validate(_ context.Context, sample interfaces.ContentSample, policy model.ContentPolicy) (interfaces.ValidationResult, error) {
	detected := DetectType(sample.Head)
	declared := baseType(sample.DeclaredType)
	byName := extensionTypes[strings.ToLower(filepath.Ext(sample.FileName))]

	reject := func(reason, detail string) (interfaces.ValidationResult, error) {
		return interfaces.ValidationResult{DetectedType: detected, RejectReason: reason, Detail: detail}, nil
	}

	for _, t := range []string{detected, declared, byName} {
		if t != "" && denied(t, policy.DeniedTypes) {
			return reject(model.RejectContentType, t+" is not accepted")
		}
	}
	if len(policy.AllowedTypes) > 0 && !matchesAny(declared, policy.AllowedTypes) {
		return reject(model.RejectContentType, declared+" is not in the allowed types")
	}

	if detected == UnknownType && policy.UnknownTypeAction == "reject" {
		return reject(model.RejectUnknownType, "content does not match a known format")
	}

	// Detection knows a limited set of formats. octet-stream and text/plain
	// mean "nothing specific recognised", which neither confirms nor
	// contradicts the declared type. A recognised format of another family
	// (a zip or PDF claiming to be a video) is rejected. Within one family
	// formats are compared loosely: a .docx is a zip, for one.
	recognised := detected != UnknownType && detected != "text/plain"
	if recognised && family(detected) != family(declared) {
		return reject(model.RejectContentMismatch, "content is "+detected+", declared "+declared)
	}
	// Plain text does contradict a format that always opens with a
	// signature the detector knows: a text file named report.pdf is not a
	// PDF, and would be served as one.
	if detected == "text/plain" && signedTypes[declared] {
		return reject(model.RejectContentMismatch, "content is text, declared "+declared)
	}
	// Only media families are compared for the extension: text and
	// application types are declared too inconsistently by browsers (a
	// .csv often arrives as application/vnd.ms-excel).
	if byName != "" && family(byName) != family(declared) && (isMedia(byName) || isMedia(declared)) {
		return reject(model.RejectExtensionMismatch, "extension implies "+byName+", declared "+declared)
	}

	return interfaces.ValidationResult{DetectedType: detected}, nil
}

// DetectType adds executable formats, which the standard library's
// sniffing (the WHATWG algorithm) does not report, to http.DetectContentType.
func DetectType(head []byte) string {
	detected := detectType(head)
	if weakSignatures[detected] && !hasBinaryBytes(head) {
		return "text/plain"
	}
	return detected
}

// weakSignatures are recognised by two bytes, which plain text matches by
// chance: "MZ" (Windows executables), "BM" (BMP) and "LP" at offset 34
// (EOT fonts). A real file of these formats has binary bytes in its
// header; text has none.
var weakSignatures = map[string]bool{
	"application/x-msdownload": true, "image/bmp": true, "application/vnd.ms-fontobject": true,
}

// hasBinaryBytes reports whether head has a byte that never appears in
// text: the WHATWG sniffing algorithm's binary data bytes.
func hasBinaryBytes(head []byte) bool {
	for _, b := range head {
		if b <= 0x08 || b == 0x0b || (b >= 0x0e && b <= 0x1a) || (b >= 0x1c && b <= 0x1f) {
			return true
		}
	}
	return false
}

func detectType(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("MZ")):
		return "application/x-msdownload"
	case bytes.HasPrefix(head, []byte("\x7fELF")):
		return "application/x-executable"
	case bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xce}), bytes.HasPrefix(head, []byte{0xfe, 0xed, 0xfa, 0xcf}),
		bytes.HasPrefix(head, []byte{0xce, 0xfa, 0xed, 0xfe}), bytes.HasPrefix(head, []byte{0xcf, 0xfa, 0xed, 0xfe}):
		return "application/x-mach-binary"
	case bytes.HasPrefix(head, []byte{0xca, 0xfe, 0xba, 0xbe}):
		// Mach-O universal binary or Java class file; both are code.
		return "application/java-vm"
	case bytes.HasPrefix(head, []byte("#!")):
		return "text/x-shellscript"
	}
	return baseType(http.DetectContentType(head))
}

func denied(contentType string, extra []string) bool {
	return alwaysDenied[contentType] || matchesAny(contentType, extra)
}

// matchesAny reports whether contentType is listed exactly or by family
// ("image/*").
func matchesAny(contentType string, list []string) bool {
	for _, entry := range list {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == contentType || (strings.HasSuffix(entry, "/*") && family(contentType)+"/*" == entry) {
			return true
		}
	}
	return false
}

// baseType drops parameters such as "; charset=utf-8".
func baseType(contentType string) string {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(contentType))
	}
	return parsed
}

func isMedia(contentType string) bool {
	switch family(contentType) {
	case "image", "video", "audio":
		return true
	}
	return false
}

func family(contentType string) string {
	family, _, _ := strings.Cut(contentType, "/")
	return family
}
