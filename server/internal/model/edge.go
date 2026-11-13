package model

import (
	"errors"
	"time"
)

// Edge and instrument ingest (docs/edge.md).

var (
	ErrDeviceNotFound    = errors.New("device or recording not found")
	ErrNotADevice        = errors.New("this call needs a device's credential")
	ErrInvalidEdge       = errors.New("invalid edge request")
	ErrChunkDiffers      = errors.New("a chunk at that path has other bytes")
	ErrOutsideDevicePath = errors.New("the path is outside what this credential may write")
	ErrRecordingComplete = errors.New("the recording is complete: it takes no more chunks")
	// ErrRecordingChanged: a chunk arrived while the recording was closing.
	ErrRecordingChanged = errors.New("a chunk arrived while the recording was closing")
)

const (
	RecordingRun       = "RUN"
	RecordingRecording = "RECORDING"

	RecordingOpen       = "OPEN"
	RecordingComplete   = "COMPLETE"
	RecordingIncomplete = "INCOMPLETE"
)

// EdgeDevice is a machine that uploads with its own credential.
type EdgeDevice struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"-"`
	Name          string     `json:"name"`
	DatasetID     string     `json:"dataset_id"`
	Namespace     string     `json:"namespace"`
	PathPrefix    string     `json:"path_prefix"`
	TokenID       string     `json:"token_id"`
	LastSeen      *time.Time `json:"last_seen,omitempty"`
	PendingFiles  int64      `json:"pending_files"`
	PendingBytes  int64      `json:"pending_bytes"`
	FailedFiles   int64      `json:"failed_files"`
	OldestPending *time.Time `json:"oldest_pending,omitempty"`
	AgentVersion  string     `json:"agent_version"`
	Note          string     `json:"note"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

// EdgeHeartbeat is what a device reports about itself.
type EdgeHeartbeat struct {
	PendingFiles  int64      `json:"pending_files"`
	PendingBytes  int64      `json:"pending_bytes"`
	FailedFiles   int64      `json:"failed_files"`
	OldestPending *time.Time `json:"oldest_pending,omitempty"`
	AgentVersion  string     `json:"agent_version"`
	Note          string     `json:"note"`
}

// EdgeRecording is a run or a recording.
type EdgeRecording struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"-"`
	DeviceID   string     `json:"device_id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"`
	GapSeconds int        `json:"gap_seconds"`
	Missing    []string   `json:"missing"`
	VersionID  string     `json:"version_id,omitempty"`
	Chunks     int64      `json:"chunks"`
	Bytes      int64      `json:"bytes"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
}

// EdgeChunk is one file of a run, or one chunk of a recording.
type EdgeChunk struct {
	Path      string     `json:"path"`
	SHA256    string     `json:"sha256"`
	SizeBytes int64      `json:"size_bytes"`
	StartsAt  *time.Time `json:"starts_at,omitempty"`
	EndsAt    *time.Time `json:"ends_at,omitempty"`
	AddedAt   time.Time  `json:"added_at"`
}

// EdgeGap is time a recording has no chunk for.
type EdgeGap struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// EdgeFile is a published file a device asks about.
type EdgeFile struct {
	AssetID  string
	ObjectID string
	Status   string
	Reason   string
}
