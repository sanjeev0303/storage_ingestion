package model

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// Managed datasets (Phase 10): a dataset is a tenant's curated collection
// of catalog objects. Its draft membership changes freely; a snapshot
// fixes the draft as an immutable version of exact object versions, which
// validation jobs check against a versioned profile. Object bytes are
// never copied: members and versions reference catalog objects and their
// versions (assets).

var (
	ErrDatasetArchived        = errors.New("the dataset is archived")
	ErrDatasetTransition      = errors.New("the dataset's status does not allow this")
	ErrMemberNotFound         = errors.New("dataset member not found")
	ErrMemberExists           = errors.New("the object is already a member of the dataset")
	ErrDatasetVersionNotFound = errors.New("dataset version not found")
	ErrNoMembers              = errors.New("the dataset has no members")
	ErrUnresolvedMembers      = errors.New("some members have no version to snapshot")
	ErrSnapshotTooLarge       = errors.New("the dataset has more members than a snapshot may hold")
	ErrImportNotFound         = errors.New("manifest import not found")
	ErrImportConflict         = errors.New("idempotency key reused for a different manifest import")
	ErrImportState            = errors.New("the manifest import is not in a state that allows this")
	ErrProfileNotFound        = errors.New("validation profile not found")
	ErrInvalidProfile         = errors.New("invalid validation profile")
	ErrValidationNotFound     = errors.New("validation job not found")
	ErrValidationConflict     = errors.New("idempotency key reused for a different validation")
	ErrValidationState        = errors.New("the validation job is not in a state that allows this")
	ErrReportNotFound         = errors.New("validation report not found")
	ErrTooManyJobs            = errors.New("the organization has too many dataset jobs running")
	ErrLineageCycle           = errors.New("the lineage edge would create a cycle")
	ErrInvalidLineage         = errors.New("invalid lineage relationship")
)

// DatasetLifecycle is a managed dataset's status.
type DatasetLifecycle string

const (
	// LifecycleDraft: being assembled; nothing validated matches the draft.
	LifecycleDraft DatasetLifecycle = "DRAFT"
	// LifecycleValidating: a validation of the current version runs.
	LifecycleValidating DatasetLifecycle = "VALIDATING"
	// The outcomes of validating the current version, while the draft
	// still matches it.
	LifecycleReady             DatasetLifecycle = "READY"
	LifecycleReadyWithWarnings DatasetLifecycle = "READY_WITH_WARNINGS"
	LifecycleFailed            DatasetLifecycle = "FAILED"
	// LifecycleArchived is final: the dataset is read-only; its versions,
	// reports and lineage are kept.
	LifecycleArchived DatasetLifecycle = "ARCHIVED"
)

// datasetTransitions lists, for each status, the statuses it may move to.
//
//	DRAFT -> VALIDATING | ARCHIVED
//	VALIDATING -> READY | READY_WITH_WARNINGS | FAILED (the validation's outcome)
//	VALIDATING -> DRAFT (cancelled, failed to run, or the draft changed meanwhile)
//	READY | READY_WITH_WARNINGS | FAILED -> DRAFT (the draft changed) | VALIDATING | ARCHIVED
//
// VALIDATING cannot be archived: cancel the validation first.
var datasetTransitions = map[DatasetLifecycle][]DatasetLifecycle{
	LifecycleDraft:             {LifecycleValidating, LifecycleArchived},
	LifecycleValidating:        {LifecycleReady, LifecycleReadyWithWarnings, LifecycleFailed, LifecycleDraft},
	LifecycleReady:             {LifecycleDraft, LifecycleValidating, LifecycleArchived},
	LifecycleReadyWithWarnings: {LifecycleDraft, LifecycleValidating, LifecycleArchived},
	LifecycleFailed:            {LifecycleDraft, LifecycleValidating, LifecycleArchived},
}

// Valid reports whether s is a known status.
func (s DatasetLifecycle) Valid() bool {
	_, ok := datasetTransitions[s]
	return ok || s == LifecycleArchived
}

