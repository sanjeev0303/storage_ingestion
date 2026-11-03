package processing

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"compress/lzw"
	"compress/zlib"
	"context"
	"encoding/ascii85"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// AWS's documented example key: the shape of a key, not a credential.
const exampleKey = "aws_access_key_id = AKIAIOSFODNN7EXAMPLE\n"

func modelPolicy() model.ContentPolicy { return model.ContentPolicy{} }

// findingDetails maps each finding's code to its detail.
func findingDetails(t *testing.T, name string, data []byte) map[string]string {
	t.Helper()
	result, err := testInspector.Validate(context.Background(), sampleOf(name, data), model.ContentPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range result.Findings {
		out[f.Code] = f.Detail
	}
	return out
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngChunk(kind string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, kind...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(kind), data...)))
}

// withChunk puts a chunk right after IHDR (8 signature bytes, 25 of IHDR).
func withChunk(p []byte, chunk []byte) []byte {
	out := append([]byte{}, p[:33]...)
	out = append(out, chunk...)
	return append(out, p[33:]...)
}

func TestImageChecks(t *testing.T) {
	p := testPNG(t)
	expectCodes(t, "plain.png", p)
	expectCodes(t, "comment.png", withChunk(p, pngChunk("tEXt", []byte("Comment\x00hello"))))

	// CVE-2022-44268: ImageMagick reads a "profile" text chunk as a path.
	expectCodes(t, "evil.png", withChunk(p, pngChunk("tEXt", []byte("profile\x00/etc/passwd"))), "image.png_profile_text")
	expectCodes(t, "private.png", withChunk(p, pngChunk("prVt", []byte("x"))), "image.png_unknown_chunk")
	expectCodes(t, "polyglot.png", append(append([]byte{}, p...), "PK\x03\x04payload"...), "image.trailing_data")

	bad := append([]byte{}, p...)
	bad[30]++ // inside IHDR's data: its CRC no longer matches
	expectCodes(t, "crc.png", bad, "format.corrupt_png")
	expectCodes(t, "cut.png", p[:len(p)-6], "format.corrupt_png")

	var j bytes.Buffer
	if err := jpeg.Encode(&j, image.NewRGBA(image.Rect(0, 0, 32, 32)), nil); err != nil {
		t.Fatal(err)
	}
	jp := j.Bytes()
	expectCodes(t, "photo.jpg", jp)
	expectCodes(t, "polyglot.jpg", append(append([]byte{}, jp...), "<?php system($_GET[0]); ?>"...), "image.trailing_data")
	expectCodes(t, "cut.jpg", jp[:len(jp)-40], "image.truncated_jpeg")

	// MPF keeps a second picture after the first one's end: not a rider.
	mpf := append([]byte{0xFF, 0xD8, 0xFF, 0xE2, 0x00, 0x06}, "MPF\x00"...)
	mpf = append(mpf, jp[2:]...)
	expectCodes(t, "camera.jpg", append(mpf, jp...))
}

func TestSecretsInsideArchives(t *testing.T) {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	for name, body := range map[string]string{"app/readme.bin": exampleKey, "app/config/.env": exampleKey} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	result := findingDetails(t, "release.tar", tb.Bytes())
	if !strings.Contains(result["secret.aws_access_key"], "app/config/.env") {
		t.Fatalf("tar: %v", result)
	}

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write([]byte(`{"note": "x"}` + "\n" + exampleKey))
	gw.Close()
	expectCodes(t, "settings.json.gz", gz.Bytes(), "secret.aws_access_key")

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	f, _ := zw.Create("deploy/terraform.tfvars")
	f.Write([]byte(exampleKey))
	zw.Close()
	expectCodes(t, "bundle.zip", zb.Bytes(), "secret.aws_access_key")

	// Binary files inside are not text, whatever they hold.
	var plain bytes.Buffer
	zw = zip.NewWriter(&plain)
	f, _ = zw.Create("weights.bin")
	f.Write([]byte(exampleKey))
	zw.Close()
	expectCodes(t, "model.zip", plain.Bytes())
}

