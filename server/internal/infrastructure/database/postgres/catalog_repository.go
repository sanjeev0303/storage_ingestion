package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

var _ interfaces.CatalogRepository = (*CatalogRepository)(nil)

// CatalogRepository stores logical objects. Their versions are assets.
//
// An object's current version follows its assets inside the assets' own
// transactions: publishVersion when an asset becomes AVAILABLE,
// retireVersion when one is soft-deleted, restoreVersion when a deletion
// is undone. Locks are taken in the order asset, object, upload session,
// tenant usage, and the audit chain last, like everywhere else.
type CatalogRepository struct {
	db *DB
}

func NewCatalogRepository(db *DB) *CatalogRepository {
	return &CatalogRepository{db: db}
}

const catalogColumns = `o.id, o.tenant_id, o.dataset, o.path, o.path_key, o.versioning, o.revision, o.metadata,
	COALESCE(o.current_asset_id, ''), COALESCE(o.current_version, 0), COALESCE(o.size_bytes, 0),
	COALESCE(o.content_type, ''), COALESCE(o.checksum_sha256, ''), o.published_at, o.next_version - 1,
	o.deleted_at, o.created_by, o.created_at, o.updated_at, COALESCE(o.storage_connection_id, '')`

func scanObject(row pgx.Row) (model.CatalogObject, error) {
	var o model.CatalogObject
	var versioning string
	var metadata []byte
	err := row.Scan(&o.ID, &o.TenantID, &o.Dataset, &o.Path, &o.PathKey, &versioning, &o.Revision, &metadata,
		&o.CurrentAssetID, &o.CurrentVersion, &o.SizeBytes, &o.ContentType, &o.ChecksumSHA256, &o.PublishedAt,
		&o.Versions, &o.DeletedAt, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.StorageConnectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, model.ErrObjectNotFound
	}
	if err != nil {
		return o, err
	}
	o.Versioning = model.Versioning(versioning)
	o.Metadata, err = decodeMetadata(metadata)
	return o, err
}

func decodeMetadata(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode metadata: %w", err)
	}
	return m, nil
}

// encodeMetadata is the JSONB value of m; nil stays NULL.
func encodeMetadata(m map[string]string) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// ─── Upload start ─────────────────────────────────────────────────────

