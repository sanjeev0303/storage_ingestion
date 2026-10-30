package model

import (
	"errors"
	"fmt"
	"time"
)

// PackState is where a pack, and the packing job that builds it, stands.
type PackState string

const (
	// PackPlanned: entries reserved, nothing written yet.
	PackPlanned PackState = "PLANNED"
	// PackWriting: the pack object is being written. A resumed job starts
	// the write over.
	PackWriting PackState = "WRITING"
	// PackVerifying: the pack object is written and recorded; it is read
	// back, and the manifest written, before publication.
	PackVerifying PackState = "VERIFYING"
	// PackPublished: readers resolve the entries to the pack.
	PackPublished PackState = "PUBLISHED"
	// PackRetiring: no entry is served from the pack any more; its objects
	// are being deleted.
	PackRetiring PackState = "RETIRING"
	// PackDeleted: the pack's objects are gone. The row stays as the record.
	PackDeleted PackState = "DELETED"
	// PackFailed: the job gave up; its objects are deleted next.
	PackFailed PackState = "FAILED"
)

// CanTransitionPack reports whether a pack may go from one state to
// another. A job retries within its state; it never moves backwards.
func CanTransitionPack(from, to PackState) bool {
	switch from {
	case PackPlanned:
		return to == PackWriting || to == PackFailed
	case PackWriting:
		return to == PackVerifying || to == PackFailed
	case PackVerifying:
		return to == PackPublished || to == PackFailed
	case PackPublished:
		return to == PackRetiring
	case PackRetiring, PackFailed:
		return to == PackDeleted
	}
	return false
}

// Building reports whether the pack's job is still running.
func (s PackState) Building() bool {
	return s == PackPlanned || s == PackWriting || s == PackVerifying
}

// PackEntryState is where one asset version stands in a pack.
type PackEntryState string

const (
	// PackEntryReserved: the asset is claimed by a pack being built.
	PackEntryReserved PackEntryState = "RESERVED"
	// PackEntryPublished: the asset's bytes are served from this entry.
	PackEntryPublished PackEntryState = "PUBLISHED"
	// PackEntryDeleted: the asset was deleted; the bytes are dead weight in the
	// pack until it is rewritten or retired.
	PackEntryDeleted PackEntryState = "DELETED"
	// PackEntryMoved: a rewrite moved the asset into another pack.
	PackEntryMoved PackEntryState = "MOVED"
	// PackEntryReleased: the pack failed before publication; the asset may be
	// packed again.
	PackEntryReleased PackEntryState = "RELEASED"
	// PackEntryExcluded: the source did not match its recorded size or
	// checksum. The asset is never packed again; an operator must look.
	PackEntryExcluded PackEntryState = "EXCLUDED"
)

// PackFormat names the physical format: entries' bytes concatenated, with
// a separate manifest.
const (
	PackFormat        = "ospack"
	PackFormatVersion = 1
)

// Pack is one physical object holding the bytes of many asset versions of
// one tenant, on one storage connection, together with the job that
// builds it.
type Pack struct {
	ID           string
	TenantID     string
	ConnectionID string
	Bucket       string
	// ObjectKey is the pack object; ManifestKey its manifest.
	ObjectKey     string
	ManifestKey   string
	Format        string
	FormatVersion int
	State         PackState
	EntryCount    int
	TotalBytes    int64
	// SHA256 is of the whole pack object, ManifestSHA256 of the manifest
	// document; neither stands in for an entry's own checksum.
	SHA256         string
	ManifestSHA256 string
	// StorageETag and StorageVersionID pin reads to the pack object as
	// written.
	StorageETag      string
	StorageVersionID string
	// RewriteOf is the pack whose live entries this one takes over.
	RewriteOf     string
	Attempts      int
	MaxAttempts   int
	NextAttemptAt time.Time
	LeaseOwner    string
	LeaseExpires  *time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	PublishedAt   *time.Time
	RetiredAt     *time.Time
	DeletedAt     *time.Time
}

// PackEntry is one asset version inside a pack.
type PackEntry struct {
	PackID   string
	Ordinal  int
	TenantID string
	AssetID  string
	// ObjectID and Version name the catalog version ("" and 0 for an asset
	// from before the catalog).
	ObjectID string
	Version  int
	// Offset and Length are the stored bytes' place in the pack; SizeBytes
	// is the logical size. Without compression they are equal.
	Offset         int64
	Length         int64
	SizeBytes      int64
	ChecksumSHA256 string
	// The source copy the bytes came from: the asset's standalone object,
	// or an entry of the pack this one rewrites.
	SourceKey       string
	SourceETag      string
	SourceVersionID string
	// SourcePackID and SourceOffset, when set, are the pack and offset the
	// bytes are copied from (a rewrite or a repack); SourceKey is then that
	// pack's object.
	SourcePackID    string
	SourceOffset    int64
	State           PackEntryState
	SourceRetiredAt *time.Time
	DeletedAt       *time.Time
}

// PackedLocation is where a packed asset's bytes are read: Length bytes
// at Offset of the pack object.
type PackedLocation struct {
	PackID           string
	ConnectionID     string
	Bucket           string
	ObjectKey        string
	StorageETag      string
	StorageVersionID string
	PackBytes        int64
	Offset           int64
	Length           int64
}

var (
	ErrPackNotFound = errors.New("pack not found")
	// ErrPackCorrupt: a pack or manifest failed verification.
	ErrPackCorrupt = errors.New("pack failed verification")
	// ErrPackConflict: an asset is already reserved or published in a pack.
	ErrPackConflict = errors.New("asset already in a pack")
)

// Range is the pack's byte range, start to end inclusive, holding bytes
// first to last of the entry; last -1 means the entry's last byte. It
// never reaches outside the entry: a read of it can return no neighbour's
// bytes.
func (l PackedLocation) Range(first, last int64) (int64, int64, error) {
	if last < 0 {
		last = l.Length - 1
	}
	if first < 0 || first > last || last >= l.Length {
		return 0, 0, fmt.Errorf("bytes %d-%d are outside a %d-byte entry", first, last, l.Length)
	}
	return l.Offset + first, l.Offset + last, nil
}

// RepackRequest gathers a dataset's packed entries, in path order, into
// new packs of the organization's target size (docs/streaming.md).
type RepackRequest struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"-"`
	DatasetID      string     `json:"dataset_id"`
	State          string     `json:"state"` // PENDING, DONE or CANCELLED
	CursorKey      string     `json:"-"`
	CursorAsset    string     `json:"-"`
	PacksPlanned   int        `json:"packs_planned"`
	EntriesPlanned int        `json:"entries_planned"`
	RequestedBy    string     `json:"requested_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

var (
	// ErrRepackActive: the dataset already has a repack under way.
	ErrRepackActive   = errors.New("a repack of this dataset is already under way")
	ErrRepackNotFound = errors.New("repack not found")
)