func TestArchivedSecretsShareOneBudget(t *testing.T) {
	inspector := testInspector
	inspector.SecretScanBytes = 64 << 10
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	filler := bytes.Repeat([]byte("x"), 48<<10)
	for i, body := range [][]byte{filler, filler, []byte(exampleKey)} {
		tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("f%d.txt", i), Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write(body)
	}
	tw.Close()
	result, err := inspector.Validate(t.Context(), sampleOf("many.tar", tb.Bytes()), modelPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Findings) != 0 || !strings.Contains(strings.Join(result.Notes, ";"), "later files were not scanned") {
		t.Fatalf("findings %v notes %v", result.Findings, result.Notes)
	}
}

func pdfWith(dict string, stream []byte) []byte {
	return []byte(fmt.Sprintf("%%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n4 0 obj\n<< /Length %d %s >>\nstream\n%s\nendstream\nendobj\n%%%%EOF\n",
		len(stream), dict, stream))
}

func TestPDFFiltersAreDecoded(t *testing.T) {
	hidden := []byte("<< /S /JavaScript /JS (app.alert(1)) >>")

	expectCodes(t, "hex.pdf", pdfWith("/Filter /ASCIIHexDecode", []byte(hex.EncodeToString(hidden)+">")), "active_content.pdf_javascript")
	expectCodes(t, "hex-short.pdf", pdfWith("/Filter /AHx", []byte(hex.EncodeToString(hidden)+">")), "active_content.pdf_javascript")

	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(hidden)
	zw.Close()
	a85 := make([]byte, ascii85.MaxEncodedLen(z.Len()))
	a85 = a85[:ascii85.Encode(a85, z.Bytes())]
	expectCodes(t, "chain.pdf", pdfWith("/Filter [/ASCII85Decode /FlateDecode]", append(a85, "~>"...)), "active_content.pdf_javascript")

	var l bytes.Buffer
	lw := lzw.NewWriter(&l, lzw.MSB, 8)
	lw.Write(hidden)
	lw.Close()
	expectCodes(t, "lzw.pdf", pdfWith("/Filter /LZWDecode /DecodeParms << /EarlyChange 0 >>", l.Bytes()), "active_content.pdf_javascript")

	rl := append([]byte{byte(len(hidden) - 1)}, hidden...)
	expectCodes(t, "rl.pdf", pdfWith("/Filter /RunLengthDecode", append(rl, 128)), "active_content.pdf_javascript")

	// Pixels are left alone; nothing to find, nothing broken.
	expectCodes(t, "photo.pdf", pdfWith("/Filter /DCTDecode", []byte("\xff\xd8\xff\xe0 not decoded")))
}

func TestLZWDecode(t *testing.T) {
	// The PDF specification's example (EarlyChange 1).
	out, err := lzwDecode(bytes.NewReader([]byte{0x80, 0x0B, 0x60, 0x50, 0x22, 0x0C, 0x0C, 0x85, 0x01}), 1, 1<<20)
	if err != nil || string(out) != "-----A---B" {
		t.Fatalf("%q %v", out, err)
	}
	// Go's writer switches width without the early change: long enough to
	// pass 9, 10, 11 and 12 bit codes and a table reset.
	rng := rand.New(rand.NewSource(1))
	data := make([]byte, 200000)
	for i := range data {
		data[i] = "abcdefgh"[rng.Intn(8)]
	}
	var buf bytes.Buffer
	w := lzw.NewWriter(&buf, lzw.MSB, 8)
	w.Write(data)
	w.Close()
	out, err = lzwDecode(bytes.NewReader(buf.Bytes()), 0, 1<<30)
	if err != nil || !bytes.Equal(out, data) {
		t.Fatalf("%d of %d bytes, %v", len(out), len(data), err)
	}
}

func TestRunLengthAndHexDecode(t *testing.T) {
	out, _ := runLengthDecode(bytes.NewReader([]byte{2, 'a', 'b', 'c', 254, 'z', 128, 'x'}), 100)
	if string(out) != "abczzz" {
		t.Fatalf("run length: %q", out)
	}
	out, _ = asciiHexDecode(strings.NewReader("41 42\n4>ignored"), 100)
	if string(out) != "AB@" {
		t.Fatalf("hex: %q", out)
	}
}
