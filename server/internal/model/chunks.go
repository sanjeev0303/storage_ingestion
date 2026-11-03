package model

import (
	"errors"
	"time"
)

// The chunk index (docs/chunks.md).

// Chunk job states.
const (
	ChunkPending = "PENDING"
	ChunkRunning = "RUNNING"
	ChunkDone    = "DONE"
	ChunkFailed  = "FAILED"
)

// ChunkJob is indexing one version's files.
type ChunkJob struct {
	VersionID    string        `json:"version_id"`
	TenantID     string        `json:"-"`
	DatasetID    string        `json:"dataset_id"`
	State        string        `json:"state"`
	FilesIndexed int64         `json:"files_indexed"`
	BytesRead    int64         `json:"bytes_read"`
	Skipped      []SkippedFile `json:"skipped"`
	Error        string        `json:"error,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
}

// ErrChunkCorrupt: a chunk or a chunked file is not what its hashes say.
var ErrChunkCorrupt = errors.New("a stored chunk failed verification")

// ChunkExtent is one chunk of a chunked file, and the object holding it.
type ChunkExtent struct {
	Offset int64
	Length int
	SHA256 string
	Key    string
}

// ChunkStorageStats is what chunk storage holds for a dataset.
type ChunkStorageStats struct {
	Enabled bool `json:"enabled"`
	// Files served from chunks, and how large they are.
	Files        int64 `json:"files"`
	LogicalBytes int64 `json:"logical_bytes"`
	// Of those, the bytes whose original object has gone: until then the
	// original and the chunks both take space.
	RetiredSourceBytes int64 `json:"retired_source_bytes"`
	ChunkBytes         int64 `json:"chunk_bytes"`
	ChunkObjects       int64 `json:"chunk_objects"`
	Rehydrating        int64 `json:"rehydrating"`
}

// Delta upload states and errors.
const (
	DeltaOpen       = "OPEN"
	DeltaQueued     = "QUEUED"
	DeltaAssembling = "ASSEMBLING"
	DeltaDone       = "DONE"
	DeltaFailed     = "FAILED"
	DeltaExpired    = "EXPIRED"
)

var (
	ErrInvalidDelta  = errors.New("invalid request")
	ErrDeltaNotFound = errors.New("delta upload not found")
	ErrDeltaState    = errors.New("the delta upload cannot do that now")
)

// DeltaUpload is a new file sent as what it does not share with a snapshot.
type DeltaUpload struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"-"`
	OwnerID       string     `json:"-"`
	DatasetID     string     `json:"dataset_id"`
	Namespace     string     `json:"namespace"`
	BaseVersionID string     `json:"base_version_id"`
	Path          string     `json:"path"`
	SizeBytes     int64      `json:"size_bytes"`
	SHA256        string     `json:"sha256"`
	ContentType   string     `json:"content_type"`
	ConnectionID  string     `json:"-"`
	State         string     `json:"state"`
	ReusedBytes   int64      `json:"reused_bytes"`
	SentBytes     int64      `json:"sent_bytes"`
	UploadID      string     `json:"upload_id,omitempty"`
	Error         string     `json:"error,omitempty"`
	Attempts      int        `json:"-"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// DeltaChunk is one chunk of a delta upload.
type DeltaChunk struct {
	Ordinal      int
	Offset       int64
	Length       int
	SHA256       string
	SourceAsset  string
	SourceOffset int64
	Received     bool
}

// RawChunk is a chunk as the indexer has it.
type RawChunk struct {
	Offset int64
	Length int
	SHA256 []byte
}

// FileChunk is one chunk of a file.
type FileChunk struct {
	Ordinal int    `json:"ordinal"`
	Offset  int64  `json:"offset"`
	Length  int    `json:"length"`
	SHA256  string `json:"sha256"`
}

// ChunkSavings is what a dataset's versions hold, counted three ways. All is
// measured from the index: bytes stay stored as whole objects.
type ChunkSavings struct {
	Enabled bool `json:"enabled"`
	// Each version counted in full, as whole copies would be.
	LogicalBytes int64 `json:"logical_bytes"`
	// Each distinct file once: what whole-file dedup stores already.
	UniqueFileBytes int64 `json:"unique_file_bytes"`
	// Of the indexed files, the same two counts and each distinct chunk once.
	IndexedFiles     int64 `json:"indexed_files"`
	IndexedFileBytes int64 `json:"indexed_file_bytes"`
	UniqueChunkBytes int64 `json:"unique_chunk_bytes"`
	UniqueChunks     int64 `json:"unique_chunks"`
	NotIndexedFiles  int64 `json:"not_indexed_files"`
	NotIndexedBytes  int64 `json:"not_indexed_bytes"`
	WouldSaveBytes   int64 `json:"would_save_bytes"`
}

// DeltaFile is a changed file of a delta.
type DeltaFile struct {
	Path        string `json:"path"`
	SizeBytes   int64  `json:"size_bytes"`
	ReusedBytes int64  `json:"reused_bytes"`
	Indexed     bool   `json:"indexed"`
}

// ChunkDelta is what moving from one version to another needs.
type ChunkDelta struct {
	FromVersionID  string `json:"from_version_id"`
	ToVersionID    string `json:"to_version_id"`
	TotalBytes     int64  `json:"total_bytes"`
	UnchangedFiles int64  `json:"unchanged_files"`
	UnchangedBytes int64  `json:"unchanged_bytes"`
	ChangedFiles   int64  `json:"changed_files"`
	ChangedBytes   int64  `json:"changed_bytes"`
	// Of the changed files' bytes, those in chunks the old version holds.
	ReusedBytes int64 `json:"reused_bytes"`
	// What a delta download fetches, and what a whole-file sync would.
	FetchBytes     int64       `json:"fetch_bytes"`
	NotIndexed     int64       `json:"not_indexed_files"`
	LargestChanged []DeltaFile `json:"largest_changed"`
}
