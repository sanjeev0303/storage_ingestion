package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DatasetRepository stores managed datasets: their draft membership,
// immutable versions, manifest imports, validation profiles, jobs,
// findings and reports, and the lineage graph. Every method is scoped to
// one tenant, except the worker's claims, which return each job with its
// own tenant.
type DatasetRepository interface {
	// CreateDataset creates a dataset, or returns the tenant's dataset of
	// that name when its settings match. A derived dataset's draft is
	// filled from its source versions in the same transaction.
	CreateDataset(ctx context.Context, d model.Dataset) (model.Dataset, bool, error)
	GetDataset(ctx context.Context, tenantID, datasetID string) (model.Dataset, error)
	ListDatasets(ctx context.Context, tenantID string, f DatasetFilter) ([]model.Dataset, error)
	UpdateDataset(ctx context.Context, tenantID, datasetID string, revision int64, patch DatasetPatch, actor string, now time.Time) (model.Dataset, error)
	ArchiveDataset(ctx context.Context, tenantID, datasetID, actor string, now time.Time) (model.Dataset, error)

	// ResolveObjects finds the tenant's catalog objects and versions the
	// references name. A reference no object matches resolves to nothing.
	ResolveObjects(ctx context.Context, tenantID string, refs []ObjectRef) ([]ResolvedRef, error)
	// AddMembers writes resolved members under the dataset's lock. With
	// a checkpoint it is one batch of a manifest import, committed with
	// the import's progress and rejected lines.
	AddMembers(ctx context.Context, tenantID, datasetID string, changes []model.MemberChange, policy model.DuplicatePolicy,
		source, actor string, now time.Time, cp *ImportCheckpoint) (model.MemberResult, error)
	RemoveMember(ctx context.Context, tenantID, datasetID, memberID, actor string, now time.Time) (model.Dataset, error)
	ListMembers(ctx context.Context, tenantID, datasetID, after string, limit int) (model.MemberPage, error)

	CreateSnapshot(ctx context.Context, in SnapshotInput) (model.DatasetVersion, bool, error)
	GetVersion(ctx context.Context, tenantID, versionID string) (model.DatasetVersion, error)
	ListVersions(ctx context.Context, tenantID, datasetID string, before, limit int) ([]model.DatasetVersion, error)
	ListVersionMembers(ctx context.Context, tenantID, versionID string, after int64, limit int) ([]model.VersionMember, error)

	// CreateImport records an import; maxActive caps the tenant's
	// unfinished ones.
	CreateImport(ctx context.Context, imp model.ManifestImport, inline []byte, maxActive int, ev *model.OutboxEvent) (model.ManifestImport, bool, error)
	SubmitImport(ctx context.Context, tenantID, importID, etag string, size int64, now time.Time, ev model.OutboxEvent) (model.ManifestImport, error)
	GetImport(ctx context.Context, tenantID, importID string) (model.ManifestImport, error)
	ListImportErrors(ctx context.Context, tenantID, importID string, afterLine int64, limit int) ([]model.ImportError, error)
	// ClaimImports leases due imports: importID alone, or any when it is "".
	ClaimImports(ctx context.Context, owner, importID string, now time.Time, lease time.Duration, limit int) ([]model.ManifestImport, error)
	InlineManifest(ctx context.Context, importID string, offset int64) ([]byte, error)
	// ExtendImport renews the lease and says whether cancellation was
	// asked for.
	ExtendImport(ctx context.Context, importID, owner string, now time.Time, lease time.Duration) (bool, error)
	FinishImport(ctx context.Context, importID, owner string, state model.ImportState, lastError string, now time.Time) error
	RetryImport(ctx context.Context, importID, owner string, next time.Time, lastError string, now time.Time) error
	CancelImport(ctx context.Context, tenantID, importID string, now time.Time) (model.ManifestImport, error)

	CreateProfile(ctx context.Context, p model.ValidationProfile) (model.ValidationProfile, error)
	GetProfile(ctx context.Context, tenantID, profileID string, version int) (model.ValidationProfile, error)
	ListProfiles(ctx context.Context, tenantID string) ([]model.ValidationProfile, error)

	CreateValidation(ctx context.Context, job model.ValidationJob, maxActive int, ev model.OutboxEvent) (model.ValidationJob, bool, error)
	GetValidation(ctx context.Context, tenantID, jobID string) (model.ValidationJob, error)
	// ListValidations and ListReports page newest first; next is the
	// cursor of the following page, "" at the end.
	ListValidations(ctx context.Context, tenantID, datasetID, after string, limit int) (jobs []model.ValidationJob, next string, err error)
	ClaimValidations(ctx context.Context, owner, jobID string, now time.Time, lease time.Duration, perTenant, limit int) ([]model.ValidationJob, error)
	// ExtendValidation renews the lease and says whether cancellation was
	// asked for.
	ExtendValidation(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) (bool, error)
	MemberEvidence(ctx context.Context, tenantID, versionID string, after int64, limit int) ([]model.MemberEvidence, error)
	CommitValidationBatch(ctx context.Context, b ValidationBatch) error
	Aggregates(ctx context.Context, tenantID, versionID string, topN int) (model.VersionAggregates, error)
	CompleteValidation(ctx context.Context, job model.ValidationJob, owner string, report model.ValidationReport, findings []model.Finding) error
	FailValidation(ctx context.Context, jobID, owner, lastError string, now time.Time) error
	RetryValidation(ctx context.Context, jobID, owner string, next time.Time, lastError string, now time.Time) error
	CancelValidation(ctx context.Context, tenantID, jobID string, now time.Time) (model.ValidationJob, error)
	// StopValidation ends a running job its worker found cancelled.
	StopValidation(ctx context.Context, jobID, owner string, now time.Time) error
	GetReport(ctx context.Context, tenantID, reportID string) (model.ValidationReport, error)
	ListReports(ctx context.Context, tenantID, datasetID string, readinessOnly bool, after string, limit int) (reports []model.ValidationReport, next string, err error)
	ListFindings(ctx context.Context, tenantID, jobID string, f FindingFilter) (model.FindingPage, error)
	// ReleaseStuckDatasets returns datasets left VALIDATING without a live
	// validation to DRAFT.
	ReleaseStuckDatasets(ctx context.Context, now time.Time) (int64, error)
	JobStats(ctx context.Context, now time.Time) (DatasetJobStats, error)

	AddLineage(ctx context.Context, tenantID string, e model.LineageEdge, now time.Time) error
	Lineage(ctx context.Context, tenantID string, start model.LineageRef, q LineageQuery) (model.LineagePage, error)
}

