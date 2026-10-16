package processing

import (
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func TestContentValidator(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + "rest of image")
	pdf := []byte("%PDF-1.7 rest")
	zip := []byte("PK\x03\x04 rest")
	unknown := []byte{0x00, 0x01, 0x02, 0x03}

	cases := []struct {
		name     string
		head     []byte
		declared string
		allowed  []string
		reason   string
	}{
		{"matching image", png, "image/png", nil, ""},
		{"same family, other format", png, "image/jpeg", nil, ""},
		{"unrecognised bytes are not a contradiction", unknown, "video/mp4", nil, ""},
		{"pdf claiming to be video", pdf, "video/mp4", nil, model.RejectContentMismatch},
		{"zip claiming to be an image", zip, "image/png", nil, model.RejectContentMismatch},
		{"html bytes are refused", []byte("<html><body>"), "text/plain", nil, model.RejectContentType},
		{"svg declared is refused", png, "image/svg+xml", nil, model.RejectContentType},
		{"parameters are ignored", pdf, "application/pdf; charset=binary", nil, ""},
		{"allowed family", png, "image/png", []string{"image/*"}, ""},
		{"not in the allow list", pdf, "application/pdf", []string{"image/*", "video/mp4"}, model.RejectContentType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, reason := ContentValidator{Allowed: c.allowed}.Validate(c.head, c.declared)
			if reason != c.reason {
				t.Fatalf("got reason %q, want %q", reason, c.reason)
			}
		})
	}
}
