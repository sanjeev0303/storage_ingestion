// Package dataset manages datasets as curated, versioned collections of
// catalog objects (Phase 10): their drafts, immutable snapshots, manifest
// imports, validation against versioned profiles, readiness reports and
// lineage. Every operation is authorized here, for the caller's tenant
// only; the dataset worker runs imports and validations with the tenant
// and actor recorded on the job, never with anything a manifest says.
package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/concurrency"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/config"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Errors the API maps to client responses.
var (
	ErrInvalidRequest     = errors.New("invalid request")
	ErrRevisionRequired   = errors.New("the dataset's revision is required: send If-Match or revision")
	ErrManifestTooLarge   = errors.New("the manifest is larger than allowed")
	ErrManifestNotStaged  = errors.New("the manifest has not been uploaded")
	ErrManifestChecksum   = errors.New("the manifest does not match its declared sha256")
	ErrContentUnavailable = errors.New("object content cannot be read by this service")
)

// ManifestStore holds staged manifests: clients upload them with a
// presigned PUT and the worker reads them in ranges.
type ManifestStore interface {
	CreateUploadPlan(ctx context.Context, target interfaces.UploadTarget) (interfaces.UploadPlan, error)
	HeadObject(ctx context.Context, bucket, objectKey string) (interfaces.ObjectMetadata, error)
	OpenRange(ctx context.Context, bucket, objectKey string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error)
}

// ContentReader streams object versions' bytes, for content checks: a
// recorded provider version is read by its ID, never by the key alone.
type ContentReader interface {
	OpenRange(ctx context.Context, bucket, objectKey string, first, last int64, cond interfaces.ReadCondition) (io.ReadCloser, error)
}

// Quotas gives a tenant's effective quota.
type Quotas interface {
	Cached(ctx context.Context, tenantID string) (model.TenantQuota, error)
}

// Dependencies are the collaborators of the dataset service.
type Dependencies struct {
	Repo interfaces.DatasetRepository
	// Manifests and ManifestBucket hold staged manifests.
	Manifests      ManifestStore
	ManifestBucket string
	// Content and ContentBucket read published object versions. Without
	// them, content checks are reported as not assessed.
	Content       ContentReader
	ContentBucket string
	// Router reads members stored on tenants' storage connections; without
	// it they are not examined.
	Router interfaces.StorageRouter
	// Packs finds members whose bytes were packed; nil when nothing is.
	Packs interfaces.PackLocator
	// Quotas, when set, lowers the manifest limits for tenants with
	// smaller quotas.
	Quotas  Quotas
	Audit   interfaces.AuditRecorder
	Metrics *metrics.Registry
	Log     *slog.Logger
}

// Service runs managed datasets.
type Service struct {
	deps    Dependencies
	cfg     config.Datasets
	content *concurrency.Limiter
	retry   retry.Policy
	now     func() time.Time
}

func NewService(deps Dependencies, cfg config.Datasets) (*Service, error) {
	if deps.Repo == nil || deps.Manifests == nil || deps.ManifestBucket == "" || deps.Log == nil {
		return nil, errors.New("dataset service dependencies are required")
	}
	return &Service{
		deps: deps, cfg: cfg,
		content: concurrency.NewLimiter(cfg.ContentConcurrency),
		retry:   retry.Policy{BaseDelay: cfg.BackoffBase, MaxDelay: cfg.BackoffMax, MaxAttempts: cfg.MaxAttempts},
		now:     func() time.Time { return time.Now().UTC() },
	}, nil
}

func (s *Service) count(name string, n int64) {
	if s.deps.Metrics != nil {
		s.deps.Metrics.Add(name, n)
	}
}

// authorize checks p's permission; a refusal is recorded in the audit log.
func (s *Service) authorize(ctx context.Context, p model.Principal, perm model.Permission, action, resourceType, resourceID string) error {
	return audit.Require(ctx, s.deps.Audit, p, perm, action, resourceType, resourceID)
}

// record audits a change p made.
func (s *Service) record(ctx context.Context, p model.Principal, action, resourceType, resourceID string, details map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	e := audit.From(ctx, p, action, resourceType, resourceID)
	e.Details = details
	s.deps.Audit.Record(ctx, e)
}