// DatasetFilter selects a page of datasets, by name.
type DatasetFilter struct {
	Lifecycle model.DatasetLifecycle
	After     string
	Limit     int
}

// DatasetPatch changes a dataset's descriptive fields; nil leaves one as
// it is.
type DatasetPatch struct {
	Description *string
	Tags        *[]string
	// Metadata replaces the dataset's metadata whole.
	Metadata       *map[string]string
	ProfileID      *string
	ProfileVersion *int
	// StorageConnectionID chooses the dataset's connection; "" clears it.
	StorageConnectionID *string
}

// ObjectRef names a catalog object by ID or by namespace and path key, and
// optionally one of its versions by asset ID or version number.
type ObjectRef struct {
	ObjectID  string
	Namespace string
	PathKey   string
	VersionID string
	Version   int
}

// ResolvedRef is what a reference named; empty IDs mean not found.
type ResolvedRef struct {
	ObjectID      string
	ObjectDeleted bool
	AssetID       string
}

// ImportCheckpoint is a manifest import's progress, committed with a batch
// of its members.
type ImportCheckpoint struct {
	ImportID string
	Owner    string
	Lines    int64
	Offset   int64
	// Rejected are the batch's rejected lines to record; RejectedCount
	// counts them all, which is more once the import's error list is full.
	Rejected      []model.ImportError
	RejectedCount int64
}

// SnapshotInput describes a snapshot to take of a dataset's draft.
type SnapshotInput struct {
	TenantID       string
	DatasetID      string
	VersionID      string
	IdempotencyKey string
	Actor          string
	Now            time.Time
	MaxMembers     int64
}

// ValidationBatch is one checkpointed batch of a validation's member pass:
// counters to add and findings to write, applied only if the job is still
// leased to Owner at From.
type ValidationBatch struct {
	JobID      string
	Owner      string
	From, To   int64
	Processed  int64
	Passed     int64
	Warnings   int64
	Failed     int64
	Skipped    int64
	Examined   int64
	BytesTotal int64
	BytesRead  int64
	RuleCounts map[string]model.RuleCount
	Findings   []model.Finding
	Now        time.Time
}

// FindingFilter selects a page of findings.
type FindingFilter struct {
	Severity model.Severity
	RuleID   string
	After    string
	Limit    int
}

// LineageQuery bounds a traversal.
type LineageQuery struct {
	// Upstream follows edges from a node to what it contains, came from or
	// was checked by; otherwise edges are followed backwards, to what
	// contains, imports or derives from it.
	Upstream bool
	MaxDepth int
	// MaxEdges caps how much of the graph one traversal reads.
	MaxEdges int
	After    string
	Limit    int
}

// DatasetJobStats are the gauges of the dataset worker's queues.
type DatasetJobStats struct {
	ImportsByState     map[string]int64
	ValidationsByState map[string]int64
	// OldestQueuedSeconds is the age of the oldest due job of either kind.
	OldestQueuedSeconds float64
	// Stuck counts running jobs whose lease ran out.
	Stuck int64
}
