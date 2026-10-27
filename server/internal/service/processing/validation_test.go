package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

func sample(head []byte, declared, name string) interfaces.ContentSample {
	return interfaces.ContentSample{Head: head, DeclaredType: declared, FileName: name, SizeBytes: int64(len(head))}
}

func TestTypeValidator(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n" + "rest of image")
	pdf := []byte("%PDF-1.7 rest")
	zipHead := []byte("PK\x03\x04 rest")
	unknown := []byte{0x00, 0x01, 0x02, 0x03}
	allowAll := model.ContentPolicy{UnknownTypeAction: "allow"}

	cases := []struct {
		name   string
		sample interfaces.ContentSample
		policy model.ContentPolicy
		reason string
	}{
		{"matching image", sample(png, "image/png", "a.png"), allowAll, ""},
		{"same family, other format", sample(png, "image/jpeg", "a.jpg"), allowAll, ""},
		{"unrecognised bytes are not a contradiction", sample(unknown, "video/mp4", "a.mp4"), allowAll, ""},
		{"unknown type rejected by policy", sample(unknown, "video/mp4", "a.mp4"), model.ContentPolicy{UnknownTypeAction: "reject"}, model.RejectUnknownType},
		{"pdf claiming to be video", sample(pdf, "video/mp4", "a.mp4"), allowAll, model.RejectContentMismatch},
		{"zip claiming to be an image", sample(zipHead, "image/png", "a.png"), allowAll, model.RejectContentMismatch},
		{"text claiming to be a pdf", sample([]byte("Sample test PDF content for upload\r\n"), "application/pdf", "report.pdf"), allowAll, model.RejectContentMismatch},
		{"text claiming to be an image", sample([]byte("not really a photo"), "image/jpeg", "a.jpg"), allowAll, model.RejectContentMismatch},
		{"text declared as json is fine", sample([]byte(`{"a": 1}`), "application/json", "a.json"), allowAll, ""},
		{"text declared as csv is fine", sample([]byte("a,b\n1,2\n"), "text/csv", "a.csv"), allowAll, ""},
		{"html bytes are refused", sample([]byte("<html><body>"), "text/plain", "a.txt"), allowAll, model.RejectContentType},
		{"svg declared is refused", sample(png, "image/svg+xml", "a.png"), allowAll, model.RejectContentType},
		{"pe executable", sample([]byte("MZ\x90\x00"), "application/pdf", "a.pdf"), allowAll, model.RejectContentType},
		// Two-byte signatures that plain text hits by chance.
		{"text starting with MZ", sample([]byte("MZA,Mazda,1999\n"), "text/csv", "cars.csv"), allowAll, ""},
		{"text starting with BM", sample([]byte("BMW,320i,2004\n"), "text/plain", "cars.txt"), allowAll, ""},
		{"text with LP at offset 34", sample([]byte(strings.Repeat("x", 34)+"LP and more text\n"), "text/plain", "a.txt"), allowAll, ""},
		{"bmp image", sample([]byte("BM\x36\x00\x10\x00\x00\x00"), "text/plain", "a.txt"), allowAll, model.RejectContentMismatch},
		{"elf executable", sample([]byte("\x7fELF\x02"), "application/octet-stream", "a.bin"), allowAll, model.RejectContentType},
		{"mach-o executable", sample([]byte{0xcf, 0xfa, 0xed, 0xfe, 0}, "application/octet-stream", "a"), allowAll, model.RejectContentType},
		{"shell script", sample([]byte("#!/bin/sh\nrm -rf /"), "text/plain", "a.txt"), allowAll, model.RejectContentType},
		{"exe extension", sample(pdf, "application/pdf", "invoice.pdf.exe"), allowAll, model.RejectContentType},
		{"extension contradicts declared media", sample(unknown, "image/png", "clip.mp4"), allowAll, model.RejectExtensionMismatch},
		{"csv as excel is fine", sample([]byte("a,b\n1,2\n"), "application/vnd.ms-excel", "data.csv"), allowAll, ""},
		{"parameters are ignored", sample(pdf, "application/pdf; charset=binary", "a.pdf"), allowAll, ""},
		{"allowed family", sample(png, "image/png", "a.png"), model.ContentPolicy{AllowedTypes: []string{"image/*"}}, ""},
		{"not in the allow list", sample(pdf, "application/pdf", "a.pdf"), model.ContentPolicy{AllowedTypes: []string{"image/*", "video/mp4"}}, model.RejectContentType},
		{"tenant denied type", sample(pdf, "application/pdf", "a.pdf"), model.ContentPolicy{DeniedTypes: []string{"application/pdf"}}, model.RejectContentType},
		{"allow list cannot allow html", sample([]byte("<html>"), "text/html", "a.html"), model.ContentPolicy{AllowedTypes: []string{"text/html"}}, model.RejectContentType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := TypeValidator{}.Validate(context.Background(), c.sample, c.policy)
			if err != nil {
				t.Fatal(err)
			}
			if result.RejectReason != c.reason {
				t.Fatalf("got reason %q (%s), want %q", result.RejectReason, result.Detail, c.reason)
			}
		})
	}
}