// CanTransitionDataset reports whether a dataset may move from -> to.
func CanTransitionDataset(from, to DatasetLifecycle) bool {
	return slices.Contains(datasetTransitions[from], to)
}

// TransitionSources are the statuses a dataset may move to `to` from, for
// conditional updates.
func TransitionSources(to DatasetLifecycle) []string {
	var from []string
	for s, targets := range datasetTransitions {
		if slices.Contains(targets, to) {
			from = append(from, string(s))
		}
	}
	slices.Sort(from)
	return from
}

// Derivation records what a derived dataset was made from: the exact
// source versions and, optionally, the transformation that made it.
type Derivation struct {
	SourceVersionIDs []string        `json:"source_version_ids"`
	Splits           []string        `json:"splits,omitempty"`
	Labels           []string        `json:"labels,omitempty"`
	Transformation   *Transformation `json:"transformation,omitempty"`
}

// Transformation names a process that turned inputs into a dataset. Its
// ConfigSHA256 identifies the configuration without storing it.
type Transformation struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	ConfigSHA256 string `json:"config_sha256,omitempty"`
}

// Splits a member may be assigned to.
var DatasetSplits = []string{"train", "validation", "test"}

// DatasetMember is one object in a dataset's draft.
type DatasetMember struct {
	ID        string
	TenantID  string
	DatasetID string
	ObjectID  string
	// AssetID pins one version; "" follows the object's current version
	// until a snapshot fixes it.
	AssetID  string
	Label    string
	Split    string
	Metadata map[string]string
	// Source says who added it: "api", "import:<id>", "derived:<version>".
	Source    string
	AddedBy   string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Read with the member: the object's namespace and path.
	Namespace string
	Path      string
}

// DuplicatePolicy says what adding an object that is already a member
// does.
type DuplicatePolicy string

const (
	// DuplicateReject refuses the addition.
	DuplicateReject DuplicatePolicy = "REJECT"
	// DuplicateSkip keeps the existing member as it is.
	DuplicateSkip DuplicatePolicy = "SKIP"
	// DuplicateReplace overwrites the member's version, label, split and
	// metadata.
	DuplicateReplace DuplicatePolicy = "REPLACE"
)

func (p DuplicatePolicy) Valid() bool {
	return p == DuplicateReject || p == DuplicateSkip || p == DuplicateReplace
}

// MemberChange is one resolved addition: the object (and version) are the
// tenant's, checked before it is written.
type MemberChange struct {
	// Line is the manifest line or request position it came from.
	Line     int64
	ObjectID string
	AssetID  string
	Label    string
	Split    string
	Metadata map[string]string
}

// MemberOutcome is what happened to one MemberChange.
type MemberOutcome string

const (
	MemberAdded     MemberOutcome = "ADDED"
	MemberUpdated   MemberOutcome = "UPDATED"
	MemberSkipped   MemberOutcome = "SKIPPED"
	MemberDuplicate MemberOutcome = "DUPLICATE"
)

// MemberResult is the outcome of adding members.
type MemberResult struct {
	Outcomes []MemberOutcome
	Dataset  Dataset
}

// MemberPage is one page of a dataset's members.
type MemberPage struct {
	Members []DatasetMember
	After   string
}

// Version validation outcomes.
const (
	VersionNotValidated = "NOT_VALIDATED"
)

// DatasetVersion is an immutable snapshot of a dataset.
type DatasetVersion struct {
	ID        string
	TenantID  string
	DatasetID string
	Version   int
	// Status is FINALIZED: a version is written whole or not at all.
	Status string
	// ValidationStatus is the latest finished validation's outcome.
	ValidationStatus string
	MemberCount      int64
	TotalBytes       int64
	ManifestSHA256   string
	ManifestFormat   string
	MembersRevision  int64
	ProfileID        string
	ProfileVersion   int
	Config           map[string]any
	ParentVersionID  string
	Derivation       *Derivation
	LatestReportID   string
	IdempotencyKey   string
	CreatedBy        string
	CreatedAt        time.Time
}

