package ingestion

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/jsonl"
)

// The manifest is JSON Lines: one object per line, read as a stream, so a
// manifest of millions of entries never has to fit in memory. A line's
// 1-based number is its entry's position, the entry's stable identity.
// Blank lines are skipped but keep their number.
//
//	{"path":"images/cat.jpg","size":48213,"content_type":"image/jpeg"}
//	{"path":"video/a.mp4","size":734003200,"content_type":"video/mp4","checksum":"sha256:9f86...","required":false,"metadata":{"camera":"a7"}}
//
// Fields: path (required), size in bytes (required, both upload paths
// verify it against the stored object), content_type (required), checksum
// ("sha256:<64 hex>", required: processing publishes an object only once
// its bytes match it), required (default true), metadata (a flat object of
// string values). Unknown fields are ignored, so a newer client can add
// fields older servers skip.

// ManifestLimits bound what one manifest and each of its lines may hold.
type ManifestLimits struct {
	MaxLineBytes       int
	MaxEntries         int
	MaxMetadataBytes   int
	MaxMetadataKeys    int
	MaxObjectBytes     int64
	SmallFileThreshold int64
	Path               model.PathPolicy
}

// ErrTooManyLines stops reading a manifest that runs past MaxEntries lines.
var ErrTooManyLines = errors.New("manifest has more lines than allowed")

// Line is one read manifest line: a valid entry in state PENDING, or an
// INVALID one with its reason.
type Line struct {
	Entry model.IngestionEntry
	// End is the byte offset just after the line: where reading resumes.
	End int64
}

type manifestLine struct {
	Path        string          `json:"path"`
	Size        *int64          `json:"size"`
	Checksum    string          `json:"checksum"`
	ContentType string          `json:"content_type"`
	Metadata    json.RawMessage `json:"metadata"`
	Required    *bool           `json:"required"`
}

// ManifestReader reads entries one line at a time. Memory is bounded by
// the longest allowed line, whatever the manifest's size.
type ManifestReader struct {
	lines  *jsonl.Reader
	limits ManifestLimits
	// skipThrough: lines up to this number are read past, not decoded.
	skipThrough int64
}

// NewManifestReader reads r, whose first byte is at offset startOffset of
// the manifest and whose first line is line startPosition+1. Lines up to
// skipThrough are read but not returned: a resubmitted manifest replays
// lines already stored.
func NewManifestReader(r io.Reader, limits ManifestLimits, startPosition, startOffset, skipThrough int64) *ManifestReader {
	return &ManifestReader{
		lines:       jsonl.NewReader(r, limits.MaxLineBytes, startPosition, startOffset),
		limits:      limits,
		skipThrough: skipThrough,
	}
}

// Position is the number of the last line read.
func (m *ManifestReader) Position() int64 { return m.lines.Line() }

// Offset is the byte offset just after the last line read.
func (m *ManifestReader) Offset() int64 { return m.lines.Offset() }

// Next returns the next non-blank line, or io.EOF after the last one.
// Other errors are fatal for the manifest: reading failed, or it has too
// many lines.
func (m *ManifestReader) Next() (Line, error) {
	for {
		raw, tooLong, err := m.lines.Next()
		if err != nil {
			return Line{}, err
		}
		position := m.lines.Line()
		if position > int64(m.limits.MaxEntries) {
			return Line{}, fmt.Errorf("%w (%d)", ErrTooManyLines, m.limits.MaxEntries)
		}
		if position <= m.skipThrough {
			continue
		}
		if tooLong {
			return Line{Entry: invalidEntry(position, true, model.EntryErrLineTooLong,
				fmt.Sprintf("line is longer than %d bytes", m.limits.MaxLineBytes)), End: m.lines.Offset()}, nil
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" {
			continue
		}
		return Line{Entry: m.decode(position, trimmed), End: m.lines.Offset()}, nil
	}
}