func TestBasicValidator(t *testing.T) {
	policy := model.ContentPolicy{MaxSizeBytes: 10}
	cases := []struct {
		name   string
		size   int64
		file   string
		reason string
	}{
		{"fine", 5, "report.pdf", ""},
		{"empty", 0, "report.pdf", model.RejectEmptyFile},
		{"too large", 11, "report.pdf", model.RejectFileTooLarge},
		{"path separator", 5, "../etc/passwd", model.RejectUnsafeFileName},
		{"windows path", 5, `C:\x.pdf`, model.RejectUnsafeFileName},
		{"control character", 5, "a\nb.pdf", model.RejectUnsafeFileName},
		{"bidi override", 5, "invoice\u202efdp.exe", model.RejectUnsafeFileName},
		{"dot dot", 5, "..", model.RejectUnsafeFileName},
		{"too long", 5, strings.Repeat("a", 256), model.RejectUnsafeFileName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := interfaces.ContentSample{SizeBytes: c.size, FileName: c.file}
			result, _ := BasicValidator{}.Validate(context.Background(), s, policy)
			if result.RejectReason != c.reason {
				t.Fatalf("got %q, want %q", result.RejectReason, c.reason)
			}
		})
	}
}

type zipEntry struct {
	name    string
	body    []byte
	symlink bool
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		header := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.symlink {
			header.SetMode(fs.ModeSymlink | 0o777)
		}
		f, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(e.body)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestArchiveValidator(t *testing.T) {
	limits := ArchiveLimits{MaxArchiveBytes: 1 << 20, MaxEntries: 3, MaxUncompressedBytes: 1 << 20, MaxRatio: 100}
	small := []byte("hello archive")

	cases := []struct {
		name    string
		entries []zipEntry
		raw     []byte
		reason  string
	}{
		{"plain archive", []zipEntry{{name: "docs/a.txt", body: small}, {name: "b.txt", body: small}}, nil, ""},
		{"parent traversal", []zipEntry{{name: "../../evil.sh", body: small}}, nil, model.RejectUnsafeArchive},
		{"absolute path", []zipEntry{{name: "/etc/cron.d/x", body: small}}, nil, model.RejectUnsafeArchive},
		{"backslash path", []zipEntry{{name: `..\evil.bat`, body: small}}, nil, model.RejectUnsafeArchive},
		{"symlink", []zipEntry{{name: "link", body: []byte("/etc/passwd"), symlink: true}}, nil, model.RejectUnsafeArchive},
		{"too many entries", []zipEntry{{name: "1"}, {name: "2"}, {name: "3"}, {name: "4"}}, nil, model.RejectUnsafeArchive},
		{"zip bomb by ratio", []zipEntry{{name: "zeros", body: make([]byte, 512<<10)}}, nil, model.RejectUnsafeArchive},
		{"expands past the limit", []zipEntry{{name: "a", body: bytes.Repeat([]byte("ab"), 300<<10)}, {name: "b", body: bytes.Repeat([]byte("cd"), 300<<10)}}, nil, model.RejectUnsafeArchive},
		{"corrupt archive", nil, []byte("PK\x03\x04 this is not a real zip"), model.RejectUnsafeArchive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := c.raw
			if data == nil {
				data = buildZip(t, c.entries)
			}
			s := interfaces.ContentSample{
				Head: data[:min(len(data), sniffBytes)], DeclaredType: "application/zip", FileName: "a.zip", SizeBytes: int64(len(data)),
				Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
			}
			result, err := ArchiveValidator{Limits: limits, TempDir: t.TempDir()}.Validate(context.Background(), s, model.ContentPolicy{})
			if err != nil {
				t.Fatal(err)
			}
			if result.RejectReason != c.reason {
				t.Fatalf("got %q (%s), want %q", result.RejectReason, result.Detail, c.reason)
			}
		})
	}
}

func TestArchiveValidatorIgnoresOtherTypesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	v := ArchiveValidator{Limits: ArchiveLimits{MaxArchiveBytes: 1 << 20, MaxEntries: 10, MaxUncompressedBytes: 1 << 20, MaxRatio: 100}, TempDir: dir}

	opened := false
	s := sample([]byte("%PDF-1.7"), "application/pdf", "a.pdf")
	s.Open = func(context.Context) (io.ReadCloser, error) { opened = true; return nil, io.EOF }
	if result, err := v.Validate(context.Background(), s, model.ContentPolicy{}); err != nil || result.RejectReason != "" || opened {
		t.Fatalf("a non-archive must pass without being read: %+v %v", result, err)
	}

	data := buildZip(t, []zipEntry{{name: "a.txt", body: []byte("x")}})
	z := interfaces.ContentSample{Head: data, SizeBytes: int64(len(data)), FileName: "a.zip",
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
	if _, err := v.Validate(context.Background(), z, model.ContentPolicy{}); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("the archive spool must be removed, found %d files", len(left))
	}
}

func TestMergePolicyOnlyNarrows(t *testing.T) {
	platform := model.ContentPolicy{AllowedTypes: []string{"image/*", "application/pdf"}, MaxSizeBytes: 100, UnknownTypeAction: "allow"}

	merged, err := mergePolicy(platform, model.ContentPolicy{
		AllowedTypes: []string{"image/png", "video/mp4"}, DeniedTypes: []string{"image/gif"},
		MaxSizeBytes: 1000, UnknownTypeAction: "reject",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(merged.AllowedTypes, ",") != "image/png" || merged.MaxSizeBytes != 100 ||
		merged.UnknownTypeAction != "reject" || strings.Join(merged.DeniedTypes, ",") != "image/gif" {
		t.Fatalf("tenant policy must only narrow: %+v", merged)
	}

	if _, err := mergePolicy(platform, model.ContentPolicy{AllowedTypes: []string{"video/mp4"}}); err == nil {
		t.Fatal("a tenant allow list with nothing the platform allows must be an error")
	}
}