// pageSize is the page size a listing uses for the requested limit.
func (s *Service) pageSize(limit int) (int, error) {
	switch {
	case limit == 0:
		return s.cfg.DefaultPageSize, nil
	case limit < 0 || limit > s.cfg.MaxPageSize:
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidRequest, s.cfg.MaxPageSize)
	}
	return limit, nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == unicode.ReplacementChar })
}

var tagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// memberMetadataLimits keep a member's metadata within its column.
var memberMetadataLimits = model.MetadataLimits{MaxKeys: 32, MaxBytes: 4096, MaxValueBytes: 1024}

// ─── Datasets ─────────────────────────────────────────────────────────

// CreateInput describes a dataset. CompletionPolicy and MaxBytes are its
// bulk-ingestion settings (Phase 7). A derived dataset names the exact
// versions it is made from.
type CreateInput struct {
	Name             string
	Description      string
	Tags             []string
	Metadata         map[string]string
	ProfileID        string
	ProfileVersion   int
	CompletionPolicy model.CompletionPolicy
	MaxBytes         int64

	SourceVersionIDs []string
	Splits           []string
	Labels           []string
	Transformation   *TransformationInput
}

// TransformationInput names the process that made a derived dataset. Its
// configuration is hashed, not stored.
type TransformationInput struct {
	Name    string
	Version string
	Config  map[string]any
}

// Create creates the caller's dataset, or returns the existing one of that
// name when its settings match. A derived dataset's draft starts as the
// members of its source versions (optionally only some splits and
// labels), each pinned to its exact version.
func (s *Service) Create(ctx context.Context, p model.Principal, in CreateInput) (model.Dataset, bool, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.create", "dataset", in.Name); err != nil {
		return model.Dataset{}, false, err
	}
	if !model.ValidDataset(in.Name) {
		return model.Dataset{}, false, fmt.Errorf("%w: name must be 1-64 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit", ErrInvalidRequest)
	}
	if in.CompletionPolicy == "" {
		in.CompletionPolicy = model.CompleteAll
	}
	if !in.CompletionPolicy.Valid() || in.MaxBytes < 0 {
		return model.Dataset{}, false, fmt.Errorf("%w: completion_policy must be ALL, REQUIRED or PARTIAL and max_bytes not negative", ErrInvalidRequest)
	}
	if err := validateDescriptive(in.Description, in.Tags, in.Metadata); err != nil {
		return model.Dataset{}, false, err
	}
	if in.ProfileID != "" {
		if _, err := s.profile(ctx, p.TenantID, in.ProfileID, in.ProfileVersion); err != nil {
			return model.Dataset{}, false, err
		}
	}
	derivation, err := s.derivation(ctx, p, in)
	if err != nil {
		return model.Dataset{}, false, err
	}

	now := s.now()
	d, created, err := s.deps.Repo.CreateDataset(ctx, model.Dataset{
		ID: event.NewID(), TenantID: p.TenantID, Name: in.Name, CompletionPolicy: in.CompletionPolicy, MaxBytes: in.MaxBytes,
		Description: in.Description, Tags: in.Tags, Metadata: in.Metadata, ProfileID: in.ProfileID, ProfileVersion: in.ProfileVersion,
		Derivation: derivation, CreatedBy: p.UserID, CreatedAt: now, UpdatedAt: now,
	})
	if err == nil && created {
		details := map[string]any{"name": d.Name}
		if derivation != nil {
			details["source_versions"] = derivation.SourceVersionIDs
			details["members"] = d.MemberCount
		}
		s.record(ctx, p, "dataset.create", "dataset", d.ID, details)
		s.count("dataset_created", 1)
	}
	return d, created, err
}