// claimObject finds or creates the object at the upload's path and, under
// the object's row lock, applies the path's rules:
//   - a create-only upload needs a path with no current version and no
//     upload in flight (ErrPathExists, ErrPathBusy);
//   - no upload starts while a create-only upload of the path is in
//     flight (ErrPathBusy).
//
// An upload is in flight until its asset is published or rejected. The
// lock is taken before the tenant's usage row, which the quota
// reservation locks next.
func claimObject(ctx context.Context, tx pgx.Tx, upload *model.UploadSession) error {
	key := model.PathKey(upload.Path)
	if _, err := tx.Exec(ctx, `
		INSERT INTO catalog_objects (id, tenant_id, dataset, path, path_key, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (tenant_id, dataset, path_key) DO NOTHING
	`, event.NewID(), upload.TenantID, upload.Dataset, upload.Path, key, upload.OwnerID, upload.CreatedAt); err != nil {
		return fmt.Errorf("create catalog object: %w", err)
	}
	var objectID, current string
	if err := tx.QueryRow(ctx, `
		SELECT id, COALESCE(current_asset_id, '') FROM catalog_objects
		WHERE tenant_id = $1 AND dataset = $2 AND path_key = $3
		FOR UPDATE
	`, upload.TenantID, upload.Dataset, key).Scan(&objectID, &current); err != nil {
		return fmt.Errorf("lock catalog object: %w", err)
	}

	var inFlight, createOnly int
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE s.create_only)
		FROM upload_sessions AS s
		LEFT JOIN assets AS a ON a.upload_id = s.id
		WHERE s.object_id = $1 AND s.tenant_id = $2
		  AND (s.status IN ('PENDING', 'UPLOADING', 'COMPLETING', 'ABORTING')
		       OR (s.status IN ('COMPLETED', 'VERIFIED') AND (a.id IS NULL OR a.status IN ('QUARANTINED', 'PROCESSING'))))
	`, objectID, upload.TenantID).Scan(&inFlight, &createOnly); err != nil {
		return fmt.Errorf("count uploads of the path: %w", err)
	}
	switch {
	case upload.CreateOnly && current != "":
		return model.ErrPathExists
	case upload.CreateOnly && inFlight > 0, createOnly > 0:
		return model.ErrPathBusy
	}
	upload.ObjectID = objectID
	return nil
}

// ─── The current version, inside asset transactions ───────────────────

// publishVersion numbers a newly AVAILABLE asset and makes it its
// object's current version, with its metadata if it was uploaded with
// any. The version it replaces stays, unless the object's versioning is
// disabled: then catalog.version_replaced hands it to the lifecycle.
// It returns the audit events to append last in the transaction.
func publishVersion(ctx context.Context, tx pgx.Tx, tenantID, assetID string, at time.Time) ([]model.AuditEvent, error) {
	var objectID string
	var version *int
	var metadata []byte
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(object_id, ''), version, metadata FROM assets WHERE tenant_id = $1 AND id = $2
	`, tenantID, assetID).Scan(&objectID, &version, &metadata)
	if err != nil {
		return nil, fmt.Errorf("read asset version: %w", err)
	}
	if objectID == "" || version != nil {
		return nil, nil // not in the catalog, or already numbered
	}

	var versioning, previous string
	var next int
	if err := tx.QueryRow(ctx, `
		SELECT versioning, COALESCE(current_asset_id, ''), next_version FROM catalog_objects
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE
	`, objectID, tenantID).Scan(&versioning, &previous, &next); err != nil {
		return nil, fmt.Errorf("lock catalog object: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE assets SET version = $3 WHERE tenant_id = $1 AND id = $2`, tenantID, assetID, next); err != nil {
		return nil, fmt.Errorf("number version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE catalog_objects SET next_version = $3 + 1, metadata = COALESCE($4::jsonb, metadata), deleted_at = NULL
		WHERE id = $1 AND tenant_id = $2
	`, objectID, tenantID, next, metadata); err != nil {
		return nil, fmt.Errorf("advance catalog object: %w", err)
	}
	if err := makeCurrent(ctx, tx, tenantID, objectID, assetID, at); err != nil {
		return nil, err
	}
	if model.Versioning(versioning) == model.VersioningDisabled && previous != "" {
		if err := insertOutbox(ctx, tx, event.NewVersionReplaced(tenantID, objectID, previous, at)); err != nil {
			return nil, err
		}
	}

	details := map[string]any{"object_id": objectID, "version": next}
	if previous != "" {
		details["replaced_asset_id"] = previous
	}
	events := []model.AuditEvent{catalogEvent(tenantID, "catalog.version_published", "asset", assetID, at, details)}
	if next == 1 {
		events = append(events, catalogEvent(tenantID, "catalog.object_created", "catalog_object", objectID, at, nil))
	}
	return events, nil
}

// retireVersion moves an object's current version off an asset being
// soft-deleted, to the newest version still AVAILABLE, or to none.
func retireVersion(ctx context.Context, tx pgx.Tx, tenantID, assetID string, at time.Time) ([]model.AuditEvent, error) {
	objectID, err := currentOf(ctx, tx, tenantID, assetID)
	if err != nil || objectID == "" {
		return nil, err
	}
	var next string
	err = tx.QueryRow(ctx, `
		SELECT id FROM assets
		WHERE tenant_id = $1 AND object_id = $2 AND status = 'AVAILABLE' AND version IS NOT NULL AND id <> $3
		ORDER BY version DESC LIMIT 1
	`, tenantID, objectID, assetID).Scan(&next)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("find previous version: %w", err)
	}
	if err := makeCurrent(ctx, tx, tenantID, objectID, next, at); err != nil {
		return nil, err
	}
	return []model.AuditEvent{catalogEvent(tenantID, "catalog.current_changed", "catalog_object", objectID, at,
		map[string]any{"from_asset_id": assetID, "to_asset_id": next, "reason": "version deleted"})}, nil
}

// restoreVersion makes a restored, published asset current again when
// its object has no current version.
func restoreVersion(ctx context.Context, tx pgx.Tx, tenantID, assetID string, at time.Time) ([]model.AuditEvent, error) {
	var objectID string
	var version *int
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(object_id, ''), version FROM assets WHERE tenant_id = $1 AND id = $2 AND status = 'AVAILABLE'
	`, tenantID, assetID).Scan(&objectID, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read restored asset: %w", err)
	}
	if objectID == "" || version == nil {
		return nil, nil
	}
	var current string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(current_asset_id, '') FROM catalog_objects WHERE id = $1 AND tenant_id = $2 FOR UPDATE
	`, objectID, tenantID).Scan(&current); err != nil {
		return nil, fmt.Errorf("lock catalog object: %w", err)
	}
	if current != "" {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE catalog_objects SET deleted_at = NULL WHERE id = $1 AND tenant_id = $2`, objectID, tenantID); err != nil {
		return nil, fmt.Errorf("undelete catalog object: %w", err)
	}
	if err := makeCurrent(ctx, tx, tenantID, objectID, assetID, at); err != nil {
		return nil, err
	}
	return []model.AuditEvent{catalogEvent(tenantID, "catalog.current_changed", "catalog_object", objectID, at,
		map[string]any{"to_asset_id": assetID, "reason": "version restored"})}, nil
}

