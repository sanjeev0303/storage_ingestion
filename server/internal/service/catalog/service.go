// Package catalog serves the object catalog: logical objects by path,
// their versions, user metadata, listings, and reconciliation of the
// catalog with its assets and with storage.
//
// Versions are created by the upload pipeline, not here: an upload names
// a path, claims the path's object when it starts, and its asset becomes
// the object's next version, and current, when processing publishes it
// (see postgres.CatalogRepository). This package reads the catalog and
// makes the changes a client asks for, each authorized against the
// caller's organization and audited.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

var (
	ErrInvalidRequest   = errors.New("invalid request")
	ErrRevisionRequired = errors.New("the object's revision is required (If-Match or revision)")
)

// Dependencies of the catalog service.
type Dependencies struct {
	Repo interfaces.CatalogRepository
	// Delete requests a version's deletion through the lifecycle, as p:
	// asset.Service.RequestDeletion, which authorizes and audits it and
	// applies retention and legal holds. It reports whether a new deletion
	// was scheduled (false: one already was). The catalog never deletes
	// bytes.
	Delete  func(ctx context.Context, p model.Principal, assetID string) (bool, error)
	Audit   interfaces.AuditRecorder
	Metrics *metrics.Registry
	Log     *slog.Logger
}

// Config bounds listings.
type Config struct {
	DefaultPageSize int
	MaxPageSize     int
	// QueryTimeout cancels a listing statement; SlowQuery logs and counts
	// one that took longer.
	QueryTimeout       time.Duration
	SlowQuery          time.Duration
	MaxMetadataFilters int
}

type Service struct {
	deps Dependencies
	cfg  Config
	now  func() time.Time
}