// derivation checks a derived dataset's sources: versions of the
// caller's tenant, at most 16.
func (s *Service) derivation(ctx context.Context, p model.Principal, in CreateInput) (*model.Derivation, error) {
	if len(in.SourceVersionIDs) == 0 {
		if in.Transformation != nil || len(in.Splits) > 0 || len(in.Labels) > 0 {
			return nil, fmt.Errorf("%w: a transformation, splits or labels need source_version_ids", ErrInvalidRequest)
		}
		return nil, nil
	}
	if len(in.SourceVersionIDs) > 16 || len(in.Labels) > 100 {
		return nil, fmt.Errorf("%w: at most 16 source versions and 100 labels", ErrInvalidRequest)
	}
	for _, id := range in.SourceVersionIDs {
		v, err := s.deps.Repo.GetVersion(ctx, p.TenantID, id)
		if err != nil {
			return nil, err
		}
		if s.cfg.MaxSnapshotMembers > 0 && v.MemberCount > s.cfg.MaxSnapshotMembers {
			return nil, model.ErrSnapshotTooLarge
		}
	}
	for _, split := range in.Splits {
		if !slices.Contains(model.DatasetSplits, split) {
			return nil, fmt.Errorf("%w: split %q is not train, validation or test", ErrInvalidRequest, split)
		}
	}
	d := &model.Derivation{SourceVersionIDs: in.SourceVersionIDs, Splits: in.Splits, Labels: in.Labels}
	if t := in.Transformation; t != nil {
		if t.Name == "" || len(t.Name) > 128 || len(t.Version) > 64 || hasControl(t.Name+t.Version) {
			return nil, fmt.Errorf("%w: a transformation needs a name (1-128 characters) and a version of at most 64", ErrInvalidRequest)
		}
		// Map keys are sorted when encoded: the same configuration always
		// hashes the same.
		raw, err := json.Marshal(t.Config)
		if err != nil || len(raw) > 16*1024 {
			return nil, fmt.Errorf("%w: the transformation config must be a JSON object of at most 16 KiB", ErrInvalidRequest)
		}
		sum := sha256.Sum256(raw)
		d.Transformation = &model.Transformation{Name: t.Name, Version: t.Version, ConfigSHA256: hex.EncodeToString(sum[:])}
	}
	return d, nil
}

func validateDescriptive(description string, tags []string, metadata map[string]string) error {
	if len(description) > 2000 {
		return fmt.Errorf("%w: description is at most 2000 characters", ErrInvalidRequest)
	}
	if len(tags) > 32 {
		return fmt.Errorf("%w: at most 32 tags", ErrInvalidRequest)
	}
	for _, t := range tags {
		if !tagPattern.MatchString(t) {
			return fmt.Errorf("%w: tag %q: 1-64 characters of letters, digits, '.', '_', ':' and '-'", ErrInvalidRequest, t)
		}
	}
	if err := model.ValidateUserMetadata(metadata, model.DefaultMetadataLimits); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return nil
}

func (s *Service) Get(ctx context.Context, p model.Principal, datasetID string) (model.Dataset, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", datasetID); err != nil {
		return model.Dataset{}, err
	}
	return s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID)
}

// Page is one page of a listing and the cursor of the next.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// List pages the caller's datasets by name, optionally of one status.
func (s *Service) List(ctx context.Context, p model.Principal, status, cursor string, limit int) (Page[model.Dataset], error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", ""); err != nil {
		return Page[model.Dataset]{}, err
	}
	lifecycle := model.DatasetLifecycle(strings.ToUpper(status))
	if status != "" && !lifecycle.Valid() {
		return Page[model.Dataset]{}, fmt.Errorf("%w: status must be one of DRAFT, VALIDATING, READY, READY_WITH_WARNINGS, FAILED, ARCHIVED", ErrInvalidRequest)
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return Page[model.Dataset]{}, err
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return Page[model.Dataset]{}, err
	}
	items, err := s.deps.Repo.ListDatasets(ctx, p.TenantID, interfaces.DatasetFilter{Lifecycle: lifecycle, After: after, Limit: size + 1})
	if err != nil {
		return Page[model.Dataset]{}, err
	}
	page := Page[model.Dataset]{Items: items}
	if len(items) > size {
		page.Items = items[:size]
		page.NextCursor = encodeCursor(page.Items[size-1].Name)
	}
	return page, nil
}

// UpdateInput changes a dataset's descriptive fields; nil leaves a field
// as it is. Metadata is a merge patch: a nil value removes the key.
type UpdateInput struct {
	Description    *string
	Tags           *[]string
	Metadata       map[string]*string
	ProfileID      *string
	ProfileVersion *int
	// StorageConnectionID sends the dataset's new uploads to a storage
	// connection ("" returns them to the default). Needs storage.use.
	StorageConnectionID *string
}