// currentOf locks and returns the object assetID is the current version
// of, or "".
func currentOf(ctx context.Context, tx pgx.Tx, tenantID, assetID string) (string, error) {
	var objectID string
	err := tx.QueryRow(ctx, `
		SELECT o.id FROM catalog_objects AS o
		JOIN assets AS a ON a.object_id = o.id AND a.tenant_id = o.tenant_id
		WHERE a.tenant_id = $1 AND a.id = $2 AND o.current_asset_id = a.id
		FOR UPDATE OF o
	`, tenantID, assetID).Scan(&objectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find object of current version: %w", err)
	}
	return objectID, nil
}

// makeCurrent points the object at assetID ("" for none) and copies the
// version's system metadata. The caller holds the object's lock.
func makeCurrent(ctx context.Context, tx pgx.Tx, tenantID, objectID, assetID string, at time.Time) error {
	var err error
	if assetID == "" {
		_, err = tx.Exec(ctx, `
			UPDATE catalog_objects
			SET current_asset_id = NULL, current_version = NULL, size_bytes = NULL, content_type = NULL,
			    checksum_sha256 = NULL, published_at = NULL, storage_connection_id = NULL,
			    revision = revision + 1, updated_at = $3
			WHERE id = $1 AND tenant_id = $2
		`, objectID, tenantID, at)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE catalog_objects AS o
			SET current_asset_id = a.id, current_version = a.version, size_bytes = a.size_bytes,
			    content_type = a.content_type, checksum_sha256 = a.checksum_sha256, published_at = a.published_at,
			    storage_connection_id = a.storage_connection_id, revision = o.revision + 1, updated_at = $4
			FROM assets AS a
			WHERE o.id = $1 AND o.tenant_id = $2 AND a.id = $3 AND a.tenant_id = o.tenant_id AND a.object_id = o.id
		`, objectID, tenantID, assetID, at)
	}
	if err != nil {
		return fmt.Errorf("set current version: %w", err)
	}
	return nil
}

// catalogEvent is an audit event the catalog writes for a change the
// system made (publication, deletion), in the tenant's chain.
func catalogEvent(tenantID, action, resourceType, resourceID string, at time.Time, details map[string]any) model.AuditEvent {
	return model.AuditEvent{
		TenantID: tenantID, ActorType: model.ActorService, ActorID: "catalog", Action: action,
		ResourceType: resourceType, ResourceID: resourceID, Outcome: model.AuditSuccess, Details: details, CreatedAt: at,
	}
}

// ─── Reads ────────────────────────────────────────────────────────────

func (r *CatalogRepository) Get(ctx context.Context, tenantID, objectID string) (model.CatalogObject, error) {
	return scanObject(r.db.Pool.QueryRow(ctx, `
		SELECT `+catalogColumns+` FROM catalog_objects AS o WHERE o.tenant_id = $1 AND o.id = $2
	`, tenantID, objectID))
}

func (r *CatalogRepository) GetByPath(ctx context.Context, tenantID, dataset, pathKey string) (model.CatalogObject, error) {
	return scanObject(r.db.Pool.QueryRow(ctx, `
		SELECT `+catalogColumns+` FROM catalog_objects AS o WHERE o.tenant_id = $1 AND o.dataset = $2 AND o.path_key = $3
	`, tenantID, dataset, pathKey))
}

// List returns one page of a tenant's objects. Every predicate is ANDed
// after the tenant's; the page is read with a keyset, never an offset,
// and the statement may run at most timeout.
func (r *CatalogRepository) List(ctx context.Context, f model.CatalogFilter, timeout time.Duration) (model.CatalogPage, error) {
	query, args, err := listQuery(f)
	if err != nil {
		return model.CatalogPage{}, err
	}
	var page model.CatalogPage
	err = withTx(ctx, r.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(timeout.Milliseconds(), 10)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list catalog objects: %w", err)
		}
		page.Objects, err = collect(rows, scanObject)
		return err
	})
	if isQueryTimeout(err) {
		return page, model.ErrQueryTimeout
	}
	if err != nil {
		return page, err
	}
	if len(page.Objects) > f.Limit {
		page.Objects = page.Objects[:f.Limit]
		last := page.Objects[len(page.Objects)-1]
		page.Next = &model.CatalogPosition{Dataset: last.Dataset, PathKey: last.PathKey, UpdatedAt: last.UpdatedAt, ID: last.ID}
		if f.Order == model.OrderByUpdated {
			page.Next.Dataset, page.Next.PathKey = "", ""
		} else {
			page.Next.UpdatedAt, page.Next.ID = time.Time{}, ""
		}
	}
	return page, nil
}

// listQuery builds a listing's SQL.
func listQuery(f model.CatalogFilter) (string, []any, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	where = append(where, "o.tenant_id = "+arg(f.TenantID))
	if !f.IncludeUnpublished {
		where = append(where, "o.current_asset_id IS NOT NULL")
	}
	if !f.AnyDataset {
		where = append(where, "o.dataset = "+arg(f.Dataset))
	}
	if f.PathPrefix != "" {
		// A range on the "C"-collated key: an index range scan, unlike
		// LIKE with a parameter.
		where = append(where, "o.path_key >= "+arg(f.PathPrefix))
		if upper, ok := prefixUpperBound(f.PathPrefix); ok {
			where = append(where, "o.path_key < "+arg(upper))
		}
	}
	if f.ObjectID != "" {
		where = append(where, "o.id = "+arg(f.ObjectID))
	}
	if f.AssetID != "" {
		t := arg(f.TenantID)
		where = append(where, "o.id = (SELECT a.object_id FROM assets AS a WHERE a.tenant_id = "+t+" AND a.id = "+arg(f.AssetID)+")")
	}
	if f.ContentType != "" {
		where = append(where, "o.content_type = "+arg(f.ContentType))
	}
	if f.MinSize > 0 {
		where = append(where, "o.size_bytes >= "+arg(f.MinSize))
	}
	if f.MaxSize > 0 {
		where = append(where, "o.size_bytes <= "+arg(f.MaxSize))
	}
	for _, t := range []struct {
		value time.Time
		cond  string
	}{
		{f.CreatedAfter, "o.created_at >= "}, {f.CreatedBefore, "o.created_at < "},
		{f.UpdatedAfter, "o.updated_at >= "}, {f.UpdatedBefore, "o.updated_at < "},
	} {
		if !t.value.IsZero() {
			where = append(where, t.cond+arg(t.value))
		}
	}
	if len(f.Metadata) > 0 {
		raw, err := json.Marshal(f.Metadata)
		if err != nil {
			return "", nil, err
		}
		where = append(where, "o.metadata @> "+arg(string(raw))+"::jsonb")
	}

	order := "o.dataset, o.path_key"
	if f.Order == model.OrderByUpdated {
		order = "o.updated_at, o.id"
		if f.After != nil {
			where = append(where, "(o.updated_at, o.id) > ("+arg(f.After.UpdatedAt)+", "+arg(f.After.ID)+")")
		}
	} else if f.After != nil {
		where = append(where, "(o.dataset, o.path_key) > ("+arg(f.After.Dataset)+", "+arg(f.After.PathKey)+")")
	}
	query := `SELECT ` + catalogColumns + ` FROM catalog_objects AS o WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + order + ` LIMIT ` + arg(f.Limit+1)
	return query, args, nil
}

// ListQuery exposes the listing SQL so tests and the benchmark can
// EXPLAIN exactly what the API runs.
func ListQuery(f model.CatalogFilter) (string, []any, error) { return listQuery(f) }

// prefixUpperBound is the least string greater than every string that
// starts with prefix, in code-point (and so UTF-8 byte) order: prefix with
// its last code point incremented.
func prefixUpperBound(prefix string) (string, bool) {
	for prefix != "" {
		r, size := utf8.DecodeLastRuneInString(prefix)
		prefix = prefix[:len(prefix)-size]
		next := r + 1
		if next >= 0xD800 && next <= 0xDFFF {
			next = 0xE000 // skip the surrogates, which are not characters
		}
		if next <= utf8.MaxRune {
			return prefix + string(next), true
		}
	}
	return "", false
}

// ListVersions returns an object's published versions, newest first,
// below version before (0: from the newest).
func (r *CatalogRepository) ListVersions(ctx context.Context, tenantID, objectID string, before, limit int) (model.VersionPage, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+` FROM assets
		WHERE tenant_id = $1 AND object_id = $2 AND version IS NOT NULL AND ($3 = 0 OR version < $3)
		ORDER BY version DESC LIMIT $4
	`, tenantID, objectID, before, limit+1)
	if err != nil {
		return model.VersionPage{}, fmt.Errorf("list versions: %w", err)
	}
	assets, err := collect(rows, func(row pgx.Row) (model.Asset, error) {
		a, err := scanAsset(row)
		if err != nil {
			return model.Asset{}, err
		}
		return *a, nil
	})
	if err != nil {
		return model.VersionPage{}, err
	}
	page := model.VersionPage{Versions: assets}
	if len(assets) > limit {
		page.Versions = assets[:limit]
		page.NextBefore = assets[limit-1].Version
	}
	return page, nil
}

