package processing

import (
	"mime"
	"net/http"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// sniffBytes is how much of the object content sniffing reads.
const sniffBytes = 512

// ContentValidator decides by the file's own bytes, using the standard
// library's content sniffing (the WHATWG algorithm). The declared type and
// file name are untrusted and only checked against what the bytes show.
type ContentValidator struct {
	// Allowed lists the permitted declared types (e.g. "video/mp4") or
	// families (e.g. "image/*"). Empty allows any type.
	Allowed []string
}

var _ interfaces.FileValidator = ContentValidator{}

// alwaysRejected are types that are dangerous to store and serve back,
// whatever the client declared.
var alwaysRejected = map[string]bool{
	"text/html":     true,
	"text/xml":      true,
	"image/svg+xml": true,
}

func (v ContentValidator) Validate(head []byte, declaredContentType string) (string, string) {
	detected := baseType(http.DetectContentType(head))
	declared := baseType(declaredContentType)

	if alwaysRejected[detected] || alwaysRejected[declared] {
		return detected, model.RejectContentType
	}
	if !v.allows(declared) {
		return detected, model.RejectContentType
	}

	// Sniffing knows a limited set of formats. "application/octet-stream"
	// and "text/plain" mean "nothing recognised", which neither confirms
	// nor contradicts the declared type. A recognised format of another
	// family (a zip or PDF claiming to be a video) is rejected. Within one
	// family the formats are compared loosely: a .docx is a zip, for one.
	recognised := detected != "application/octet-stream" && detected != "text/plain"
	if recognised && family(detected) != family(declared) {
		return detected, model.RejectContentMismatch
	}

	return detected, ""
}

func (v ContentValidator) allows(contentType string) bool {
	if len(v.Allowed) == 0 {
		return true
	}
	for _, allowed := range v.Allowed {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == contentType || (strings.HasSuffix(allowed, "/*") && family(contentType)+"/*" == allowed) {
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

func family(contentType string) string {
	family, _, _ := strings.Cut(contentType, "/")
	return family
}