// Update changes a dataset if it is still at revision.
func (s *Service) Update(ctx context.Context, p model.Principal, datasetID string, revision int64, in UpdateInput) (model.Dataset, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.update", "dataset", datasetID); err != nil {
		return model.Dataset{}, err
	}
	if revision <= 0 {
		return model.Dataset{}, ErrRevisionRequired
	}
	current, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID)
	if err != nil {
		return model.Dataset{}, err
	}
	if in.StorageConnectionID != nil {
		if err := s.authorize(ctx, p, model.PermStorageUse, "dataset.update", "dataset", datasetID); err != nil {
			return model.Dataset{}, err
		}
	}
	patch := interfaces.DatasetPatch{Description: in.Description, Tags: in.Tags, ProfileID: in.ProfileID, ProfileVersion: in.ProfileVersion,
		StorageConnectionID: in.StorageConnectionID}
	description, tags := current.Description, current.Tags
	if in.Description != nil {
		description = *in.Description
	}
	if in.Tags != nil {
		tags = *in.Tags
	}
	metadata := current.Metadata
	if in.Metadata != nil {
		metadata = maps.Clone(current.Metadata)
		if metadata == nil {
			metadata = map[string]string{}
		}
		for k, v := range in.Metadata {
			if v == nil {
				delete(metadata, k)
			} else {
				metadata[k] = *v
			}
		}
		patch.Metadata = &metadata
	}
	if err := validateDescriptive(description, tags, metadata); err != nil {
		return model.Dataset{}, err
	}
	if in.ProfileID != nil && *in.ProfileID != "" {
		version := current.ProfileVersion
		if in.ProfileVersion != nil {
			version = *in.ProfileVersion
		}
		if _, err := s.profile(ctx, p.TenantID, *in.ProfileID, version); err != nil {
			return model.Dataset{}, err
		}
	}
	d, err := s.deps.Repo.UpdateDataset(ctx, p.TenantID, datasetID, revision, patch, p.UserID, s.now())
	if err == nil {
		s.record(ctx, p, "dataset.update", "dataset", datasetID, map[string]any{"revision": d.Revision})
	}
	return d, err
}

// Archive makes a dataset read-only for good. Its versions, reports and
// lineage stay; imports still waiting are cancelled.
func (s *Service) Archive(ctx context.Context, p model.Principal, datasetID string) (model.Dataset, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsManage, "dataset.archive", "dataset", datasetID); err != nil {
		return model.Dataset{}, err
	}
	d, err := s.deps.Repo.ArchiveDataset(ctx, p.TenantID, datasetID, p.UserID, s.now())
	if err == nil {
		s.record(ctx, p, "dataset.archive", "dataset", datasetID, map[string]any{"versions": d.NextVersion - 1})
		s.count("dataset_archived", 1)
	}
	return d, err
}

// ─── Membership ───────────────────────────────────────────────────────

// MemberInput adds one catalog object: by ID, or by namespace and path;
// pinned to a version (asset ID, or version number) or following the
// current version.
type MemberInput struct {
	ObjectID  string            `json:"object_id"`
	Namespace string            `json:"namespace"`
	Path      string            `json:"path"`
	VersionID string            `json:"version_id"`
	Version   int               `json:"version"`
	Label     string            `json:"label"`
	Split     string            `json:"split"`
	Metadata  map[string]string `json:"metadata"`
}

// parsed is a checked member input: its catalog reference and the change
// it becomes once resolved, or why it was refused.
type parsed struct {
	ref    interfaces.ObjectRef
	change model.MemberChange
	code   string
	detail string
}