func (r *CatalogRepository) GetVersion(ctx context.Context, tenantID, objectID string, version int) (model.Asset, error) {
	asset, err := scanAsset(r.db.Pool.QueryRow(ctx, `
		SELECT `+assetColumns+` FROM assets WHERE tenant_id = $1 AND object_id = $2 AND version = $3
	`, tenantID, objectID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Asset{}, model.ErrVersionNotFound
	}
	if err != nil {
		return model.Asset{}, fmt.Errorf("get version: %w", err)
	}
	return *asset, nil
}

// LiveVersions lists the object's assets that are not deleted, for a
// logical deletion.
func (r *CatalogRepository) LiveVersions(ctx context.Context, tenantID, objectID string) ([]model.Asset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+assetColumns+` FROM assets
		WHERE tenant_id = $1 AND object_id = $2 AND status NOT IN ('SOFT_DELETED', 'DELETED')
		ORDER BY created_at
	`, tenantID, objectID)
	if err != nil {
		return nil, fmt.Errorf("list live versions: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.Asset, error) {
		a, err := scanAsset(row)
		if err != nil {
			return model.Asset{}, err
		}
		return *a, nil
	})
}

// ─── Changes by clients ───────────────────────────────────────────────

// Change applies a client's change to an object's metadata, path or
// versioning. model.ErrRevisionConflict when the object changed since the
// client read it; model.ErrPathExists when the new path is taken.
func (r *CatalogRepository) Change(ctx context.Context, c interfaces.ObjectChange) (model.CatalogObject, error) {
	var out model.CatalogObject
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		before, err := scanObject(tx.QueryRow(ctx, `
			SELECT `+catalogColumns+` FROM catalog_objects AS o WHERE o.tenant_id = $1 AND o.id = $2 FOR UPDATE
		`, c.TenantID, c.ObjectID))
		if err != nil {
			return err
		}
		if before.Revision != c.ExpectedRevision {
			return model.ErrRevisionConflict
		}
		after := before
		after.Metadata = cloneMetadata(before.Metadata)
		if err := c.Apply(&after); err != nil {
			return err
		}
		metadata, err := encodeMetadata(after.Metadata)
		if err != nil {
			return err
		}
		if metadata == nil {
			metadata = []byte("{}")
		}
		out, err = scanObject(tx.QueryRow(ctx, `
			UPDATE catalog_objects AS o
			SET metadata = $3, path = $4, path_key = $5, versioning = $6, revision = revision + 1, updated_at = $7
			WHERE o.tenant_id = $1 AND o.id = $2
			RETURNING `+catalogColumns,
			c.TenantID, c.ObjectID, metadata, after.Path, after.PathKey, string(after.Versioning), c.At))
		if isConstraint(err, "ux_catalog_objects_path") {
			return model.ErrPathExists
		}
		if err != nil {
			return fmt.Errorf("change catalog object: %w", err)
		}
		_, err = appendAudit(ctx, tx, c.Audit(before, out))
		return err
	})
	return out, err
}