// ManifestFormatV1 names the canonical manifest a version's hash covers.
const ManifestFormatV1 = "dataset-manifest/v1"

// VersionMember is one member of a dataset version: an exact object
// version, with what was recorded about it when the snapshot was taken.
type VersionMember struct {
	Ordinal        int64
	ObjectID       string
	AssetID        string
	ObjectVersion  int
	Path           string
	Label          string
	Split          string
	SizeBytes      int64
	ContentType    string
	ChecksumSHA256 string
	Metadata       map[string]string
}

// CanonicalLine is the member's line in its version's canonical manifest:
// a JSON object with the keys below in this order, as Go's encoding/json
// writes it (metadata keys sorted, <, > and & escaped), then "\n". The
// manifest is the lines in ordinal order, and ordinals follow object IDs,
// so the same members always give the same bytes. A version's
// ManifestSHA256 is the SHA-256 of those bytes.
func (m VersionMember) CanonicalLine() []byte {
	line := struct {
		ObjectID    string            `json:"object_id"`
		VersionID   string            `json:"version_id"`
		Version     int               `json:"version"`
		Path        string            `json:"path"`
		SizeBytes   int64             `json:"size_bytes"`
		ContentType string            `json:"content_type"`
		SHA256      string            `json:"sha256"`
		Label       string            `json:"label"`
		Split       string            `json:"split"`
		Metadata    map[string]string `json:"metadata"`
	}{m.ObjectID, m.AssetID, m.ObjectVersion, m.Path, m.SizeBytes, m.ContentType, m.ChecksumSHA256, m.Label, m.Split, m.Metadata}
	if line.Metadata == nil {
		line.Metadata = map[string]string{}
	}
	// Strings and a map of strings: encoding cannot fail.
	b, _ := json.Marshal(line)
	return append(b, '\n')
}

// ImportState is a manifest import's state.
type ImportState string

const (
	ImportAwaitingUpload ImportState = "AWAITING_UPLOAD"
	ImportQueued         ImportState = "QUEUED"
	ImportRunning        ImportState = "RUNNING"
	ImportCompleted      ImportState = "COMPLETED"
	ImportFailed         ImportState = "FAILED"
	ImportCancelled      ImportState = "CANCELLED"
)

// Terminal reports whether the import has ended.
func (s ImportState) Terminal() bool {
	return s == ImportCompleted || s == ImportFailed || s == ImportCancelled
}

// Where a manifest import's bytes are.
const (
	ManifestSourceInline = "INLINE"
	ManifestSourceStaged = "STAGED"
)

// ManifestImport reads a JSONL manifest into a dataset's draft.
type ManifestImport struct {
	ID               string
	TenantID         string
	DatasetID        string
	State            ImportState
	Source           string
	IdempotencyKey   string
	ManifestKey      string
	ManifestETag     string
	ManifestBytes    int64
	ManifestSHA256   string
	DuplicatePolicy  DuplicatePolicy
	LinesRead        int64
	MembersAdded     int64
	MembersUpdated   int64
	MembersSkipped   int64
	LinesRejected    int64
	CheckpointLine   int64
	CheckpointOffset int64
	Attempts         int
	CancelRequested  bool
	LastError        string
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	StartedAt        *time.Time
	CompletedAt      *time.Time
}

// ImportError is a rejected manifest line.
type ImportError struct {
	Line   int64
	Code   string
	Detail string
}

// Codes of rejected manifest lines.
const (
	LineInvalidJSON      = "invalid_json"
	LineInvalidUTF8      = "invalid_utf8"
	LineTooLong          = "line_too_long"
	LineMissingReference = "missing_reference"
	LineInvalidReference = "invalid_reference"
	LineInvalidPath      = "invalid_path"
	LineObjectNotFound   = "object_not_found"
	LineVersionNotFound  = "version_not_found"
	LineInvalidSplit     = "invalid_split"
	LineInvalidLabel     = "invalid_label"
	LineInvalidMetadata  = "invalid_metadata"
	LineDuplicate        = "duplicate_member"
	LineDuplicateInFile  = "duplicate_in_manifest"
)