// parseMember checks one member input's own fields. References are
// resolved against the catalog afterwards.
func parseMember(in MemberInput) parsed {
	p := parsed{change: model.MemberChange{Label: in.Label, Split: in.Split, Metadata: in.Metadata}}
	switch {
	case in.ObjectID == "" && in.Path == "":
		p.code, p.detail = model.LineMissingReference, "object_id or path is required"
		return p
	case in.ObjectID != "" && in.Path != "":
		p.code, p.detail = model.LineInvalidReference, "give object_id or path, not both"
		return p
	case in.VersionID != "" && in.Version != 0:
		p.code, p.detail = model.LineInvalidReference, "give version_id or version, not both"
		return p
	case in.Version < 0 || len(in.ObjectID) > 128 || len(in.VersionID) > 128:
		p.code, p.detail = model.LineInvalidReference, "the reference is malformed"
		return p
	}
	p.ref = interfaces.ObjectRef{ObjectID: in.ObjectID, VersionID: in.VersionID, Version: in.Version}
	if in.Path != "" {
		if in.Namespace != "" && !model.ValidDataset(in.Namespace) {
			p.code, p.detail = model.LineInvalidReference, "namespace is not a valid dataset namespace"
			return p
		}
		// The catalog's own normalization: "..", control characters and
		// absolute paths are refused, case and Unicode forms folded.
		_, key, err := model.NormalizePath(in.Path, model.DefaultPathPolicy)
		if err != nil {
			p.code, p.detail = model.LineInvalidPath, clip(err.Error())
			return p
		}
		p.ref.Namespace, p.ref.PathKey = in.Namespace, key
	}
	if len(in.Label) > 256 || hasControl(in.Label) {
		p.code, p.detail = model.LineInvalidLabel, "labels are at most 256 characters, without control characters"
		return p
	}
	if in.Split != "" && !slices.Contains(model.DatasetSplits, in.Split) {
		p.code, p.detail = model.LineInvalidSplit, "split must be train, validation or test"
		return p
	}
	if err := model.ValidateUserMetadata(in.Metadata, memberMetadataLimits); err != nil {
		p.code, p.detail = model.LineInvalidMetadata, clip(err.Error())
		return p
	}
	return p
}

