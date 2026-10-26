// Package compaction holds the physical pack format: many small objects'
// bytes concatenated into one pack object, described by a manifest.
package compaction

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Manifest describes one pack: which asset version's bytes lie where. It
// is written once, next to the pack, and never changed. Its encoding is
// canonical: fields in a fixed order and entries by ordinal, so the same
// pack always yields the same bytes and the same hash.
type Manifest struct {
	Format        string          `json:"format"`
	FormatVersion int             `json:"format_version"`
	PackID        string          `json:"pack_id"`
	TenantID      string          `json:"tenant_id"`
	ConnectionID  string          `json:"storage_connection_id"`
	PackBytes     int64           `json:"pack_bytes"`
	PackSHA256    string          `json:"pack_sha256"`
	EntryCount    int             `json:"entry_count"`
	Entries       []ManifestEntry `json:"entries"`
	// Checksum is the SHA-256 of this document encoded with Checksum "".
	Checksum string `json:"checksum"`
}

// ManifestEntry is one asset version in the pack. Without compression
// Length equals Size; SHA256 is of the original bytes.
type ManifestEntry struct {
	Ordinal     int    `json:"ordinal"`
	AssetID     string `json:"asset_id"`
	ObjectID    string `json:"object_id,omitempty"`
	Version     int    `json:"version,omitempty"`
	Offset      int64  `json:"offset"`
	Length      int64  `json:"length"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Compression string `json:"compression"`
}

// CompressionNone is the only entry encoding of format version 1.
const CompressionNone = "none"

// ErrInvalidManifest: a manifest that must not be trusted.
var ErrInvalidManifest = fmt.Errorf("%w: invalid manifest", model.ErrPackCorrupt)

// Encode returns the manifest's canonical bytes, with its checksum filled
// in.
func (m Manifest) Encode() ([]byte, error) {
	m.Checksum = ""
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	m.Checksum = hex.EncodeToString(sum[:])
	return json.Marshal(m)
}

// ParseManifest reads a manifest and checks everything a reader relies on
// before trusting it. maxEntries bounds the entry count.
func ParseManifest(data []byte, maxEntries int) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if err := m.Validate(maxEntries); err != nil {
		return Manifest{}, err
	}
	canonical, err := m.Encode()
	if err != nil {
		return Manifest{}, err
	}
	if !bytes.Equal(canonical, data) {
		// Either the checksum is wrong or the document is not in canonical
		// form; both mean it is not the manifest that was written.
		return Manifest{}, fmt.Errorf("%w: checksum or encoding does not match", ErrInvalidManifest)
	}
	return m, nil
}

// Validate checks the manifest's structure: the format, identifiers, and
// that the entries tile the pack exactly, in order, without overlap or
// overflow.
func (m Manifest) Validate(maxEntries int) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
	}
	if m.Format != model.PackFormat || m.FormatVersion != model.PackFormatVersion {
		return invalid("unsupported format %q version %d", m.Format, m.FormatVersion)
	}
	for name, id := range map[string]string{"pack_id": m.PackID, "tenant_id": m.TenantID, "storage_connection_id": m.ConnectionID} {
		if !ValidID(id) {
			return invalid("%s is not a valid identifier", name)
		}
	}
	if !validSHA256(m.PackSHA256) {
		return invalid("pack_sha256 is not a SHA-256")
	}
	if m.EntryCount != len(m.Entries) || len(m.Entries) == 0 || len(m.Entries) > maxEntries {
		return invalid("%d entries listed, entry_count %d, at most %d allowed", len(m.Entries), m.EntryCount, maxEntries)
	}
	seen := make(map[string]bool, len(m.Entries))
	var next int64
	for i, e := range m.Entries {
		switch {
		case e.Ordinal != i:
			return invalid("entry %d has ordinal %d", i, e.Ordinal)
		case !ValidID(e.AssetID) || (e.ObjectID != "" && !ValidID(e.ObjectID)):
			return invalid("entry %d has an invalid identifier", i)
		case seen[e.AssetID]:
			return invalid("asset %s is listed twice", e.AssetID)
		case e.Version < 0 || e.Length < 0 || e.Size < 0:
			return invalid("entry %d has a negative number", i)
		case e.Compression != CompressionNone || e.Length != e.Size:
			return invalid("entry %d: unsupported encoding", i)
		case !validSHA256(e.SHA256):
			return invalid("entry %d has no valid SHA-256", i)
		case e.Offset != next:
			return invalid("entry %d starts at %d, not at %d", i, e.Offset, next)
		case e.Length > math.MaxInt64-next:
			return invalid("entry %d overflows the pack", i)
		}
		seen[e.AssetID] = true
		next += e.Length
	}
	if next != m.PackBytes {
		return invalid("entries cover %d bytes of a %d-byte pack", next, m.PackBytes)
	}
	return nil
}

// ValidID reports whether s is a platform identifier: 1 to 128 letters,
// digits, '-', '_' or '.', never a path.
func ValidID(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		ok := c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

// validSHA256 reports whether s is a lowercase hex SHA-256.
func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
