package interfaces

import (
	"context"
	"io"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ContentSample is what validators may inspect. The declared type and the
// file name come from the client and are untrusted. Head is the start of
// the stored object; Open streams the whole object for validators that
// need it (archive inspection), always with bounded memory.
type ContentSample struct {
	Head         []byte
	DeclaredType string
	// DetectedType is what the type check found in the bytes; checks
	// later in the chain see it.
	DetectedType string
	FileName     string
	AssetID      string
	SizeBytes    int64
	Open         func(ctx context.Context) (io.ReadCloser, error)
}

// ValidationResult is one validator's verdict. RejectReason is a
// sanitized code (model.Reject*) when the content must not be accepted;
// Detail is a short note for operators.
type ValidationResult struct {
	DetectedType string
	RejectReason string
	Detail       string
	// Findings are what a content check found. Each has a Code and Detail,
	// and may suggest an Action; the tenant's policy decides the action.
	Findings []model.ContentFinding
	// Notes say what a check did not look at (an object over its size
	// limit, a text file read only in part), for the processing record.
	Notes []string
}

// FileValidator judges stored content by its own bytes. New formats are
// supported by adding a validator to the chain. An error means the check
// could not run (e.g. a storage read failed) and is retried; it is never a
// pass.
type FileValidator interface {
	Name() string
	Validate(ctx context.Context, sample ContentSample, policy model.ContentPolicy) (ValidationResult, error)
}