// resolve looks the parsed inputs' references up in the tenant's catalog
// and fills in their objects and versions; one that names nothing of the
// tenant's is refused, as if it did not exist.
func (s *Service) resolve(ctx context.Context, tenantID string, items []parsed) error {
	var refs []interfaces.ObjectRef
	var at []int
	for i, it := range items {
		if it.code == "" {
			refs = append(refs, it.ref)
			at = append(at, i)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	resolved, err := s.deps.Repo.ResolveObjects(ctx, tenantID, refs)
	if err != nil {
		return err
	}
	for j, r := range resolved {
		it := &items[at[j]]
		ref := refs[j]
		switch {
		case r.ObjectID == "":
			it.code, it.detail = model.LineObjectNotFound, "no such object"
		case r.ObjectDeleted:
			it.code, it.detail = model.LineObjectNotFound, "the object was deleted"
		case (ref.VersionID != "" || ref.Version > 0) && r.AssetID == "":
			it.code, it.detail = model.LineVersionNotFound, "the object has no such version"
		default:
			it.change.ObjectID, it.change.AssetID = r.ObjectID, r.AssetID
		}
	}
	return nil
}

// dedupe keeps one change per object within a batch, as the duplicate
// policy says: REPLACE keeps the last, SKIP and REJECT the first. The
// others are skipped (SKIP, REPLACE) or refused (REJECT).
func dedupe(items []parsed, policy model.DuplicatePolicy) (keep []int, dropped map[int]bool) {
	dropped = map[int]bool{}
	last := map[string]int{}
	for i, it := range items {
		if it.code != "" {
			continue
		}
		prev, seen := last[it.change.ObjectID]
		switch {
		case !seen:
			last[it.change.ObjectID] = i
		case policy == model.DuplicateReplace:
			dropped[prev] = true
			last[it.change.ObjectID] = i
		default:
			dropped[i] = true
		}
	}
	for i, it := range items {
		if it.code == "" && !dropped[i] {
			keep = append(keep, i)
		}
	}
	return keep, dropped
}

// MemberOutcome is what happened to one requested member.
type MemberOutcome struct {
	Index    int
	Outcome  string
	ObjectID string
	AssetID  string
	Code     string
	Error    string
}

// AddResult is the outcome of adding members.
type AddResult struct {
	Dataset  model.Dataset
	Outcomes []MemberOutcome
	Added    int
	Updated  int
	Skipped  int
	Rejected int
}

// AddMembers adds catalog objects to a dataset's draft. Each input is
// judged on its own: bad or unknown references are refused without
// failing the others.
func (s *Service) AddMembers(ctx context.Context, p model.Principal, datasetID string, inputs []MemberInput, policy model.DuplicatePolicy) (AddResult, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.members_add", "dataset", datasetID); err != nil {
		return AddResult{}, err
	}
	if policy == "" {
		policy = model.DuplicateReject
	}
	if !policy.Valid() {
		return AddResult{}, fmt.Errorf("%w: on_duplicate must be REJECT, SKIP or REPLACE", ErrInvalidRequest)
	}
	if len(inputs) == 0 || len(inputs) > s.cfg.MaxBulkMembers {
		return AddResult{}, fmt.Errorf("%w: add 1 to %d members per request", ErrInvalidRequest, s.cfg.MaxBulkMembers)
	}
	d, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID)
	if err != nil {
		return AddResult{}, err
	}
	if d.Lifecycle == model.LifecycleArchived {
		return AddResult{}, model.ErrDatasetArchived
	}

	items := make([]parsed, len(inputs))
	for i, in := range inputs {
		items[i] = parseMember(in)
		items[i].change.Line = int64(i)
	}
	if err := s.resolve(ctx, p.TenantID, items); err != nil {
		return AddResult{}, err
	}
	keep, dropped := dedupe(items, policy)
	changes := make([]model.MemberChange, len(keep))
	for j, i := range keep {
		changes[j] = items[i].change
	}

	result := AddResult{Dataset: d, Outcomes: make([]MemberOutcome, len(items))}
	if len(changes) > 0 {
		written, err := s.deps.Repo.AddMembers(ctx, p.TenantID, datasetID, changes, policy, "api", p.UserID, s.now(), nil)
		if err != nil {
			return AddResult{}, err
		}
		if written.Dataset.ID != "" {
			result.Dataset = written.Dataset
		}
		for j, i := range keep {
			result.Outcomes[i] = MemberOutcome{Outcome: string(written.Outcomes[j])}
		}
	}
	for i, it := range items {
		o := &result.Outcomes[i]
		o.Index, o.ObjectID, o.AssetID = i, it.change.ObjectID, it.change.AssetID
		switch {
		case it.code != "":
			o.Outcome, o.Code, o.Error = "REJECTED", it.code, it.detail
		case dropped[i] && policy == model.DuplicateReject:
			o.Outcome, o.Code, o.Error = "REJECTED", model.LineDuplicateInFile, "the object appears more than once in the request"
		case dropped[i]:
			o.Outcome = string(model.MemberSkipped)
		case o.Outcome == string(model.MemberDuplicate):
			o.Outcome, o.Code, o.Error = "REJECTED", model.LineDuplicate, "the object is already a member"
		}
		switch o.Outcome {
		case string(model.MemberAdded):
			result.Added++
		case string(model.MemberUpdated):
			result.Updated++
		case string(model.MemberSkipped):
			result.Skipped++
		default:
			result.Rejected++
		}
	}
	if result.Added+result.Updated > 0 {
		s.record(ctx, p, "dataset.members_add", "dataset", datasetID, map[string]any{
			"added": result.Added, "updated": result.Updated, "skipped": result.Skipped, "rejected": result.Rejected})
		s.count("dataset_members_added", int64(result.Added))
	}
	return result, nil
}

// RemoveMember takes a member out of the draft. Versions that hold the
// object keep it.
func (s *Service) RemoveMember(ctx context.Context, p model.Principal, datasetID, memberID string) (model.Dataset, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.members_remove", "dataset", datasetID); err != nil {
		return model.Dataset{}, err
	}
	d, err := s.deps.Repo.RemoveMember(ctx, p.TenantID, datasetID, memberID, p.UserID, s.now())
	if err == nil {
		s.record(ctx, p, "dataset.members_remove", "dataset", datasetID, map[string]any{"member_id": memberID})
	}
	return d, err
}

// Members pages a dataset's draft.
func (s *Service) Members(ctx context.Context, p model.Principal, datasetID, cursor string, limit int) (Page[model.DatasetMember], error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", datasetID); err != nil {
		return Page[model.DatasetMember]{}, err
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return Page[model.DatasetMember]{}, err
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return Page[model.DatasetMember]{}, err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return Page[model.DatasetMember]{}, err
	}
	members, err := s.deps.Repo.ListMembers(ctx, p.TenantID, datasetID, after, size)
	if err != nil {
		return Page[model.DatasetMember]{}, err
	}
	page := Page[model.DatasetMember]{Items: members.Members}
	if members.After != "" {
		page.NextCursor = encodeCursor(members.After)
	}
	return page, nil
}

