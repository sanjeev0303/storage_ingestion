package processing

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

// Format checks: a file that claims a format but is broken is refused at
// the gate instead of failing a pipeline weeks later. Each reads the
// object once, front to back.

// bgzfEOF is the empty block that ends every complete BGZF file (BAM,
// bgzipped VCF and FASTQ). Without it the file was cut short.
var bgzfEOF = []byte{0x1f, 0x8b, 0x08, 0x04, 0, 0, 0, 0, 0, 0xff, 0x06, 0, 0x42, 0x43, 0x02, 0, 0x1b, 0, 0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0}

var (
	errExpandedLimit = errors.New("expanded past the inspection limit")
	errBomb          = errors.New("compression ratio too high")
)

// expandReader counts the bytes a decompressor yields, against the
// compressed bytes read so far.
type expandReader struct {
	r          io.Reader
	compressed *tailReader
	n          int64
	limit      int64
	ratio      int64
}

func (e *expandReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n += int64(n)
	// Small outputs say nothing about a bomb; text compresses 10:1.
	if e.ratio > 0 && e.n > 64<<20 && e.compressed.read > 0 && e.n/e.compressed.read > e.ratio {
		return n, errBomb
	}
	if e.limit > 0 && e.n > e.limit {
		return n, errExpandedLimit
	}
	return n, err
}

// inspectGzip checks a gzip stream to its end (gzip verifies each
// member's CRC-32 and length), and what it holds: a tar's entries, a
// BAM's records or a FASTQ's reads. A BGZF file must end with its EOF block.
func (v ContentInspector) inspectGzip(r io.Reader, sample interfaces.ContentSample, in *inspection) error {
	compressed := &tailReader{r: r, n: len(bgzfEOF)}
	zr, err := gzip.NewReader(compressed)
	if err != nil {
		in.find("gzip", "format.corrupt_gzip", "not a valid gzip stream")
		return nil
	}
	bgzf := isBGZF(zr.Header.Extra)
	expanded := &expandReader{r: zr, compressed: compressed, limit: v.Limits.MaxUncompressedBytes, ratio: v.Limits.MaxRatio}
	br := bufio.NewReaderSize(expanded, 64<<10)

	inner := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(sample.FileName), ".gz"), ".bgz"), ".tgz")
	head, _ := br.Peek(262)
	switch {
	case bgzf && bytes.HasPrefix(head, []byte("BAM\x01")):
		err = inspectBAM(br, in)
	case len(head) >= 262 && string(head[257:262]) == "ustar", strings.HasSuffix(sample.FileName, ".tgz"):
		err = v.inspectTar(br, 0, in)
	case path.Ext(inner) == ".fastq" || path.Ext(inner) == ".fq":
		err = inspectFASTQ(br, "fastq", in)
	}
	if err == nil {
		// Read what the inner check left, so every member's CRC is checked.
		_, err = io.Copy(io.Discard, br)
	}
	switch {
	case errors.Is(err, errBomb):
		in.find("gzip", "format.archive_bomb", fmt.Sprintf("expands more than %d times its compressed size", v.Limits.MaxRatio))
		return nil
	case errors.Is(err, errExpandedLimit):
		in.note("gzip content checked through its first %d expanded bytes; the rest and its CRC were not", v.Limits.MaxUncompressedBytes)
		return nil
	case err != nil:
		if isSourceError(err) {
			return err
		}
		in.find("gzip", "format.corrupt_gzip", "the compressed data is damaged: "+err.Error())
		return nil
	}
	if bgzf && !bytes.Equal(compressed.tail, bgzfEOF) {
		in.find("gzip", "format.truncated_bgzf", "the BGZF end-of-file block is missing: the file was cut short")
	}
	return nil
}

func isBGZF(extra []byte) bool {
	for len(extra) >= 4 {
		size := int(binary.LittleEndian.Uint16(extra[2:4]))
		if extra[0] == 'B' && extra[1] == 'C' {
			return true
		}
		if len(extra) < 4+size {
			return false
		}
		extra = extra[4+size:]
	}
	return false
}