func (m *ManifestReader) decode(position int64, raw string) model.IngestionEntry {
	var line manifestLine
	if err := json.Unmarshal([]byte(raw), &line); err != nil {
		return invalidEntry(position, true, model.EntryErrInvalidJSON, clip(err.Error()))
	}
	required := true
	if line.Required != nil {
		required = *line.Required
	}
	invalid := func(code, detail string) model.IngestionEntry {
		return invalidEntry(position, required, code, detail)
	}

	path, key, err := model.NormalizePath(line.Path, m.limits.Path)
	if err != nil {
		return invalid(model.EntryErrInvalidPath, clip(err.Error()))
	}
	entry := model.IngestionEntry{Position: position, Path: path, PathKey: key, Required: required, State: model.EntryPending}

	switch {
	case line.Size == nil:
		return invalid(model.EntryErrInvalidSize, "size is required")
	case *line.Size <= 0:
		return invalid(model.EntryErrInvalidSize, "size must be greater than zero")
	case *line.Size > m.limits.MaxObjectBytes:
		return invalid(model.EntryErrTooLarge, fmt.Sprintf("larger than the %d-byte object limit", m.limits.MaxObjectBytes))
	}
	entry.SizeBytes = *line.Size

	if line.Checksum != "" {
		sum, ok := parseChecksum(line.Checksum)
		if !ok {
			return invalid(model.EntryErrInvalidChecksum, "checksum must be sha256:<64 hex digits>")
		}
		entry.ChecksumSHA256 = sum
	}

	contentType := strings.TrimSpace(line.ContentType)
	if contentType == "" || len(contentType) > 255 {
		return invalid(model.EntryErrInvalidContentType, "content_type is required, at most 255 bytes")
	}
	if _, _, err := mime.ParseMediaType(contentType); err != nil {
		return invalid(model.EntryErrInvalidContentType, "content_type is not a valid media type")
	}
	entry.ContentType = contentType

	if metadata, problem := m.metadata(line.Metadata); problem != "" {
		return invalid(model.EntryErrInvalidMetadata, problem)
	} else {
		entry.Metadata = metadata
	}

	entry.Strategy = model.StrategySingle
	if entry.SizeBytes > m.limits.SmallFileThreshold {
		entry.Strategy = model.StrategyMultipart
	}
	// Checked last, so a line with other problems reports those. Processing
	// publishes only an object whose SHA-256 matches the declared one, so
	// an entry without it could never be published.
	if entry.ChecksumSHA256 == "" {
		return invalid(model.EntryErrChecksumRequired, "checksum is required: the stored bytes are verified against it")
	}
	return entry
}

// metadata checks the optional metadata object: flat, string values only,
// within the key and byte limits. It returns it re-encoded with sorted keys.
func (m *ManifestReader) metadata(raw json.RawMessage) (json.RawMessage, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	if len(raw) > m.limits.MaxMetadataBytes {
		return nil, fmt.Sprintf("metadata is larger than %d bytes", m.limits.MaxMetadataBytes)
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, "metadata must be an object of string values"
	}
	if len(values) > m.limits.MaxMetadataKeys {
		return nil, fmt.Sprintf("metadata has more than %d keys", m.limits.MaxMetadataKeys)
	}
	for k := range values {
		if !model.ValidMetadataKey(k) {
			return nil, "metadata keys must be 1-64 characters of letters, digits, '.', '_' or '-'"
		}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, "metadata could not be encoded"
	}
	return encoded, ""
}

// parseChecksum accepts "sha256:<64 hex>" and returns the lower-case hex.
func parseChecksum(s string) (string, bool) {
	algorithm, value, ok := strings.Cut(s, ":")
	if !ok || !strings.EqualFold(algorithm, "sha256") || len(value) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", false
	}
	return strings.ToLower(value), true
}

func invalidEntry(position int64, required bool, code, detail string) model.IngestionEntry {
	return model.IngestionEntry{
		Position: position, Required: required, State: model.EntryInvalid,
		ErrorCode: code, ErrorDetail: detail,
	}
}

// clip keeps an error message short enough to store and show.
func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