// ─── Snapshots and versions ───────────────────────────────────────────

// Snapshot fixes the draft as the dataset's next immutable version. The
// same idempotency key returns the version it made the first time.
func (s *Service) Snapshot(ctx context.Context, p model.Principal, datasetID, idempotencyKey string) (model.DatasetVersion, bool, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsWrite, "dataset.snapshot", "dataset", datasetID); err != nil {
		return model.DatasetVersion{}, false, err
	}
	if len(idempotencyKey) > 128 || hasControl(idempotencyKey) {
		return model.DatasetVersion{}, false, fmt.Errorf("%w: the idempotency key is at most 128 characters", ErrInvalidRequest)
	}
	started := time.Now()
	v, created, err := s.deps.Repo.CreateSnapshot(ctx, interfaces.SnapshotInput{
		TenantID: p.TenantID, DatasetID: datasetID, VersionID: event.NewID(), IdempotencyKey: idempotencyKey,
		Actor: p.UserID, Now: s.now(), MaxMembers: s.cfg.MaxSnapshotMembers,
	})
	if err != nil {
		return v, false, err
	}
	if created {
		s.record(ctx, p, "dataset.snapshot", "dataset", datasetID, map[string]any{
			"version_id": v.ID, "version": v.Version, "members": v.MemberCount, "manifest_sha256": v.ManifestSHA256})
		s.count("dataset_snapshots_created", 1)
		s.count("dataset_snapshot_members", v.MemberCount)
		s.count("dataset_snapshot_ms_total", time.Since(started).Milliseconds())
		s.deps.Log.Info("dataset snapshot", "tenant_id", p.TenantID, "dataset_id", datasetID, "version_id", v.ID,
			"version", v.Version, "members", v.MemberCount, "duration_ms", time.Since(started).Milliseconds())
	}
	return v, created, nil
}

// Versions pages a dataset's versions, newest first; before is a version
// number (0: from the newest).
func (s *Service) Versions(ctx context.Context, p model.Principal, datasetID string, before, limit int) ([]model.DatasetVersion, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset", datasetID); err != nil {
		return nil, err
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return nil, err
	}
	if before < 0 {
		return nil, fmt.Errorf("%w: before must not be negative", ErrInvalidRequest)
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return nil, err
	}
	return s.deps.Repo.ListVersions(ctx, p.TenantID, datasetID, before, size)
}

// Version returns one of the caller's versions; with datasetID set it must
// belong to that dataset.
func (s *Service) Version(ctx context.Context, p model.Principal, datasetID, versionID string) (model.DatasetVersion, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.read", "dataset_version", versionID); err != nil {
		return model.DatasetVersion{}, err
	}
	v, err := s.deps.Repo.GetVersion(ctx, p.TenantID, versionID)
	if err != nil {
		return v, err
	}
	if datasetID != "" && v.DatasetID != datasetID {
		return model.DatasetVersion{}, model.ErrDatasetVersionNotFound
	}
	return v, nil
}

// VersionMembers pages a version's members in canonical order; after is
// the last ordinal of the previous page.
func (s *Service) VersionMembers(ctx context.Context, p model.Principal, datasetID, versionID string, after int64, limit int) ([]model.VersionMember, error) {
	if _, err := s.Version(ctx, p, datasetID, versionID); err != nil {
		return nil, err
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return nil, err
	}
	if after < 0 {
		return nil, fmt.Errorf("%w: after must not be negative", ErrInvalidRequest)
	}
	return s.deps.Repo.ListVersionMembers(ctx, p.TenantID, versionID, after, size)
}