// inspectTar walks a tar's headers: too many entries, paths that leave
// the extraction directory, links pointing out of it and device files are
// what extraction tools get wrong.
func (v ContentInspector) inspectTar(r io.Reader, _ int64, in *inspection) error {
	tr := tar.NewReader(r)
	entries := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if passthrough(err) {
				return err
			}
			in.find("tar", "format.corrupt_tar", "the archive cannot be read: "+err.Error())
			return nil
		}
		entries++
		if v.Limits.MaxEntries > 0 && entries > v.Limits.MaxEntries {
			in.find("tar", "format.archive_too_many_entries", fmt.Sprintf("more than %d entries", v.Limits.MaxEntries))
			return nil
		}
		switch {
		case !insideArchive(h.Name):
			in.find("tar", "format.unsafe_archive_entry", "entry "+h.Name+" would extract outside its directory")
		case (h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeLink) && !insideArchive(path.Join(path.Dir(h.Name), h.Linkname)):
			in.find("tar", "format.unsafe_archive_entry", "link "+h.Name+" points outside the archive")
		case h.Typeflag == tar.TypeChar || h.Typeflag == tar.TypeBlock || h.Typeflag == tar.TypeFifo:
			in.find("tar", "format.unsafe_archive_entry", "entry "+h.Name+" is a device file")
		}
	}
}

func insideArchive(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	clean := path.Clean(name)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

// inspectBAM walks a BAM's header and every alignment record: lengths
// that fit, and references that exist. samtools quickcheck looks only at
// the header and the EOF block; a record cut in half passes it.
func inspectBAM(r io.Reader, in *inspection) error {
	bad := func(err error, format string, args ...any) error {
		if passthrough(err) {
			return err
		}
		in.find("bam", "format.invalid_bam", fmt.Sprintf(format, args...))
		return nil
	}
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return bad(err, "the header is cut short")
	}
	textLen, err := readInt32(r)
	if err != nil || textLen < 0 || textLen > 1<<30 {
		return bad(err, "the header text length is invalid")
	}
	if _, err := io.CopyN(io.Discard, r, int64(textLen)); err != nil {
		return bad(err, "the header text is cut short")
	}
	refs, err := readInt32(r)
	if err != nil || refs < 0 || refs > 1<<24 {
		return bad(err, "the reference count is invalid")
	}
	for i := int32(0); i < refs; i++ {
		nameLen, err := readInt32(r)
		if err != nil || nameLen < 1 || nameLen > 1<<16 {
			return bad(err, "reference %d has an invalid name", i)
		}
		if _, err := io.CopyN(io.Discard, r, int64(nameLen)+4); err != nil {
			return bad(err, "reference %d is cut short", i)
		}
	}
	buf := make([]byte, 64<<10)
	for record := int64(1); ; record++ {
		size, err := readInt32(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return bad(err, "record %d is cut short", record)
		}
		if size < 32 || size > 64<<20 {
			return bad(nil, "record %d has an invalid length %d", record, size)
		}
		if int(size) > len(buf) {
			buf = make([]byte, size)
		}
		body := buf[:size]
		if _, err := io.ReadFull(r, body); err != nil {
			return bad(err, "record %d is cut short", record)
		}
		refID := int32(binary.LittleEndian.Uint32(body[0:4]))
		nameLen := int32(body[8])
		nextRef := int32(binary.LittleEndian.Uint32(body[20:24]))
		if refID < -1 || refID >= refs || nextRef < -1 || nextRef >= refs {
			return bad(nil, "record %d names a reference that does not exist", record)
		}
		if nameLen < 1 || 32+nameLen > size {
			return bad(nil, "record %d has an invalid read name", record)
		}
	}
}

func readInt32(r io.Reader) (int32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return int32(binary.LittleEndian.Uint32(b[:])), nil
}

// inspectFASTQ checks every record: four lines, "@" then sequence then
// "+" then a quality string as long as the sequence, in printable ASCII.
func inspectFASTQ(r io.Reader, source string, in *inspection) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	var lines [4][]byte
	for record := int64(1); ; record++ {
		got := 0
		for got < 4 && sc.Scan() {
			line := bytes.TrimRight(sc.Bytes(), "\r")
			if got == 0 && len(line) == 0 {
				continue // blank lines between or after records
			}
			lines[got] = append(lines[got][:0], line...)
			got++
		}
		if err := sc.Err(); err != nil {
			if passthrough(err) {
				return err
			}
			in.find(source, "format.invalid_fastq", fmt.Sprintf("record %d cannot be read: %v", record, err))
			return nil
		}
		if got == 0 {
			if record == 1 {
				in.find(source, "format.invalid_fastq", "no records")
			}
			return nil
		}
		if detail := fastqRecordProblem(lines, got); detail != "" {
			in.find(source, "format.invalid_fastq", fmt.Sprintf("record %d: %s", record, detail))
			return nil
		}
	}
}

