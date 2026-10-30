package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ContentCheckRepository keeps content checks' findings and the re-scans
// of published assets.
type ContentCheckRepository interface {
	ReplaceFindings(ctx context.Context, tenantID, assetID, jobID string, findings []model.ContentFinding, at time.Time) error
	AddFinding(ctx context.Context, tenantID, assetID, jobID string, f model.ContentFinding, at time.Time) error
	ListFindings(ctx context.Context, tenantID, assetID string) ([]model.ContentFinding, error)
	// RecordCoverage notes that the content checks ran on a job's upload,
	// FULL or PARTIAL.
	RecordCoverage(ctx context.Context, jobID, coverage string) error
	ClaimStaleScans(ctx context.Context, scanner, version string, now time.Time, limit int) ([]model.StaleScan, error)
	MarkRescanned(ctx context.Context, jobID, version string, at time.Time) error
	MarkRescanInfected(ctx context.Context, jobID, signature string, at time.Time) error
	// BlockAsset stops every read of a published asset.
	BlockAsset(ctx context.Context, tenantID, assetID, reason string, at time.Time) error
	// RaiseClassification makes an asset at least as restricted as class.
	RaiseClassification(ctx context.Context, tenantID, assetID, class string) error
}

// ContentChecks is what the API serves: findings, blast radius and the
// tenant's content policy.
type ContentChecks interface {
	ListFindings(ctx context.Context, tenantID, assetID string) ([]model.ContentFinding, error)
	BlastRadius(ctx context.Context, tenantID, assetID string, limit int) (model.BlastRadius, error)
	GetPolicy(ctx context.Context, tenantID string) (model.ContentPolicy, bool, error)
	PutPolicy(ctx context.Context, tenantID string, p model.ContentPolicy, at time.Time) error
}