func NewService(deps Dependencies, cfg Config) (*Service, error) {
	if deps.Repo == nil || deps.Delete == nil || deps.Audit == nil || deps.Log == nil {
		return nil, errors.New("catalog dependencies are required")
	}
	if deps.Metrics == nil {
		deps.Metrics = metrics.New()
	}
	if cfg.DefaultPageSize <= 0 || cfg.MaxPageSize < cfg.DefaultPageSize || cfg.QueryTimeout <= 0 || cfg.MaxMetadataFilters <= 0 {
		return nil, errors.New("invalid catalog configuration")
	}
	return &Service{deps: deps, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Service) require(ctx context.Context, p model.Principal, perm model.Permission, action, resourceID string) error {
	if !p.Valid() {
		return model.ErrUnauthenticated
	}
	return audit.Require(ctx, s.deps.Audit, p, perm, action, "catalog_object", resourceID)
}

// ─── Reads ────────────────────────────────────────────────────────────

// Get returns an object of the caller's organization. Another tenant's
// object is model.ErrObjectNotFound, like a missing one.
func (s *Service) Get(ctx context.Context, p model.Principal, objectID string) (model.CatalogObject, error) {
	if err := s.require(ctx, p, model.PermAssetsRead, "catalog.read", objectID); err != nil {
		return model.CatalogObject{}, err
	}
	return s.deps.Repo.Get(ctx, p.TenantID, objectID)
}

// Lookup finds the object at a path in a dataset namespace ("" for the
// root), with the same normalization uploads use.
func (s *Service) Lookup(ctx context.Context, p model.Principal, dataset, path string) (model.CatalogObject, error) {
	if err := s.require(ctx, p, model.PermAssetsRead, "catalog.read", ""); err != nil {
		return model.CatalogObject{}, err
	}
	if dataset != "" && !model.ValidDataset(dataset) {
		return model.CatalogObject{}, fmt.Errorf("%w: invalid dataset", ErrInvalidRequest)
	}
	_, key, err := model.NormalizePath(path, model.DefaultPathPolicy)
	if err != nil {
		return model.CatalogObject{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return s.deps.Repo.GetByPath(ctx, p.TenantID, dataset, key)
}

// ListRequest is a listing as a client asks for it.
type ListRequest struct {
	// Dataset limits the listing to one namespace when AnyDataset is false
	// ("" is the root namespace).
	Dataset            string
	AnyDataset         bool
	Prefix             string
	ObjectID           string
	AssetID            string
	ContentType        string
	MinSize, MaxSize   int64
	CreatedAfter       time.Time
	CreatedBefore      time.Time
	UpdatedAfter       time.Time
	UpdatedBefore      time.Time
	Metadata           map[string]string
	IncludeUnpublished bool
	Order              string
	Cursor             string
	Limit              int
}

// ListResult is a page and the cursor of the next one ("" on the last).
type ListResult struct {
	Objects    []model.CatalogObject
	NextCursor string
}

// List pages the caller's organization's objects. The tenant is applied
// before every other predicate, pages are keyset pages in a total order,
// and a statement running past the time budget is cancelled
// (model.ErrQueryTimeout) rather than allowed to scan without bound.
func (s *Service) List(ctx context.Context, p model.Principal, req ListRequest) (ListResult, error) {
	if err := s.require(ctx, p, model.PermAssetsRead, "catalog.list", ""); err != nil {
		return ListResult{}, err
	}
	f, err := s.filter(p, req)
	if err != nil {
		return ListResult{}, err
	}
	if req.Cursor != "" {
		pos, err := decodeCursor(req.Cursor, f)
		if err != nil {
			return ListResult{}, err
		}
		f.After = &pos
	}

	started := time.Now()
	page, err := s.deps.Repo.List(ctx, f, s.cfg.QueryTimeout)
	took := time.Since(started)
	s.deps.Metrics.Add("catalog_list_requests", 1)
	s.deps.Metrics.Add("catalog_list_ms", took.Milliseconds())
	if req.Cursor != "" {
		s.deps.Metrics.Add("catalog_list_continuations", 1)
	}
	if errors.Is(err, model.ErrQueryTimeout) {
		s.deps.Metrics.Add("catalog_list_timeouts", 1)
		s.deps.Log.Warn("catalog listing timed out", describe(f)...)
	}
	if err != nil {
		return ListResult{}, err
	}
	s.deps.Metrics.Add("catalog_list_rows", int64(len(page.Objects)))
	if s.cfg.SlowQuery > 0 && took > s.cfg.SlowQuery {
		s.deps.Metrics.Add("catalog_list_slow", 1)
		s.deps.Log.Warn("slow catalog listing", append(describe(f), "duration_ms", took.Milliseconds(), "rows", len(page.Objects))...)
	}

	out := ListResult{Objects: page.Objects}
	if page.Next != nil {
		out.NextCursor = encodeCursor(f, *page.Next)
	}
	return out, nil
}

// describe is a listing's shape for logs: which filters, never their
// values (paths and metadata may be sensitive).
func describe(f model.CatalogFilter) []any {
	return []any{"tenant_id", f.TenantID, "order", string(f.Order), "dataset_filter", !f.AnyDataset,
		"prefix_filter", f.PathPrefix != "", "metadata_filters", len(f.Metadata), "unpublished", f.IncludeUnpublished}
}

func (s *Service) filter(p model.Principal, req ListRequest) (model.CatalogFilter, error) {
	f := model.CatalogFilter{
		TenantID: p.TenantID, Dataset: req.Dataset, AnyDataset: req.AnyDataset, ObjectID: req.ObjectID,
		AssetID: req.AssetID, ContentType: strings.TrimSpace(req.ContentType), MinSize: req.MinSize, MaxSize: req.MaxSize,
		CreatedAfter: req.CreatedAfter, CreatedBefore: req.CreatedBefore, UpdatedAfter: req.UpdatedAfter,
		UpdatedBefore: req.UpdatedBefore, Metadata: req.Metadata, IncludeUnpublished: req.IncludeUnpublished,
		Limit: req.Limit,
	}
	switch req.Order {
	case "", string(model.OrderByPath):
		f.Order = model.OrderByPath
	case string(model.OrderByUpdated):
		f.Order = model.OrderByUpdated
	default:
		return f, fmt.Errorf("%w: order is path or updated", ErrInvalidRequest)
	}
	if f.Limit == 0 {
		f.Limit = s.cfg.DefaultPageSize
	}
	if f.Limit < 0 || f.Limit > s.cfg.MaxPageSize {
		return f, fmt.Errorf("%w: limit is 1 to %d", ErrInvalidRequest, s.cfg.MaxPageSize)
	}
	if !f.AnyDataset && f.Dataset != "" && !model.ValidDataset(f.Dataset) {
		return f, fmt.Errorf("%w: invalid dataset", ErrInvalidRequest)
	}
	if req.Prefix != "" {
		key, err := prefixKey(req.Prefix)
		if err != nil {
			return f, err
		}
		f.PathPrefix = key
	}
	if f.MinSize < 0 || f.MaxSize < 0 || (f.MaxSize > 0 && f.MinSize > f.MaxSize) {
		return f, fmt.Errorf("%w: size range", ErrInvalidRequest)
	}
	if len(f.Metadata) > s.cfg.MaxMetadataFilters {
		return f, fmt.Errorf("%w: at most %d metadata filters", ErrInvalidRequest, s.cfg.MaxMetadataFilters)
	}
	for k := range f.Metadata {
		if !model.ValidMetadataKey(k) {
			return f, fmt.Errorf("%w: metadata filter key %q", ErrInvalidRequest, k)
		}
	}
	return f, nil
}

// prefixKey is a path prefix in key form (lower-cased NFC), matched
// against object path keys. Unlike a path it may end inside a segment or
// with "/".
func prefixKey(prefix string) (string, error) {
	if !utf8.ValidString(prefix) || len(prefix) > model.DefaultPathPolicy.MaxBytes {
		return "", fmt.Errorf("%w: prefix must be UTF-8 of at most %d bytes", ErrInvalidRequest, model.DefaultPathPolicy.MaxBytes)
	}
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "..") || strings.Contains(prefix, `\`) {
		return "", fmt.Errorf("%w: a prefix is relative and has no '..' or '\\'", ErrInvalidRequest)
	}
	for _, r := range prefix {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: prefix has control characters", ErrInvalidRequest)
		}
	}
	return model.PathKey(norm.NFC.String(prefix)), nil
}

// Versions pages an object's published versions, newest first.
func (s *Service) Versions(ctx context.Context, p model.Principal, objectID string, before, limit int) (model.VersionPage, error) {
	if err := s.require(ctx, p, model.PermAssetsRead, "catalog.read", objectID); err != nil {
		return model.VersionPage{}, err
	}
	if limit == 0 {
		limit = s.cfg.DefaultPageSize
	}
	if limit < 0 || limit > s.cfg.MaxPageSize || before < 0 {
		return model.VersionPage{}, fmt.Errorf("%w: limit is 1 to %d", ErrInvalidRequest, s.cfg.MaxPageSize)
	}
	// The object first, so another tenant's object is not found rather
	// than an empty history.
	if _, err := s.deps.Repo.Get(ctx, p.TenantID, objectID); err != nil {
		return model.VersionPage{}, err
	}
	return s.deps.Repo.ListVersions(ctx, p.TenantID, objectID, before, limit)
}

// Version returns one version; version 0 is the current one.
func (s *Service) Version(ctx context.Context, p model.Principal, objectID string, version int) (model.Asset, error) {
	if err := s.require(ctx, p, model.PermAssetsRead, "catalog.read", objectID); err != nil {
		return model.Asset{}, err
	}
	if version == 0 {
		o, err := s.deps.Repo.Get(ctx, p.TenantID, objectID)
		if err != nil {
			return model.Asset{}, err
		}
		if !o.Published() {
			return model.Asset{}, model.ErrVersionNotFound
		}
		version = o.CurrentVersion
	}
	return s.deps.Repo.GetVersion(ctx, p.TenantID, objectID, version)
}

// ─── Changes ──────────────────────────────────────────────────────────

// UpdateMetadata applies a merge patch to the object's user metadata:
// keys with a value are set, keys with nil removed. The revision the
// client read is required; a concurrent change is model.ErrRevisionConflict.
// The audit record names the keys changed, not their values.
func (s *Service) UpdateMetadata(ctx context.Context, p model.Principal, objectID string, revision int64, patch map[string]*string) (model.CatalogObject, error) {
	if err := s.require(ctx, p, model.PermCatalogWrite, "catalog.metadata_update", objectID); err != nil {
		return model.CatalogObject{}, err
	}
	if revision <= 0 {
		return model.CatalogObject{}, ErrRevisionRequired
	}
	var set, removed []string
	for k, v := range patch {
		if v == nil {
			removed = append(removed, k)
		} else {
			set = append(set, k)
		}
	}
	sort.Strings(set)
	sort.Strings(removed)
	return s.change(ctx, p, objectID, revision, "catalog.metadata_update", func(o *model.CatalogObject) error {
		if o.Metadata == nil {
			o.Metadata = map[string]string{}
		}
		for k, v := range patch {
			if v == nil {
				delete(o.Metadata, k)
			} else {
				o.Metadata[k] = *v
			}
		}
		if err := model.ValidateUserMetadata(o.Metadata, model.DefaultMetadataLimits); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		return nil
	}, func(before, after model.CatalogObject) map[string]any {
		return map[string]any{"set": set, "removed": removed}
	})
}

// Rename moves the object to another path in its namespace. It is a
// metadata change: no bytes move, every version keeps its storage key.
// model.ErrPathExists when another object holds the path.
func (s *Service) Rename(ctx context.Context, p model.Principal, objectID string, revision int64, path string) (model.CatalogObject, error) {
	if err := s.require(ctx, p, model.PermCatalogWrite, "catalog.rename", objectID); err != nil {
		return model.CatalogObject{}, err
	}
	if revision <= 0 {
		return model.CatalogObject{}, ErrRevisionRequired
	}
	normalized, key, err := model.NormalizePath(path, model.DefaultPathPolicy)
	if err != nil {
		return model.CatalogObject{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return s.change(ctx, p, objectID, revision, "catalog.rename", func(o *model.CatalogObject) error {
		o.Path, o.PathKey = normalized, key
		return nil
	}, func(before, after model.CatalogObject) map[string]any {
		return map[string]any{"from": before.Path, "to": after.Path}
	})
}

// SetVersioning changes what a new upload to the object's path does.
func (s *Service) SetVersioning(ctx context.Context, p model.Principal, objectID string, revision int64, v model.Versioning) (model.CatalogObject, error) {
	if err := s.require(ctx, p, model.PermCatalogWrite, "catalog.versioning", objectID); err != nil {
		return model.CatalogObject{}, err
	}
	if revision <= 0 {
		return model.CatalogObject{}, ErrRevisionRequired
	}
	if v != model.VersioningEnabled && v != model.VersioningDisabled {
		return model.CatalogObject{}, fmt.Errorf("%w: versioning is ENABLED or DISABLED", ErrInvalidRequest)
	}
	return s.change(ctx, p, objectID, revision, "catalog.versioning", func(o *model.CatalogObject) error {
		o.Versioning = v
		return nil
	}, func(before, after model.CatalogObject) map[string]any {
		return map[string]any{"from": string(before.Versioning), "to": string(after.Versioning)}
	})
}

// change applies a client's change with its audit event, whose details
// come from the object before and after.
func (s *Service) change(ctx context.Context, p model.Principal, objectID string, revision int64, action string,
	apply func(o *model.CatalogObject) error, details func(before, after model.CatalogObject) map[string]any) (model.CatalogObject, error) {
	o, err := s.deps.Repo.Change(ctx, interfaces.ObjectChange{
		TenantID: p.TenantID, ObjectID: objectID, ExpectedRevision: revision, At: s.now(), Apply: apply,
		Audit: func(before, after model.CatalogObject) model.AuditEvent {
			e := audit.From(ctx, p, action, "catalog_object", objectID)
			e.Details = details(before, after)
			e.Details["revision_before"], e.Details["revision_after"] = before.Revision, after.Revision
			return e
		},
	})
	if errors.Is(err, model.ErrRevisionConflict) {
		s.deps.Metrics.Add("catalog_revision_conflicts", 1)
	}
	return o, err
}

// Promote makes an AVAILABLE version current again: restoring an older
// version. Later versions stay.
func (s *Service) Promote(ctx context.Context, p model.Principal, objectID string, version int, revision int64) (model.CatalogObject, error) {
	if err := s.require(ctx, p, model.PermCatalogWrite, "catalog.promote", objectID); err != nil {
		return model.CatalogObject{}, err
	}
	if revision <= 0 {
		return model.CatalogObject{}, ErrRevisionRequired
	}
	o, err := s.deps.Repo.SetCurrent(ctx, p.TenantID, objectID, version, revision, s.now(),
		func(before model.CatalogObject, assetID string) model.AuditEvent {
			e := audit.From(ctx, p, "catalog.current_changed", "catalog_object", objectID)
			e.Details = map[string]any{"from_asset_id": before.CurrentAssetID, "to_asset_id": assetID, "version": version, "reason": "promoted"}
			return e
		})
	if errors.Is(err, model.ErrRevisionConflict) {
		s.deps.Metrics.Add("catalog_revision_conflicts", 1)
	}
	return o, err
}

// DeleteVersion requests the deletion of one version through the
// lifecycle. A current version stops being current at once; the newest
// AVAILABLE version before it becomes current. Retention and legal holds
// delay the removal of its bytes, never the request.
func (s *Service) DeleteVersion(ctx context.Context, p model.Principal, objectID string, version int) (model.Asset, error) {
	if err := s.require(ctx, p, model.PermAssetsDelete, "catalog.version_delete", objectID); err != nil {
		return model.Asset{}, err
	}
	v, err := s.deps.Repo.GetVersion(ctx, p.TenantID, objectID, version)
	if err != nil {
		return model.Asset{}, err
	}
	if _, err := s.deps.Delete(ctx, p, v.ID); err != nil {
		return model.Asset{}, err
	}
	e := audit.From(ctx, p, "catalog.version_delete", "catalog_object", objectID)
	e.Details = map[string]any{"version": version, "asset_id": v.ID}
	s.deps.Audit.Record(ctx, e)
	return s.deps.Repo.GetVersion(ctx, p.TenantID, objectID, version)
}

// DeleteObject deletes a logical object: every published or rejected
// version is handed to the lifecycle, then the object is marked deleted
// and leaves listings. The path keeps its object and history; an upload
// to it later publishes a new version. Versions still being processed
// are not touched (a deletion would race their processing): they are
// reported, and publish into the object when they finish.
func (s *Service) DeleteObject(ctx context.Context, p model.Principal, objectID string) (model.CatalogObject, int, error) {
	if err := s.require(ctx, p, model.PermAssetsDelete, "catalog.object_delete", objectID); err != nil {
		return model.CatalogObject{}, 0, err
	}
	if _, err := s.deps.Repo.Get(ctx, p.TenantID, objectID); err != nil {
		return model.CatalogObject{}, 0, err
	}
	versions, err := s.deps.Repo.LiveVersions(ctx, p.TenantID, objectID)
	if err != nil {
		return model.CatalogObject{}, 0, err
	}
	inFlight := 0
	for _, v := range versions {
		if v.Status == model.AssetStatusQuarantined || v.Status == model.AssetStatusProcessing {
			inFlight++
			continue
		}
		if _, err := s.deps.Delete(ctx, p, v.ID); err != nil {
			return model.CatalogObject{}, inFlight, fmt.Errorf("delete version %d: %w", v.Version, err)
		}
	}
	e := audit.From(ctx, p, "catalog.object_delete", "catalog_object", objectID)
	e.Details = map[string]any{"versions": len(versions) - inFlight, "in_flight": inFlight}
	o, err := s.deps.Repo.MarkDeleted(ctx, p.TenantID, objectID, s.now(), e)
	return o, inFlight, err
}

// RetireReplaced handles catalog.version_replaced: the deletion, as the
// catalog, of a version a newer one replaced on an object whose
// versioning is disabled. It returns why nothing was done when the event
// no longer applies.
func (s *Service) RetireReplaced(ctx context.Context, tenantID, objectID, assetID string) (string, error) {
	o, err := s.deps.Repo.Get(ctx, tenantID, objectID)
	if errors.Is(err, model.ErrObjectNotFound) {
		return "the object no longer exists", nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case o.CurrentAssetID == assetID:
		return "the version is current again", nil
	case o.Versioning != model.VersioningDisabled:
		return "versioning was enabled again: the version is kept", nil
	}
	created, err := s.deps.Delete(ctx, model.ServicePrincipal(tenantID, "catalog"), assetID)
	if errors.Is(err, model.ErrInvalidAssetState) || (err == nil && !created) {
		return "the version is no longer AVAILABLE", nil
	}
	if err != nil {
		return "", err
	}
	return "replaced version handed to the lifecycle", nil
}
