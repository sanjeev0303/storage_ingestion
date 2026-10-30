package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ContentInspector looks inside an upload's bytes for what its type alone
// does not show: active content in documents, code in model files,
// credentials in text, and files whose format is broken. It opens the
// object at most once and hands it to the one check its format calls for.
// What it finds is a finding; the tenant's policy decides whether a
// finding flags the asset or rejects it.
type ContentInspector struct {
	Limits ArchiveLimits
	// MaxBytes: larger objects are not inspected, and a note says so.
	MaxBytes int64
	// SecretScanBytes is how much of a text file the secret scan reads.
	SecretScanBytes int64
	// TempDir is where zips and PDFs are spooled; empty uses the OS default.
	TempDir string
}

func (ContentInspector) Name() string { return "content" }

// inspection collects one object's findings and notes.
type inspection struct {
	findings []model.ContentFinding
	notes    []string
}

func (in *inspection) find(source, code, detail string) {
	for _, f := range in.findings {
		if f.Code == code {
			return // one finding per code is enough to act on
		}
	}
	in.findings = append(in.findings, model.ContentFinding{Source: source, Code: code, Detail: detail})
}

func (in *inspection) note(format string, args ...any) {
	in.notes = append(in.notes, fmt.Sprintf(format, args...))
}

func (v ContentInspector) Validate(ctx context.Context, sample interfaces.ContentSample, _ model.ContentPolicy) (interfaces.ValidationResult, error) {
	format := contentFormat(sample)
	if format == "" {
		return interfaces.ValidationResult{}, nil
	}
	in := &inspection{}
	if v.MaxBytes > 0 && sample.SizeBytes > v.MaxBytes {
		in.note("%s content checks skipped: the object is larger than %d bytes", format, v.MaxBytes)
		return interfaces.ValidationResult{Notes: in.notes}, nil
	}

	var err error
	switch format {
	case "zip", "pdf":
		err = v.spooled(ctx, sample, func(f *os.File, size int64) error {
			if format == "pdf" {
				return inspectPDF(ctx, f, size, in)
			}
			return v.inspectZip(ctx, f, size, in)
		})
	default:
		err = v.streamed(ctx, sample, format, in)
	}
	if err != nil {
		return interfaces.ValidationResult{}, err
	}
	return interfaces.ValidationResult{Findings: in.findings, Notes: in.notes}, nil
}

// contentFormat picks the check for an object from its own bytes, and from
// its name where the bytes alone are ambiguous (FASTQ is plain text).
func contentFormat(sample interfaces.ContentSample) string {
	head := sample.Head
	name := strings.ToLower(sample.FileName)
	ext := path.Ext(name)
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return "pdf"
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return "zip"
	case bytes.HasPrefix(head, oleMagic):
		return "ole"
	case bytes.HasPrefix(head, mcapMagic):
		return "mcap"
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return "gzip"
	case bytes.HasPrefix(head, npyMagic):
		return "npy"
	case len(head) >= 2 && head[0] == 0x80 && head[1] >= 2 && head[1] <= 5:
		return "pickle"
	case ext == ".safetensors":
		return "safetensors"
	case len(head) > 262 && string(head[257:262]) == "ustar":
		return "tar"
	case ext == ".fastq" || ext == ".fq":
		return "fastq"
	case ext == ".mcap":
		return "mcap" // named MCAP without the magic: the format check says so
	case pickleNames[ext]:
		return "pickle"
	case isText(sample):
		return "text"
	}
	return ""
}

var pickleNames = map[string]bool{".pkl": true, ".pickle": true, ".pt": true, ".pth": true, ".ckpt": true, ".joblib": true}

// isText: the secret scan reads what was detected as text, and the usual
// configuration and notebook files whatever they were declared as.
func isText(sample interfaces.ContentSample) bool {
	switch sample.DetectedType {
	case "text/plain", "text/csv", "application/json":
		return true
	}
	switch strings.ToLower(path.Ext(sample.FileName)) {
	case ".env", ".yaml", ".yml", ".ini", ".cfg", ".conf", ".toml", ".properties", ".ipynb", ".tf", ".tfvars", ".py", ".r", ".sql", ".log", ".txt", ".csv", ".tsv", ".json", ".jsonl", ".md":
		return !hasBinaryBytes(sample.Head)
	}
	return false
}