func fastqRecordProblem(lines [4][]byte, got int) string {
	if got < 4 {
		return "the file ends inside the record"
	}
	if lines[0][0] != '@' {
		return "the header line does not start with @"
	}
	if len(lines[2]) == 0 || lines[2][0] != '+' {
		return "the separator line does not start with +"
	}
	if len(lines[1]) != len(lines[3]) {
		return fmt.Sprintf("the sequence has %d bases but %d quality scores", len(lines[1]), len(lines[3]))
	}
	for _, c := range lines[1] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '.' || c == '-' || c == '*') {
			return fmt.Sprintf("the sequence has the character %q", c)
		}
	}
	for _, c := range lines[3] {
		if c < 33 || c > 126 {
			return fmt.Sprintf("the quality string has the byte 0x%02x", c)
		}
	}
	return ""
}

var mcapMagic = []byte("\x89MCAP0\r\n")

// inspectMCAP walks an MCAP recording's records to its footer and closing
// magic. A recorder that crashed leaves no footer; readers then need a
// recovery pass, and summaries (message counts, time range) are missing.
func inspectMCAP(r io.Reader, size int64, in *inspection) error {
	bad := func(err error, detail string) error {
		if passthrough(err) {
			return err
		}
		in.find("mcap", "format.invalid_mcap", detail)
		return nil
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var magic [8]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || !bytes.Equal(magic[:], mcapMagic) {
		return bad(err, "the MCAP magic is missing")
	}
	read := int64(len(mcapMagic))
	var hdr [9]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return bad(err, "the recording has no footer: it was cut short")
		}
		opcode, length := hdr[0], binary.LittleEndian.Uint64(hdr[1:])
		read += 9
		if opcode == 0 || length > uint64(size-read) {
			return bad(nil, fmt.Sprintf("a record at byte %d is invalid", read-9))
		}
		if _, err := io.CopyN(io.Discard, br, int64(length)); err != nil {
			return bad(err, "a record is cut short")
		}
		read += int64(length)
		if opcode == 0x02 { // footer
			break
		}
	}
	if _, err := io.ReadFull(br, magic[:]); err != nil || !bytes.Equal(magic[:], mcapMagic) {
		return bad(err, "the closing magic after the footer is missing")
	}
	return nil
}

var oleMagic = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe0 + 1}

// inspectOLE looks through a legacy Office file (.doc, .xls, .ppt, .msg)
// for the directory entries that hold a VBA project or an embedded package.
// Entry names are UTF-16, and a macro document always has _VBA_PROJECT.
func inspectOLE(r io.Reader, in *inspection) error {
	found, err := streamContains(r, map[string][]byte{
		"macro":   utf16le("_VBA_PROJECT"),
		"package": utf16le("\x01Ole10Native"),
	})
	if err != nil {
		return err
	}
	if found["macro"] {
		in.find("office", "active_content.ole_macro", "the document contains a VBA macro project")
	}
	if found["package"] {
		in.find("office", "active_content.ole_package", "the document embeds a packaged file")
	}
	return nil
}

func utf16le(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		out = append(out, s[i], 0)
	}
	return out
}

// streamContains reports which patterns occur in r, reading it in chunks
// that overlap by the longest pattern.
func streamContains(r io.Reader, patterns map[string][]byte) (map[string]bool, error) {
	longest := 0
	for _, p := range patterns {
		longest = max(longest, len(p))
	}
	found := map[string]bool{}
	buf := make([]byte, 0, 1<<20+longest)
	chunk := make([]byte, 1<<20)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for name, p := range patterns {
			if !found[name] && bytes.Contains(buf, p) {
				found[name] = true
			}
		}
		if len(found) == len(patterns) {
			return found, nil
		}
		if len(buf) > longest {
			buf = append(buf[:0], buf[len(buf)-longest:]...)
		}
		if err == io.EOF {
			return found, nil
		}
		if err != nil {
			return found, err
		}
	}
}

// sourceError marks a failure to read the object itself, which is retried
// rather than taken as a sign the format is broken.
type sourceError struct{ err error }

func (e sourceError) Error() string { return e.err.Error() }
func (e sourceError) Unwrap() error { return e.err }

func isSourceError(err error) bool {
	var se sourceError
	return errors.As(err, &se)
}

// passthrough: a failure to read the object, or a decompression limit, is
// not a broken format and goes up to the caller instead of a finding.
func passthrough(err error) bool {
	return err != nil && (isSourceError(err) || errors.Is(err, errBomb) || errors.Is(err, errExpandedLimit))
}