// ─── Validation ───────────────────────────────────────────────────────

// Severity of a validation rule and its findings.
type Severity string

const (
	SeverityError   Severity = "ERROR"
	SeverityWarning Severity = "WARNING"
	SeverityInfo    Severity = "INFO"
)

func (s Severity) Valid() bool {
	return s == SeverityError || s == SeverityWarning || s == SeverityInfo
}

// RuleScope is what a rule looks at.
type RuleScope string

const (
	RuleScopeDataset        RuleScope = "DATASET"
	RuleScopeObjectMetadata RuleScope = "OBJECT_METADATA"
	RuleScopeObjectContent  RuleScope = "OBJECT_CONTENT"
	RuleScopeSplit          RuleScope = "SPLIT"
)

// Rule is one check of a validation profile.
type Rule struct {
	ID       string          `json:"id"`
	Version  int             `json:"version"`
	Type     string          `json:"type"`
	Severity Severity        `json:"severity"`
	Enabled  bool            `json:"enabled"`
	Scope    RuleScope       `json:"scope"`
	Config   json.RawMessage `json:"config,omitempty"`
}

// Thresholds turn finding counts into an outcome. More ERROR findings
// than MaxErrors, or more WARNING findings than MaxWarnings when it is
// set, fail the version. Otherwise any ERROR or WARNING finding makes it
// READY_WITH_WARNINGS, and none makes it READY. INFO findings never
// count.
type Thresholds struct {
	MaxErrors   int64  `json:"max_errors"`
	MaxWarnings *int64 `json:"max_warnings,omitempty"`
}

// Outcome is the status these thresholds give the finding counts.
func (t Thresholds) Outcome(errors, warnings int64) DatasetLifecycle {
	if errors > t.MaxErrors || (t.MaxWarnings != nil && warnings > *t.MaxWarnings) {
		return LifecycleFailed
	}
	if errors > 0 || warnings > 0 {
		return LifecycleReadyWithWarnings
	}
	return LifecycleReady
}

// ProfileDefinition is a profile's rules and thresholds.
type ProfileDefinition struct {
	// Kind names what the profile is for (image, audio, ...), for people.
	Kind       string     `json:"kind"`
	Rules      []Rule     `json:"rules"`
	Thresholds Thresholds `json:"thresholds"`
}

// ValidationProfile is a versioned set of rules. Built-in profiles have
// no tenant.
type ValidationProfile struct {
	ID          string
	Version     int
	TenantID    string
	Name        string
	Description string
	Definition  ProfileDefinition
	BuiltIn     bool
	CreatedBy   string
	CreatedAt   time.Time
}

// ValidationMode is how much of a dataset a validation reads.
type ValidationMode string

const (
	// ModeMetadata reads catalog records only, never object bytes.
	ModeMetadata ValidationMode = "METADATA"
	// ModeFull checks every member's content integrity.
	ModeFull ValidationMode = "FULL"
	// ModeSampled checks content for a bounded, deterministic sample.
	ModeSampled ValidationMode = "SAMPLED"
)

func (m ValidationMode) Valid() bool {
	return m == ModeMetadata || m == ModeFull || m == ModeSampled
}

// ValidationState is a validation job's state.
type ValidationState string

const (
	ValidationQueued                ValidationState = "QUEUED"
	ValidationRunning               ValidationState = "RUNNING"
	ValidationCompleted             ValidationState = "COMPLETED"
	ValidationCompletedWithWarnings ValidationState = "COMPLETED_WITH_WARNINGS"
	// ValidationFailed: the version failed validation (the job has a
	// report), or the job could not run (no report, LastError says why).
	ValidationFailed    ValidationState = "FAILED"
	ValidationCancelled ValidationState = "CANCELLED"
)