// streamed reads the object once, front to back, into the format's check.
func (v ContentInspector) streamed(ctx context.Context, sample interfaces.ContentSample, format string, in *inspection) error {
	body, err := sample.Open(ctx)
	if err != nil {
		return fmt.Errorf("open object for content checks: %w", err)
	}
	defer body.Close()
	r := &ctxReader{ctx: ctx, r: body}
	switch format {
	case "ole":
		return inspectOLE(r, in)
	case "mcap":
		return inspectMCAP(r, sample.SizeBytes, in)
	case "gzip":
		return v.inspectGzip(r, sample, in)
	case "tar":
		return v.inspectTar(r, sample.SizeBytes, in)
	case "npy":
		return inspectNPY(r, "npy", in)
	case "pickle":
		return inspectPickles(r, "pickle", in)
	case "safetensors":
		return inspectSafetensors(r, sample.SizeBytes, in)
	case "fastq":
		return inspectFASTQ(r, "fastq", in)
	case "text":
		return scanSecrets(r, v.SecretScanBytes, in)
	}
	return nil
}

// spooled copies the object to a temp file, for formats read out of order.
func (v ContentInspector) spooled(ctx context.Context, sample interfaces.ContentSample, fn func(*os.File, int64) error) error {
	spool, err := os.CreateTemp(v.TempDir, "content-inspect-*")
	if err != nil {
		return fmt.Errorf("create content spool: %w", err)
	}
	defer func() {
		spool.Close()
		os.Remove(spool.Name())
	}()
	body, err := sample.Open(ctx)
	if err != nil {
		return fmt.Errorf("open object for content checks: %w", err)
	}
	written, err := io.Copy(spool, &ctxReader{ctx: ctx, r: body})
	body.Close()
	if err != nil {
		return fmt.Errorf("spool object for content checks: %w", err)
	}
	return fn(spool, written)
}

// inspectZip looks at the entries of a zip: Office documents' macros and
// embedded objects, the pickles inside PyTorch checkpoints and the arrays
// inside .npz files. Unreadable zips are the archive check's business.
func (v ContentInspector) inspectZip(ctx context.Context, f *os.File, size int64, in *inspection) error {
	reader, err := zip.NewReader(f, size)
	if err != nil {
		return nil
	}
	if v.Limits.MaxEntries > 0 && len(reader.File) > v.Limits.MaxEntries {
		in.note("zip content checks skipped: more than %d entries", v.Limits.MaxEntries)
		return nil
	}
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.ToLower(entry.Name)
		base := path.Base(name)
		switch {
		case base == "vbaproject.bin":
			in.find("office", "active_content.ooxml_macro", "the document contains a VBA macro project ("+entry.Name+")")
		case strings.Contains(name, "/activex/"):
			in.find("office", "active_content.ooxml_activex", "the document contains ActiveX controls")
		case strings.Contains(name, "/embeddings/") && strings.HasSuffix(name, ".bin"):
			in.find("office", "active_content.ooxml_ole_object", "the document embeds an OLE object ("+entry.Name+")")
		case strings.HasSuffix(name, ".pkl"):
			if err := v.zipEntry(entry, func(r io.Reader) error { return inspectPickles(r, "pytorch", in) }); err != nil {
				return err
			}
		case strings.HasSuffix(name, ".npy"):
			if err := v.zipEntry(entry, func(r io.Reader) error { return inspectNPY(r, "npz", in) }); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v ContentInspector) zipEntry(entry *zip.File, fn func(io.Reader) error) error {
	rc, err := entry.Open()
	if err != nil {
		return nil // an unreadable entry is the archive check's business
	}
	defer rc.Close()
	limit := v.Limits.MaxUncompressedBytes
	if limit <= 0 {
		limit = 1 << 30
	}
	return fn(io.LimitReader(rc, limit))
}

// ctxReader stops a long read when the job's context ends, and marks a
// failed read of the object as a sourceError, so the checks retry it
// instead of taking it for a broken file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, sourceError{err}
	}
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF {
		err = sourceError{err}
	}
	return n, err
}

// tailReader remembers the last bytes read through it, for formats that
// end with a marker (BGZF's EOF block, MCAP's closing magic).
type tailReader struct {
	r    io.Reader
	n    int
	tail []byte
	read int64
}

func (t *tailReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.read += int64(n)
		t.tail = append(t.tail, p[:n]...)
		if len(t.tail) > t.n {
			t.tail = append(t.tail[:0], t.tail[len(t.tail)-t.n:]...)
		}
	}
	return n, err
}