// DiffVersions compares two versions of one dataset by object: what was
// added, removed, changed (other bytes) or relabeled (same bytes, other
// label, split or path) going from base to target.
func (s *Service) DiffVersions(ctx context.Context, p model.Principal, datasetID, baseID, targetID, after string, limit int) (model.VersionDiff, error) {
	if baseID == "" {
		return model.VersionDiff{}, fmt.Errorf("%w: against names the version to compare with", ErrInvalidRequest)
	}
	if _, err := s.Version(ctx, p, datasetID, targetID); err != nil {
		return model.VersionDiff{}, err
	}
	// The other side may be a version of the dataset's parent or of
	// another branch of it: what a branch changed, or will merge.
	base, err := s.Version(ctx, p, "", baseID)
	if err != nil {
		return model.VersionDiff{}, err
	}
	if same, err := s.sameFamily(ctx, p.TenantID, datasetID, base.DatasetID); err != nil || !same {
		if err == nil {
			err = model.ErrDatasetVersionNotFound
		}
		return model.VersionDiff{}, err
	}
	size, err := s.pageSize(limit)
	if err != nil {
		return model.VersionDiff{}, err
	}
	return s.deps.Repo.DiffVersions(ctx, p.TenantID, baseID, targetID, after, size)
}

// ─── Lineage ──────────────────────────────────────────────────────────

// LineageRequest bounds a lineage query.
type LineageRequest struct {
	// Direction is "upstream" (what a node came from, contains or was
	// checked by; the default) or "downstream" (what contains, imports or
	// derives from it).
	Direction string
	Depth     int
	Cursor    string
	Limit     int
}

// DatasetLineage walks the graph from a dataset.
func (s *Service) DatasetLineage(ctx context.Context, p model.Principal, datasetID string, req LineageRequest) (model.LineagePage, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.lineage", "dataset", datasetID); err != nil {
		return model.LineagePage{}, err
	}
	if _, err := s.deps.Repo.GetDataset(ctx, p.TenantID, datasetID); err != nil {
		return model.LineagePage{}, err
	}
	return s.lineage(ctx, p, model.LineageRef{Kind: model.NodeDataset, RefID: datasetID}, req)
}

// VersionLineage walks the graph from a dataset version.
func (s *Service) VersionLineage(ctx context.Context, p model.Principal, versionID string, req LineageRequest) (model.LineagePage, error) {
	if err := s.authorize(ctx, p, model.PermDatasetsRead, "dataset.lineage", "dataset_version", versionID); err != nil {
		return model.LineagePage{}, err
	}
	if _, err := s.deps.Repo.GetVersion(ctx, p.TenantID, versionID); err != nil {
		return model.LineagePage{}, err
	}
	return s.lineage(ctx, p, model.LineageRef{Kind: model.NodeDatasetVersion, RefID: versionID}, req)
}

func (s *Service) lineage(ctx context.Context, p model.Principal, start model.LineageRef, req LineageRequest) (model.LineagePage, error) {
	q := interfaces.LineageQuery{Upstream: true, MaxDepth: req.Depth, MaxEdges: s.cfg.MaxLineageEdges, After: req.Cursor}
	switch req.Direction {
	case "", "upstream":
	case "downstream":
		q.Upstream = false
	default:
		return model.LineagePage{}, fmt.Errorf("%w: direction must be upstream or downstream", ErrInvalidRequest)
	}
	if q.MaxDepth == 0 {
		q.MaxDepth = min(3, s.cfg.MaxLineageDepth)
	}
	if q.MaxDepth < 0 || q.MaxDepth > s.cfg.MaxLineageDepth {
		return model.LineagePage{}, fmt.Errorf("%w: depth must be between 1 and %d", ErrInvalidRequest, s.cfg.MaxLineageDepth)
	}
	var err error
	if q.Limit, err = s.pageSize(req.Limit); err != nil {
		return model.LineagePage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.LineageQueryTimeout)
	defer cancel()
	started := time.Now()
	page, err := s.deps.Repo.Lineage(ctx, p.TenantID, start, q)
	s.count("dataset_lineage_queries", 1)
	s.count("dataset_lineage_ms_total", time.Since(started).Milliseconds())
	if errors.Is(err, context.DeadlineExceeded) {
		return page, model.ErrQueryTimeout
	}
	return page, err
}

// ─── Cursors ──────────────────────────────────────────────────────────

// Cursors of name- and ID-ordered listings are the last key, opaque to
// clients.
func encodeCursor(key string) string {
	return "c1." + hex.EncodeToString([]byte(key))
}

func decodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, ok := strings.CutPrefix(cursor, "c1.")
	key, err := hex.DecodeString(raw)
	if !ok || err != nil || len(key) == 0 || len(key) > 512 {
		return "", model.ErrInvalidCursor
	}
	return string(key), nil
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