// Terminal reports whether the job has ended.
func (s ValidationState) Terminal() bool {
	return s != ValidationQueued && s != ValidationRunning
}

// ValidationJob checks one dataset version against one profile version.
type ValidationJob struct {
	ID               string
	TenantID         string
	DatasetID        string
	DatasetVersionID string
	ProfileID        string
	ProfileVersion   int
	Profile          ProfileDefinition
	Mode             ValidationMode
	SampleSize       int
	Readiness        bool
	State            ValidationState
	TotalItems       int64
	ProcessedItems   int64
	PassedItems      int64
	WarningItems     int64
	FailedItems      int64
	ContentSkipped   int64
	ContentExamined  int64
	BytesTotal       int64
	BytesRead        int64
	// RuleCounts are the member pass's per-rule outcome counts.
	RuleCounts        map[string]RuleCount
	CheckpointOrdinal int64
	Attempts          int
	MaxAttempts       int
	CancelRequested   bool
	LastError         string
	ReportID          string
	IdempotencyKey    string
	CreatedBy         string
	CreatedAt         time.Time
	StartedAt         *time.Time
	CompletedAt       *time.Time
	UpdatedAt         time.Time
}

// RuleCount is how a rule fared over the members it was applied to.
type RuleCount struct {
	Checked int64 `json:"checked"`
	Failed  int64 `json:"failed"`
	// Skipped members were not assessed by the rule.
	Skipped int64 `json:"skipped"`
	// Findings counts the findings stored, at most the per-rule maximum.
	Findings int64 `json:"findings"`
	// Read counts members whose bytes a content check read.
	Read int64 `json:"read,omitempty"`
}

// Finding is one problem a validation found.
type Finding struct {
	// ID is stable for the job: the same problem found twice is one row.
	ID          string
	JobID       string
	TenantID    string
	RuleID      string
	RuleVersion int
	Severity    Severity
	Code        string
	ObjectID    string
	AssetID     string
	Message     string
	Expected    string
	Observed    string
	Evidence    string
	Remediation string
	CreatedAt   time.Time
}

// MemberEvidence is what a validation knows about one version member: what
// the snapshot recorded, and the object version's state and processing
// evidence now.
type MemberEvidence struct {
	VersionMember
	ObjectExists  bool
	ObjectDeleted bool
	AssetStatus   AssetStatus
	// AssetSizeBytes and AssetChecksum are the asset's own records.
	AssetSizeBytes int64
	AssetChecksum  string
	// ObjectKey is where the version's bytes are; it never leaves the
	// service.
	ObjectKey string
	// From the version's processing job: PASSED or FAILED (or "" when there
	// is no record), DECLARED or COMPUTED, and the scan's result.
	VerificationStatus string
	ChecksumSource     string
	ScanResult         string
}

// Count is one value of a distribution.
type Count struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// ChecksumGroup is a set of members with the same verified content.
type ChecksumGroup struct {
	Checksum  string   `json:"checksum_sha256"`
	Members   int64    `json:"members"`
	ObjectIDs []string `json:"object_ids"`
	Splits    []string `json:"splits"`
}

// VersionAggregates are a version's distributions and verified-content
// groups, computed by the database.
type VersionAggregates struct {
	Members int64
	// DistinctAssets counts the distinct object versions referenced.
	DistinctAssets int64
	TotalBytes     int64
	MinSize        int64
	MaxSize        int64
	Splits         []Count
	Labels         []Count
	LabelCount     int64
	Unlabeled      int64
	ContentTypes   []Count
	Extensions     []Count
	// VerifiedChecksums counts members whose recorded checksum processing
	// verified against the stored bytes; only those take part in
	// duplicate and leakage groups.
	VerifiedChecksums int64
	Duplicates        []ChecksumGroup
	DuplicateGroups   int64
	Leaks             []ChecksumGroup
	LeakGroups        int64
	// The evidence behind the members now: processing verification
	// results, malware scan verdicts and version states, each counted.
	Verification  []Count
	ScanResults   []Count
	AssetStatuses []Count
}