func cloneMetadata(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SetCurrent makes a published, AVAILABLE version current: restoring an
// older version, or choosing the newest again.
func (r *CatalogRepository) SetCurrent(ctx context.Context, tenantID, objectID string, version int, expectedRevision int64, at time.Time,
	audit func(before model.CatalogObject, assetID string) model.AuditEvent) (model.CatalogObject, error) {
	var out model.CatalogObject
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		// The asset first: the lock order every asset change uses.
		var assetID, status string
		err := tx.QueryRow(ctx, `
			SELECT id, status FROM assets WHERE tenant_id = $1 AND object_id = $2 AND version = $3 FOR UPDATE
		`, tenantID, objectID, version).Scan(&assetID, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ErrVersionNotFound
		}
		if err != nil {
			return fmt.Errorf("lock version: %w", err)
		}
		if model.AssetStatus(status) != model.AssetStatusAvailable {
			return model.ErrInvalidAssetState
		}
		before, err := scanObject(tx.QueryRow(ctx, `
			SELECT `+catalogColumns+` FROM catalog_objects AS o WHERE o.tenant_id = $1 AND o.id = $2 FOR UPDATE
		`, tenantID, objectID))
		if err != nil {
			return err
		}
		if before.Revision != expectedRevision {
			return model.ErrRevisionConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog_objects SET deleted_at = NULL WHERE id = $1 AND tenant_id = $2`, objectID, tenantID); err != nil {
			return err
		}
		if err := makeCurrent(ctx, tx, tenantID, objectID, assetID, at); err != nil {
			return err
		}
		out, err = scanObject(tx.QueryRow(ctx, `
			SELECT `+catalogColumns+` FROM catalog_objects AS o WHERE o.tenant_id = $1 AND o.id = $2
		`, tenantID, objectID))
		if err != nil {
			return err
		}
		_, err = appendAudit(ctx, tx, audit(before, assetID))
		return err
	})
	return out, err
}

// MarkDeleted records a logical deletion once no version is current any
// more. model.ErrInvalidAssetState when one still is (a version that
// could not be deleted, or one published meanwhile).
func (r *CatalogRepository) MarkDeleted(ctx context.Context, tenantID, objectID string, at time.Time, audit model.AuditEvent) (model.CatalogObject, error) {
	var out model.CatalogObject
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		out, err = scanObject(tx.QueryRow(ctx, `
			UPDATE catalog_objects AS o SET deleted_at = $3, revision = revision + 1, updated_at = $3
			WHERE o.tenant_id = $1 AND o.id = $2 AND o.current_asset_id IS NULL
			RETURNING `+catalogColumns, tenantID, objectID, at))
		if errors.Is(err, model.ErrObjectNotFound) {
			if _, getErr := r.Get(ctx, tenantID, objectID); getErr != nil {
				return getErr
			}
			return model.ErrInvalidAssetState
		}
		if err != nil {
			return err
		}
		_, err = appendAudit(ctx, tx, audit)
		return err
	})
	return out, err
}
