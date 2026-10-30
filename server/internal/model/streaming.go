package model

import (
	"errors"
	"time"
)

// LayoutRow is a snapshot member in path order with the pack it is read
// from, if any (docs/streaming.md).
type LayoutRow struct {
	Ordinal   int64
	Path      string
	AssetID   string
	SizeBytes int64
	SHA256    string
	PackID    string
}

// Shard is a run of a snapshot's members, in path order, served as one
// tar stream. A sample (files sharing a path stem) is never split.
type Shard struct {
	Ordinals  []int64 `json:"ordinals"`
	Files     int     `json:"files"`
	Bytes     int64   `json:"bytes"`
	Packs     int     `json:"packs"`
	FirstPath string  `json:"first_path"`
	LastPath  string  `json:"last_path"`
}

// VerifyFailure is a file that did not read back as published.
type VerifyFailure struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// RunVerification is what a training run's data loader found when it
// re-hashed what it read.
type RunVerification struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"-"`
	RunID      string          `json:"run_id"`
	VersionID  string          `json:"version_id,omitempty"`
	Host       string          `json:"host"`
	SampleRate float64         `json:"sample_rate"`
	Checked    int64           `json:"checked"`
	Failed     int64           `json:"failed"`
	Failures   []VerifyFailure `json:"failures"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
}

// RunCheckpoint is a snapshot of the files a run saved at a step. It is
// verified once a host other than the writer re-read every file.
type RunCheckpoint struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"-"`
	RunID       string     `json:"run_id"`
	Step        int64      `json:"step"`
	VersionID   string     `json:"version_id"`
	WrittenBy   string     `json:"written_by"`
	VerifiedBy  string     `json:"verified_by,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
	VerifyError string     `json:"verify_error,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
}

var (
	ErrCheckpointExists   = errors.New("the run already has a checkpoint at that step")
	ErrCheckpointNotFound = errors.New("checkpoint not found")
	// ErrSameHost: a checkpoint is verified by a host other than the one
	// that wrote it, so a fault in that host's memory or CPU is not
	// repeated by the check.
	ErrSameHost = errors.New("a checkpoint must be verified on a host other than the one that wrote it")
)