// FindingPage is one page of a report's findings.
type FindingPage struct {
	Findings []Finding
	After    string
}

// ReportSchemaVersion is the version of the report body's layout.
const ReportSchemaVersion = 1

// ValidationReport is the durable outcome of a finished validation job.
type ValidationReport struct {
	ID               string
	TenantID         string
	JobID            string
	DatasetID        string
	DatasetVersionID string
	ProfileID        string
	ProfileVersion   int
	Mode             ValidationMode
	Status           DatasetLifecycle
	Readiness        bool
	SchemaVersion    int
	Body             json.RawMessage
	Summary          string
	StartedAt        time.Time
	CompletedAt      time.Time
}

// ─── Lineage ──────────────────────────────────────────────────────────

// LineageKind is what a lineage node stands for.
type LineageKind string

const (
	NodeObjectVersion    LineageKind = "OBJECT_VERSION"
	NodeDataset          LineageKind = "DATASET"
	NodeDatasetVersion   LineageKind = "DATASET_VERSION"
	NodeManifestImport   LineageKind = "MANIFEST_IMPORT"
	NodeValidationReport LineageKind = "VALIDATION_REPORT"
	NodeTransformation   LineageKind = "TRANSFORMATION"
)

// LineageRelation is an edge's meaning, read "from <relation> to".
type LineageRelation string

const (
	RelContains            LineageRelation = "CONTAINS"
	RelDerivedFrom         LineageRelation = "DERIVED_FROM"
	RelImportedFrom        LineageRelation = "IMPORTED_FROM"
	RelValidatedBy         LineageRelation = "VALIDATED_BY"
	RelCreatedFromSnapshot LineageRelation = "CREATED_FROM_SNAPSHOT"
	RelTransformedBy       LineageRelation = "TRANSFORMED_BY"
)

// lineageRules are the node kinds each relation may connect.
var lineageRules = map[LineageRelation][2][]LineageKind{
	RelContains:            {{NodeDataset, NodeDatasetVersion}, {NodeDatasetVersion, NodeObjectVersion}},
	RelDerivedFrom:         {{NodeDatasetVersion}, {NodeDatasetVersion}},
	RelImportedFrom:        {{NodeDataset}, {NodeManifestImport}},
	RelValidatedBy:         {{NodeDatasetVersion}, {NodeValidationReport}},
	RelCreatedFromSnapshot: {{NodeDataset}, {NodeDatasetVersion}},
	RelTransformedBy:       {{NodeDataset, NodeDatasetVersion}, {NodeTransformation}},
}

// ValidLineage reports whether relation may connect a from node to a to
// node.
func ValidLineage(relation LineageRelation, from, to LineageKind) bool {
	kinds, ok := lineageRules[relation]
	return ok && slices.Contains(kinds[0], from) && slices.Contains(kinds[1], to)
}

// Acyclic reports whether edges of this relation must never form a cycle
// (provenance: nothing derives from itself).
func (r LineageRelation) Acyclic() bool {
	return r == RelDerivedFrom || r == RelCreatedFromSnapshot || r == RelContains
}

// LineageRef names a record in the lineage graph.
type LineageRef struct {
	Kind  LineageKind
	RefID string
}

// LineageEdge is one edge, as a traversal found it.
type LineageEdge struct {
	From      LineageRef
	To        LineageRef
	Relation  LineageRelation
	Actor     string
	JobID     string
	Details   map[string]any
	CreatedAt time.Time
	// Depth is how many edges from the start the edge's far end is.
	Depth int
}

// LineagePage is one page of a traversal.
type LineagePage struct {
	Edges []LineageEdge
	After string
	// Truncated: the traversal reached its depth limit with edges beyond.
	Truncated bool
}
