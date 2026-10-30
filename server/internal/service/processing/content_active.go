package processing

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
)

// PDF actions that run code or open other things when the file is viewed.
// Names are matched after decoding #xx escapes (/J#61vaScript), in the
// file and inside its compressed streams, where object streams hide
// dictionaries.
var pdfNames = map[string][2]string{
	"JavaScript":    {"active_content.pdf_javascript", "the PDF contains JavaScript"},
	"JS":            {"active_content.pdf_javascript", "the PDF contains JavaScript"},
	"Launch":        {"active_content.pdf_launch", "the PDF has a Launch action, which starts another program"},
	"EmbeddedFile":  {"active_content.pdf_embedded_file", "the PDF carries an embedded file"},
	"EmbeddedFiles": {"active_content.pdf_embedded_file", "the PDF carries an embedded file"},
	"RichMedia":     {"active_content.pdf_rich_media", "the PDF contains rich media (Flash or 3D)"},
	"XFA":           {"active_content.pdf_xfa", "the PDF contains an XFA form, which can run scripts"},
}

var (
	pdfNameRe   = regexp.MustCompile(`/[^\s/<>\[\]()%{}]+`)
	pdfStreamRe = regexp.MustCompile(`stream\r?\n`)
)

const (
	pdfStreamLimit    = 8 << 20   // inflated bytes read from one stream
	pdfInflatedBudget = 256 << 20 // inflated bytes read from all streams
	pdfMaxStreams     = 20000
)

func inspectPDF(ctx context.Context, f *os.File, size int64, in *inspection) error {
	names := map[string]bool{}
	starts := map[int64]bool{}
	const chunk, overlap = 1 << 20, 256
	buf := make([]byte, chunk+overlap)
	var base int64 // file offset of buf[0]
	carry := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := f.ReadAt(buf[carry:], base+int64(carry))
		data := buf[:carry+n]
		pdfCollectNames(data, names)
		for _, m := range pdfStreamRe.FindAllIndex(data, -1) {
			// "endstream" is not the start of one.
			if m[0] >= 3 && string(data[m[0]-3:m[0]]) == "end" {
				continue
			}
			if len(starts) < pdfMaxStreams {
				starts[base+int64(m[1])] = true
			}
		}
		if err == io.EOF || n == 0 {
			break
		}
		if err != nil {
			return fmt.Errorf("read spooled PDF: %w", err)
		}
		keep := min(overlap, len(data))
		copy(buf, data[len(data)-keep:])
		base += int64(len(data) - keep)
		carry = keep
	}

	budget := int64(pdfInflatedBudget)
	for start := range starts {
		if budget <= 0 {
			in.note("PDF content checked in %d MiB of decompressed streams; the rest were not", pdfInflatedBudget>>20)
			break
		}
		zr, err := zlib.NewReader(io.NewSectionReader(f, start, size-start))
		if err != nil {
			continue // not a Flate stream
		}
		inflated, _ := io.ReadAll(io.LimitReader(zr, min(pdfStreamLimit, budget)))
		zr.Close()
		budget -= int64(len(inflated))
		pdfCollectNames(inflated, names)
	}

	for name := range names {
		if what, ok := pdfNames[name]; ok {
			in.find("pdf", what[0], what[1])
		}
	}
	return nil
}

func pdfCollectNames(data []byte, names map[string]bool) {
	for _, m := range pdfNameRe.FindAll(data, -1) {
		name := pdfDecodeName(m[1:])
		if _, ok := pdfNames[name]; ok {
			names[name] = true
		}
	}
}

// pdfDecodeName undoes #xx escapes in a PDF name.
func pdfDecodeName(raw []byte) string {
	if !bytes.Contains(raw, []byte("#")) {
		return string(raw)
	}
	var out []byte
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i+2 < len(raw) {
			if v, err := strconv.ParseUint(string(raw[i+1:i+3]), 16, 8); err == nil {
				out = append(out, byte(v))
				i += 2
				continue
			}
		}
		out = append(out, raw[i])
	}
	return string(out)
}
