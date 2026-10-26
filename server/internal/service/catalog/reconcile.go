package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/metrics"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// Reconciler compares the catalog with its assets and with storage.
//
// It repairs catalog rows only: the current version, version numbers,
// assets published before the catalog existed. Replaced versions of
// objects without versioning go to the lifecycle, which applies retention.
// Physical discrepancies (a missing object, a size that differs, an
// object nothing refers to) are reported and never repaired by deleting:
// one scan that does not find metadata proves nothing, and orphan bytes
// are the cleanup worker's, after its own grace period.
type Reconciler struct {
	repo    interfaces.CatalogMaintenance
	catalog *Service
	// Storage is optional: without it, the physical checks are skipped.
	storage interfaces.LifecycleStorage
	bucket  string
	prefix  string
	// router checks versions stored on tenants' connections; without it
	// they are skipped.
	router  interfaces.StorageRouter
	// packs finds versions whose bytes were packed; nil when nothing is.
	packs   interfaces.PackLocator
	audit   interfaces.AuditRecorder
	metrics *metrics.Registry
	log     *slog.Logger
	now     func() time.Time
}

func NewReconciler(repo interfaces.CatalogMaintenance, catalog *Service, storage interfaces.LifecycleStorage,
	bucket, prefix string, recorder interfaces.AuditRecorder, registry *metrics.Registry, log *slog.Logger) *Reconciler {
	if registry == nil {
		registry = metrics.New()
	}
	return &Reconciler{repo: repo, catalog: catalog, storage: storage, bucket: bucket, prefix: strings.Trim(prefix, "/"),
		audit: recorder, metrics: registry, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// Packed lets the storage checks find packed versions in their packs.
func (r *Reconciler) Packed(packs interfaces.PackLocator) *Reconciler {
	r.packs = packs
	return r
}

// ReconcileOptions bound a run.
type ReconcileOptions struct {
	// TenantID limits the run to one tenant; "" runs every tenant.
	TenantID string
	// DryRun reports without repairing.
	DryRun    bool
	BatchSize int
	// CheckStorage compares published versions with storage and lists the
	// published prefix, at most MaxStorageChecks objects of each per
	// tenant.
	CheckStorage     bool
	MaxStorageChecks int
	// StaleAfter is how old a completed upload without an asset must be
	// to be reported.
	StaleAfter time.Duration
}

// ReconcileReport is what a run found and did.
type ReconcileReport struct {
	DryRun          bool                   `json:"dry_run"`
	Tenants         int                    `json:"tenants"`
	ObjectsChecked  int                    `json:"objects_checked"`
	VersionsChecked int                    `json:"versions_checked"`
	StoredListed    int                    `json:"stored_objects_listed"`
	StorageErrors   int                    `json:"storage_errors"`
	Counts          map[string]int         `json:"findings_by_kind"`
	Repaired        int                    `json:"repaired"`
	Findings        []model.CatalogFinding `json:"findings"`
}

// maxReportedFindings caps the findings a report lists; Counts has them
// all.
const maxReportedFindings = 1000

func (r *ReconcileReport) add(f model.CatalogFinding) {
	r.Counts[f.Kind]++
	if f.Repaired {
		r.Repaired++
	}
	if len(r.Findings) < maxReportedFindings {
		r.Findings = append(r.Findings, f)
	}
}

// Reconcile runs one reconciliation, audited as the system.
func (r *Reconciler) Reconcile(ctx context.Context, opts ReconcileOptions) (ReconcileReport, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	if opts.MaxStorageChecks <= 0 {
		opts.MaxStorageChecks = 10_000
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 24 * time.Hour
	}
	report := ReconcileReport{DryRun: opts.DryRun, Counts: map[string]int{}}

	valid, err := r.repo.PathIndexValid(ctx)
	if err != nil {
		return report, err
	}
	if !valid {
		report.add(model.CatalogFinding{Kind: model.FindingPathIndexInvalid, Detail: "rebuild ux_catalog_objects_path; paths may be duplicated"})
	}

	tenants := []string{opts.TenantID}
	if opts.TenantID == "" {
		if tenants, err = r.allTenants(ctx); err != nil {
			return report, err
		}
	}
	for _, tenant := range tenants {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.Tenants++
		if err := r.tenant(ctx, tenant, opts, &report); err != nil {
			return report, fmt.Errorf("tenant %s: %w", tenant, err)
		}
	}

	for kind, n := range report.Counts {
		r.metrics.Add("catalog_findings_"+kind, int64(n))
	}
	e := audit.System(ctx, "catalog.reconcile", "catalog", opts.TenantID)
	e.TenantID = ""
	e.Details = map[string]any{"dry_run": opts.DryRun, "tenants": report.Tenants, "repaired": report.Repaired, "findings": report.Counts}
	r.audit.Record(ctx, e)
	return report, nil
}

func (r *Reconciler) allTenants(ctx context.Context) ([]string, error) {
	var all []string
	after := ""
	for {
		page, err := r.repo.CatalogTenantsAfter(ctx, after, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			return all, nil
		}
		after = page[len(page)-1]
	}
}

func (r *Reconciler) tenant(ctx context.Context, tenant string, opts ReconcileOptions, report *ReconcileReport) error {
	if err := r.objects(ctx, tenant, opts, report); err != nil {
		return err
	}
	if err := r.uncatalogued(ctx, tenant, opts, report); err != nil {
		return err
	}
	stale, err := r.repo.StaleUploads(ctx, tenant, r.now().Add(-opts.StaleAfter))
	if err != nil {
		return err
	}
	if stale > 0 {
		report.add(model.CatalogFinding{Kind: model.FindingStaleUploads, TenantID: tenant,
			Detail: fmt.Sprintf("%d completed uploads older than %s have no asset; processing reconciliation enqueues them", stale, opts.StaleAfter)})
	}
	if opts.CheckStorage && r.storage != nil {
		if err := r.published(ctx, tenant, opts, report); err != nil {
			return err
		}
		if err := r.unreferenced(ctx, tenant, opts, report); err != nil {
			return err
		}
	}
	return nil
}

// objects checks every object's current version and version numbers.
func (r *Reconciler) objects(ctx context.Context, tenant string, opts ReconcileOptions, report *ReconcileReport) error {
	var after model.CatalogPosition
	for {
		checks, err := r.repo.CheckObjects(ctx, tenant, after, opts.BatchSize)
		if err != nil {
			return err
		}
		for _, c := range checks {
			report.ObjectsChecked++
			o := c.Object
			var kinds []string
			switch {
			case c.CurrentStatus != "" && c.CurrentStatus != string(model.AssetStatusAvailable):
				kinds = append(kinds, model.FindingCurrentInvalid)
			case c.CurrentStale:
				kinds = append(kinds, model.FindingCurrentStale)
			}
			if c.MissingCurrent {
				kinds = append(kinds, model.FindingCurrentMissing)
			}
			if c.Unnumbered {
				kinds = append(kinds, model.FindingUnnumbered)
			}
			repaired := false
			if len(kinds) > 0 && !opts.DryRun {
				if err := r.repo.RepairCurrent(ctx, tenant, o.ID, r.now()); err != nil {
					return err
				}
				repaired = true
			}
			for _, kind := range kinds {
				report.add(model.CatalogFinding{Kind: kind, TenantID: tenant, ObjectID: o.ID, AssetID: o.CurrentAssetID, Repaired: repaired})
			}

			for _, assetID := range c.ReplacedLive {
				f := model.CatalogFinding{Kind: model.FindingReplacedLive, TenantID: tenant, ObjectID: o.ID, AssetID: assetID}
				if !opts.DryRun {
					note, err := r.catalog.RetireReplaced(ctx, tenant, o.ID, assetID)
					if err != nil {
						return err
					}
					f.Detail, f.Repaired = note, true
				}
				report.add(f)
			}
		}
		if len(checks) < opts.BatchSize {
			return nil
		}
		last := checks[len(checks)-1].Object
		after = model.CatalogPosition{Dataset: last.Dataset, PathKey: last.PathKey}
	}
}

// uncatalogued attaches AVAILABLE assets from before the catalog to the
// object at their file name in their dataset: the backfill.
func (r *Reconciler) uncatalogued(ctx context.Context, tenant string, opts ReconcileOptions, report *ReconcileReport) error {
	after := ""
	for {
		assets, err := r.repo.UncataloguedAssets(ctx, tenant, after, opts.BatchSize)
		if err != nil {
			return err
		}
		for _, a := range assets {
			path := legacyPath(a)
			f := model.CatalogFinding{Kind: model.FindingUncatalogued, TenantID: tenant, AssetID: a.ID, Detail: "path " + path}
			if !opts.DryRun {
				if f.Repaired, err = r.repo.AttachLegacy(ctx, a, path, r.now()); err != nil {
					return err
				}
			}
			report.add(f)
		}
		if len(assets) < opts.BatchSize {
			return nil
		}
		after = assets[len(assets)-1].ID
	}
}

// legacyPath is the path an asset from before the catalog is attached at:
// its file name when that is a valid path, otherwise one derived from its
// ID so it is still reachable.
func legacyPath(a model.Asset) string {
	if normalized, _, err := model.NormalizePath(a.FileName, model.DefaultPathPolicy); err == nil {
		return normalized
	}
	return "legacy/" + a.ID
}

// published compares published versions with their stored objects.
func (r *Reconciler) published(ctx context.Context, tenant string, opts ReconcileOptions, report *ReconcileReport) error {
	after := ""
	checked := 0
	for checked < opts.MaxStorageChecks {
		versions, err := r.repo.PublishedVersions(ctx, tenant, after, min(opts.BatchSize, opts.MaxStorageChecks-checked))
		if err != nil {
			return err
		}
		for _, v := range versions {
			checked++
			report.VersionsChecked++
			storage, bucket, err := r.versionStorage(ctx, v)
			if err != nil {
				report.StorageErrors++
				r.log.Warn("catalog storage check skipped", "tenant_id", tenant, "asset_id", v.ID, "error", err)
				continue
			}
			// A packed version's bytes are its entry in the pack: the pack must
			// be there, as large as recorded.
			key, versionID, wantSize := v.ObjectKey, v.StorageVersionID, v.SizeBytes
			if r.packs != nil {
				loc, packed, err := r.packs.PackedLocation(ctx, tenant, v.ID)
				if err != nil {
					report.StorageErrors++
					r.log.Warn("catalog pack lookup failed", "tenant_id", tenant, "asset_id", v.ID, "error", err)
					continue
				}
				if packed {
					if loc.Length != v.SizeBytes {
						report.add(model.CatalogFinding{Kind: model.FindingSizeMismatch, TenantID: tenant, ObjectID: v.ObjectID,
							AssetID: v.ID, Key: loc.ObjectKey, Detail: fmt.Sprintf("catalog %d bytes, pack entry %d", v.SizeBytes, loc.Length)})
						continue
					}
					bucket, key, versionID, wantSize = loc.Bucket, loc.ObjectKey, loc.StorageVersionID, loc.PackBytes
				}
			}
			var meta interfaces.ObjectMetadata
			if versionID != "" {
				meta, err = storage.HeadObjectVersion(ctx, bucket, key, versionID)
			} else {
				meta, err = storage.HeadObject(ctx, bucket, key)
			}
			switch {
			case errors.Is(err, interfaces.ErrObjectNotFound):
				r.metrics.Add("catalog_missing_objects", 1)
				report.add(model.CatalogFinding{Kind: model.FindingObjectMissing, TenantID: tenant, ObjectID: v.ObjectID,
					AssetID: v.ID, Key: key, Detail: fmt.Sprintf("version %d", v.Version)})
			case err != nil:
				// Not a finding: an unreachable store says nothing about the
				// object. Counted, and the run goes on.
				report.StorageErrors++
				r.log.Warn("catalog storage check failed", "tenant_id", tenant, "asset_id", v.ID, "error", err)
			case meta.SizeBytes != wantSize:
				report.add(model.CatalogFinding{Kind: model.FindingSizeMismatch, TenantID: tenant, ObjectID: v.ObjectID, AssetID: v.ID,
					Key: key, Detail: fmt.Sprintf("expected %d bytes, stored %d", wantSize, meta.SizeBytes)})
			}
		}
		if len(versions) == 0 || len(versions) < opts.BatchSize {
			return nil
		}
		after = versions[len(versions)-1].ID
	}
	return nil
}

// Route lets the reconciler check versions stored on tenants' storage
// connections.
func (r *Reconciler) Route(router interfaces.StorageRouter) *Reconciler {
	r.router = router
	return r
}

// versionStorage is where a published version's bytes are: the
// platform's bucket or the version's own storage connection.
func (r *Reconciler) versionStorage(ctx context.Context, v model.Asset) (interfaces.LifecycleStorage, string, error) {
	if model.IsPlatformConnection(v.StorageConnectionID) {
		return r.storage, r.bucket, nil
	}
	if r.router == nil {
		return nil, "", model.ErrStorageNotConfigured
	}
	loc, err := r.router.Locate(ctx, v.TenantID, v.StorageConnectionID)
	if err != nil {
		return nil, "", err
	}
	return loc.Storage, loc.ReadyBucket, nil
}

// unreferenced lists the tenant's published prefix on the platform's
// bucket and reports stored objects no asset refers to. Nothing is
// deleted. Tenants' own connections are swept by the cleanup worker.
func (r *Reconciler) unreferenced(ctx context.Context, tenant string, opts ReconcileOptions, report *ReconcileReport) error {
	prefix := r.prefix + "/" + tenant + "/"
	cursor := ""
	listed := 0
	for listed < opts.MaxStorageChecks {
		page, err := r.storage.ListObjectsPage(ctx, r.bucket, prefix, cursor, min(opts.BatchSize, 1000))
		if err != nil {
			report.StorageErrors++
			r.log.Warn("catalog storage listing failed", "tenant_id", tenant, "error", err)
			return nil
		}
		// Pack objects belong to pack records, which the pack worker checks.
		objects := page.Objects[:0:0]
		ids := make([]string, 0, len(page.Objects))
		for _, o := range page.Objects {
			id := strings.TrimPrefix(o.ObjectKey, prefix)
			if strings.HasPrefix(id, "packs/") {
				continue
			}
			objects = append(objects, o)
			ids = append(ids, id)
		}
		known, err := r.repo.ExistingAssets(ctx, tenant, ids)
		if err != nil {
			return err
		}
		for i, o := range objects {
			listed++
			report.StoredListed++
			if !known[ids[i]] {
				report.add(model.CatalogFinding{Kind: model.FindingUnreferenced, TenantID: tenant, Key: o.ObjectKey,
					Detail: "no asset refers to it; the cleanup worker's orphan sweep decides, after its grace period"})
			}
		}
		if page.Cursor == "" {
			return nil
		}
		cursor = page.Cursor
	}
	return nil
}

// Stats sizes the catalog.
func (r *Reconciler) Stats(ctx context.Context) (model.CatalogStats, error) {
	return r.repo.Stats(ctx)
}
