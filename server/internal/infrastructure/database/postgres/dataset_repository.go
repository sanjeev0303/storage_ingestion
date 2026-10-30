package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DatasetRepository stores managed datasets (Phase 10): the dataset rows
// Phase 7 created, their draft membership, immutable versions, manifest
// imports (dataset_import_repository.go), validation
// (validation_repository.go) and lineage (lineage_repository.go).
//
// Every change to a dataset's membership or versions takes the dataset's
// row lock first, so a snapshot sees a membership no change is halfway
// through.
type DatasetRepository struct {
	db *DB
}

func NewDatasetRepository(db *DB) *DatasetRepository {
	return &DatasetRepository{db: db}
}

var _ interfaces.DatasetRepository = (*DatasetRepository)(nil)

// leaveReady is the status a change to the draft leaves: a validated
// outcome no longer describes it.
const leaveReady = `CASE WHEN lifecycle_status IN ('READY', 'READY_WITH_WARNINGS', 'FAILED') THEN 'DRAFT' ELSE lifecycle_status END`

func newRowID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ─── Datasets ─────────────────────────────────────────────────────────

func (r *DatasetRepository) CreateDataset(ctx context.Context, d model.Dataset) (model.Dataset, bool, error) {
	metadata, err := json.Marshal(nonNilMetadata(d.Metadata))
	if err != nil {
		return model.Dataset{}, false, err
	}
	var derivation []byte
	if d.Derivation != nil {
		if derivation, err = json.Marshal(d.Derivation); err != nil {
			return model.Dataset{}, false, err
		}
	}
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}

	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.Dataset{}, false, fmt.Errorf("begin create dataset: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	stored, err := scanDataset(tx.QueryRow(ctx, `
		INSERT INTO datasets (id, tenant_id, name, completion_policy, status, max_bytes, created_by, created_at, updated_at,
		                      description, tags, metadata, profile_id, profile_version, derivation, updated_by)
		VALUES ($1, $2, $3, $4, 'OPEN', $5, $6, $7, $7, $8, $9, $10, NULLIF($11, ''), $12, $13, $6)
		ON CONFLICT (tenant_id, name) DO NOTHING
		RETURNING `+datasetColumns,
		d.ID, d.TenantID, d.Name, string(d.CompletionPolicy), d.MaxBytes, d.CreatedBy, d.CreatedAt,
		d.Description, tags, metadata, d.ProfileID, d.ProfileVersion, derivation))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := scanDataset(tx.QueryRow(ctx,
			`SELECT `+datasetColumns+` FROM datasets WHERE tenant_id = $1 AND name = $2`, d.TenantID, d.Name))
		if err != nil {
			return model.Dataset{}, false, fmt.Errorf("read existing dataset: %w", err)
		}
		if !sameDataset(existing, d) {
			return existing, false, model.ErrDatasetConflict
		}
		return existing, false, nil
	}
	if err != nil {
		return model.Dataset{}, false, fmt.Errorf("create dataset: %w", err)
	}

	if d.Derivation != nil {
		if stored, err = r.fillDerived(ctx, tx, stored); err != nil {
			return model.Dataset{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Dataset{}, false, fmt.Errorf("commit create dataset: %w", err)
	}
	return stored, true, nil
}

// fillDerived copies the source versions' members into a new derived
// dataset, pinned to their exact versions, and records where they came
// from. The first source to hold an object wins. An empty derivation
// records only where it came from.
func (r *DatasetRepository) fillDerived(ctx context.Context, tx pgx.Tx, d model.Dataset) (model.Dataset, error) {
	splits := nonNilStrings(d.Derivation.Splits)
	labels := nonNilStrings(d.Derivation.Labels)
	var copied int64
	for _, source := range d.Derivation.SourceVersionIDs {
		if d.Derivation.Empty {
			err := link(ctx, tx, d.TenantID, model.LineageEdge{
				From: model.LineageRef{Kind: model.NodeDataset, RefID: d.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: source},
				Relation: model.RelCreatedFromSnapshot, Actor: d.CreatedBy, Details: map[string]any{"members_copied": false},
			}, d.CreatedAt, true)
			if err != nil {
				return d, err
			}
			continue
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO dataset_members (id, tenant_id, dataset_id, object_id, asset_id, label, split, metadata,
			                             source, added_by, created_at, updated_at)
			SELECT replace(gen_random_uuid()::text, '-', ''), vm.tenant_id, $3, vm.object_id, vm.asset_id, vm.label, vm.split,
			       vm.member_metadata, 'derived:' || $2, $6, $7, $7
			FROM dataset_version_members AS vm
			WHERE vm.version_id = $2 AND vm.tenant_id = $1
			  AND (cardinality($4::text[]) = 0 OR vm.split = ANY($4))
			  AND (cardinality($5::text[]) = 0 OR vm.label = ANY($5))
			ORDER BY vm.ordinal
			ON CONFLICT (dataset_id, object_id) DO NOTHING
		`, d.TenantID, source, d.ID, splits, labels, d.CreatedBy, d.CreatedAt)
		if err != nil {
			return d, fmt.Errorf("copy members of version %s: %w", source, err)
		}
		copied += tag.RowsAffected()

		// The new dataset was created from that snapshot.
		err = link(ctx, tx, d.TenantID, model.LineageEdge{
			From: model.LineageRef{Kind: model.NodeDataset, RefID: d.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: source},
			Relation: model.RelCreatedFromSnapshot, Actor: d.CreatedBy,
			Details: map[string]any{"splits": splits, "labels": labels},
		}, d.CreatedAt, true)
		if err != nil {
			return d, err
		}
	}
	if t := d.Derivation.Transformation; t != nil {
		err := link(ctx, tx, d.TenantID, model.LineageEdge{
			From: model.LineageRef{Kind: model.NodeDataset, RefID: d.ID}, To: transformationRef(*t),
			Relation: model.RelTransformedBy, Actor: d.CreatedBy, Details: transformationDetails(*t),
		}, d.CreatedAt, true)
		if err != nil {
			return d, err
		}
	}
	return scanDataset(tx.QueryRow(ctx, `
		UPDATE datasets SET member_count = $3, members_revision = members_revision + 1
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+datasetColumns, d.TenantID, d.ID, copied))
}

func transformationRef(t model.Transformation) model.LineageRef {
	return model.LineageRef{Kind: model.NodeTransformation, RefID: t.Name + "@" + t.Version + "#" + t.ConfigSHA256}
}

func transformationDetails(t model.Transformation) map[string]any {
	return map[string]any{"name": t.Name, "version": t.Version, "config_sha256": t.ConfigSHA256}
}

// sameDataset tells whether a create request matches the dataset that
// already has its name.
func sameDataset(existing, want model.Dataset) bool {
	return existing.CompletionPolicy == want.CompletionPolicy && existing.MaxBytes == want.MaxBytes &&
		existing.Description == want.Description && slices.Equal(nonNilStrings(existing.Tags), nonNilStrings(want.Tags)) &&
		maps.Equal(nonNilMetadata(existing.Metadata), nonNilMetadata(want.Metadata)) &&
		existing.ProfileID == want.ProfileID && existing.ProfileVersion == want.ProfileVersion &&
		(existing.Derivation == nil) == (want.Derivation == nil)
}

func nonNilMetadata(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (r *DatasetRepository) GetDataset(ctx context.Context, tenantID, datasetID string) (model.Dataset, error) {
	d, err := scanDataset(r.db.Pool.QueryRow(ctx,
		`SELECT `+datasetColumns+` FROM datasets WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Dataset{}, model.ErrDatasetNotFound
	}
	if err != nil {
		return model.Dataset{}, fmt.Errorf("get dataset: %w", err)
	}
	return d, nil
}

// ListDatasets pages a tenant's datasets by name, optionally of one
// lifecycle status.
func (r *DatasetRepository) ListDatasets(ctx context.Context, tenantID string, f interfaces.DatasetFilter) ([]model.Dataset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+datasetColumns+` FROM datasets
		WHERE tenant_id = $1 AND ($2 = '' OR lifecycle_status = $2) AND name > $3
		  AND parent_dataset_id IS NULL -- branches are listed by their dataset
		ORDER BY name
		LIMIT $4
	`, tenantID, string(f.Lifecycle), f.After, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list datasets: %w", err)
	}
	return collect(rows, scanDataset)
}

func (r *DatasetRepository) UpdateDataset(ctx context.Context, tenantID, datasetID string, revision int64, patch interfaces.DatasetPatch,
	actor string, now time.Time) (model.Dataset, error) {
	var metadata []byte
	if patch.Metadata != nil {
		var err error
		if metadata, err = json.Marshal(nonNilMetadata(*patch.Metadata)); err != nil {
			return model.Dataset{}, err
		}
	}
	var tags []string
	if patch.Tags != nil {
		tags = nonNilStrings(*patch.Tags)
	}
	var residency []string
	if patch.Residency != nil {
		residency = nonNilStrings(*patch.Residency)
	}
	d, err := scanDataset(r.db.Pool.QueryRow(ctx, `
		UPDATE datasets SET
		    description = COALESCE($4, description),
		    tags = CASE WHEN $5::boolean THEN $6::text[] ELSE tags END,
		    metadata = COALESCE($7::jsonb, metadata),
		    profile_id = CASE WHEN $8::text IS NULL THEN profile_id ELSE NULLIF($8, '') END,
		    profile_version = COALESCE($9, profile_version),
		    storage_connection_id = CASE WHEN $12::text IS NULL THEN storage_connection_id ELSE NULLIF($12, '') END,
		    data_class = CASE WHEN $13::text IS NULL THEN data_class ELSE NULLIF($13, '') END,
		    residency = CASE WHEN $14::boolean THEN $15::text[] ELSE residency END,
		    require_customer_key = COALESCE($16, require_customer_key),
		    replica_connection_id = CASE WHEN $17::text IS NULL THEN replica_connection_id ELSE NULLIF($17, '') END,
		    escrow = COALESCE($18, escrow),
		    vault_retention_days = COALESCE($19, vault_retention_days),
		    scrub_days = COALESCE($20, scrub_days),
		    revision = revision + 1, updated_by = $10, updated_at = $11
		WHERE tenant_id = $1 AND id = $2 AND revision = $3 AND lifecycle_status <> 'ARCHIVED'
		RETURNING `+datasetColumns,
		tenantID, datasetID, revision, patch.Description, patch.Tags != nil, tags, metadata,
		patch.ProfileID, patch.ProfileVersion, actor, now, patch.StorageConnectionID, patch.DataClass,
		patch.Residency != nil, residency, patch.RequireCustomerKey,
		patch.ReplicaConnectionID, patch.Escrow, patch.VaultRetentionDays, patch.ScrubDays))
	if errors.Is(err, pgx.ErrNoRows) {
		return r.whyNot(ctx, tenantID, datasetID, model.ErrRevisionConflict)
	}
	if err != nil {
		// The binding trigger refuses another tenant's or an inactive
		// connection.
		return model.Dataset{}, bindingError(err)
	}
	return d, nil
}

// whyNot explains a conditional change of a dataset that matched no row:
// the dataset is missing or archived, or else otherwise.
func (r *DatasetRepository) whyNot(ctx context.Context, tenantID, datasetID string, otherwise error) (model.Dataset, error) {
	d, err := r.GetDataset(ctx, tenantID, datasetID)
	if err != nil {
		return model.Dataset{}, err
	}
	if d.Lifecycle == model.LifecycleArchived {
		return d, model.ErrDatasetArchived
	}
	return d, otherwise
}

// ArchiveDataset makes the dataset read-only. Imports still waiting are
// cancelled; running ones are asked to stop.
func (r *DatasetRepository) ArchiveDataset(ctx context.Context, tenantID, datasetID, actor string, now time.Time) (model.Dataset, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.Dataset{}, fmt.Errorf("begin archive: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	d, err := scanDataset(tx.QueryRow(ctx, `
		UPDATE datasets SET lifecycle_status = 'ARCHIVED', archived_at = $3, revision = revision + 1,
		                    updated_by = $4, updated_at = $3
		WHERE tenant_id = $1 AND id = $2 AND lifecycle_status = ANY($5)
		RETURNING `+datasetColumns, tenantID, datasetID, now, actor, model.TransitionSources(model.LifecycleArchived)))
	if errors.Is(err, pgx.ErrNoRows) {
		return r.whyNot(ctx, tenantID, datasetID, model.ErrDatasetTransition)
	}
	if err != nil {
		return model.Dataset{}, fmt.Errorf("archive dataset: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE dataset_manifest_imports SET
		    state = CASE WHEN state IN ('AWAITING_UPLOAD', 'QUEUED') THEN 'CANCELLED' ELSE state END,
		    completed_at = CASE WHEN state IN ('AWAITING_UPLOAD', 'QUEUED') THEN $3 ELSE completed_at END,
		    cancel_requested = true, updated_at = $3
		WHERE tenant_id = $1 AND dataset_id = $2 AND state IN ('AWAITING_UPLOAD', 'QUEUED', 'RUNNING')
	`, tenantID, datasetID, now); err != nil {
		return model.Dataset{}, fmt.Errorf("stop imports: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Dataset{}, fmt.Errorf("commit archive: %w", err)
	}
	return d, nil
}

// lockDraft takes the dataset's row lock for a change to its draft.
func lockDraft(ctx context.Context, tx pgx.Tx, tenantID, datasetID string) (model.Dataset, error) {
	d, err := scanDataset(tx.QueryRow(ctx,
		`SELECT `+datasetColumns+` FROM datasets WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, datasetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, model.ErrDatasetNotFound
	}
	if err != nil {
		return d, fmt.Errorf("lock dataset: %w", err)
	}
	if d.Lifecycle == model.LifecycleArchived {
		return d, model.ErrDatasetArchived
	}
	return d, nil
}

// ─── Membership ───────────────────────────────────────────────────────

// ResolveObjects looks references up in the tenant's catalog only, so a
// reference to another tenant's object resolves to nothing.
func (r *DatasetRepository) ResolveObjects(ctx context.Context, tenantID string, refs []interfaces.ObjectRef) ([]interfaces.ResolvedRef, error) {
	out := make([]interfaces.ResolvedRef, len(refs))
	var ids, namespaces, keys []string
	var byID, byPath []int
	for i, ref := range refs {
		if ref.ObjectID != "" {
			ids = append(ids, ref.ObjectID)
			byID = append(byID, i)
		} else {
			namespaces = append(namespaces, ref.Namespace)
			keys = append(keys, ref.PathKey)
			byPath = append(byPath, i)
		}
	}

	type found struct {
		id      string
		deleted bool
	}
	if len(ids) > 0 {
		rows, err := r.db.Pool.Query(ctx, `
			SELECT id, deleted_at IS NOT NULL FROM catalog_objects WHERE tenant_id = $1 AND id = ANY($2)
		`, tenantID, ids)
		if err != nil {
			return nil, fmt.Errorf("resolve object IDs: %w", err)
		}
		objects := map[string]found{}
		for rows.Next() {
			var f found
			if err := rows.Scan(&f.id, &f.deleted); err != nil {
				rows.Close()
				return nil, err
			}
			objects[f.id] = f
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, i := range byID {
			if f, ok := objects[refs[i].ObjectID]; ok {
				out[i] = interfaces.ResolvedRef{ObjectID: f.id, ObjectDeleted: f.deleted}
			}
		}
	}
	if len(byPath) > 0 {
		rows, err := r.db.Pool.Query(ctx, `
			SELECT r.i, o.id, o.deleted_at IS NOT NULL
			FROM unnest($2::text[], $3::text[]) WITH ORDINALITY AS r(namespace, path_key, i)
			JOIN catalog_objects AS o ON o.tenant_id = $1 AND o.dataset = r.namespace AND o.path_key = r.path_key
		`, tenantID, namespaces, keys)
		if err != nil {
			return nil, fmt.Errorf("resolve object paths: %w", err)
		}
		for rows.Next() {
			var i int
			var f found
			if err := rows.Scan(&i, &f.id, &f.deleted); err != nil {
				rows.Close()
				return nil, err
			}
			out[byPath[i-1]] = interfaces.ResolvedRef{ObjectID: f.id, ObjectDeleted: f.deleted}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Versions, of objects found: by asset ID or by number.
	var vObjects, vIDs []string
	var vNumbers []int32
	var vIndex []int
	for i, ref := range refs {
		if out[i].ObjectID != "" && (ref.VersionID != "" || ref.Version > 0) {
			vObjects = append(vObjects, out[i].ObjectID)
			vIDs = append(vIDs, ref.VersionID)
			vNumbers = append(vNumbers, int32(ref.Version))
			vIndex = append(vIndex, i)
		}
	}
	if len(vIndex) > 0 {
		rows, err := r.db.Pool.Query(ctx, `
			SELECT r.i, a.id
			FROM unnest($2::text[], $3::text[], $4::int[]) WITH ORDINALITY AS r(object_id, version_id, version, i)
			JOIN assets AS a ON a.tenant_id = $1 AND a.object_id = r.object_id
			     AND ((r.version_id <> '' AND a.id = r.version_id) OR (r.version_id = '' AND a.version = r.version))
		`, tenantID, vObjects, vIDs, vNumbers)
		if err != nil {
			return nil, fmt.Errorf("resolve versions: %w", err)
		}
		for rows.Next() {
			var i int
			var assetID string
			if err := rows.Scan(&i, &assetID); err != nil {
				rows.Close()
				return nil, err
			}
			out[vIndex[i-1]].AssetID = assetID
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AddMembers writes changes, which name distinct objects, under the
// dataset's lock. The duplicate policy decides what an object that is
// already a member gets: REJECT and SKIP leave it, REPLACE overwrites it
// (a replacement that changes nothing counts as skipped).
func (r *DatasetRepository) AddMembers(ctx context.Context, tenantID, datasetID string, changes []model.MemberChange,
	policy model.DuplicatePolicy, source, actor string, now time.Time, cp *interfaces.ImportCheckpoint) (model.MemberResult, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.MemberResult{}, fmt.Errorf("begin add members: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if cp != nil {
		// The import must still be this worker's, and the batch new.
		var ok bool
		err := tx.QueryRow(ctx, `
			SELECT state = 'RUNNING' AND lease_owner = $2 AND checkpoint_line < $3
			FROM dataset_manifest_imports WHERE id = $1 FOR UPDATE
		`, cp.ImportID, cp.Owner, cp.Lines).Scan(&ok)
		if err != nil {
			return model.MemberResult{}, fmt.Errorf("lock import: %w", err)
		}
		if !ok {
			return model.MemberResult{}, model.ErrImportState
		}
	}

	result := model.MemberResult{Outcomes: make([]model.MemberOutcome, len(changes))}
	var added, updated int64
	if len(changes) > 0 {
		d, err := lockDraft(ctx, tx, tenantID, datasetID)
		if err != nil {
			return result, err
		}
		result.Dataset = d

		ids := make([]string, len(changes))
		objects := make([]string, len(changes))
		assets := make([]string, len(changes))
		labels := make([]string, len(changes))
		splits := make([]string, len(changes))
		metadata := make([]string, len(changes))
		index := make(map[string]int, len(changes))
		for i, c := range changes {
			raw, err := json.Marshal(nonNilMetadata(c.Metadata))
			if err != nil {
				return result, err
			}
			ids[i], objects[i], assets[i], labels[i], splits[i], metadata[i] = newRowID(), c.ObjectID, c.AssetID, c.Label, c.Split, string(raw)
			index[c.ObjectID] = i
			if policy == model.DuplicateReject {
				result.Outcomes[i] = model.MemberDuplicate
			} else {
				result.Outcomes[i] = model.MemberSkipped
			}
		}
		onConflict := `DO NOTHING`
		if policy == model.DuplicateReplace {
			onConflict = `DO UPDATE SET asset_id = EXCLUDED.asset_id, label = EXCLUDED.label, split = EXCLUDED.split,
			                  metadata = EXCLUDED.metadata, source = EXCLUDED.source, added_by = EXCLUDED.added_by,
			                  updated_at = EXCLUDED.updated_at
			              WHERE (dataset_members.asset_id, dataset_members.label, dataset_members.split, dataset_members.metadata)
			                    IS DISTINCT FROM (EXCLUDED.asset_id, EXCLUDED.label, EXCLUDED.split, EXCLUDED.metadata)`
		}
		rows, err := tx.Query(ctx, `
			INSERT INTO dataset_members (id, tenant_id, dataset_id, object_id, asset_id, label, split, metadata,
			                             source, added_by, created_at, updated_at)
			SELECT c.id, $1, $2, c.object_id, NULLIF(c.asset_id, ''), NULLIF(c.label, ''), NULLIF(c.split, ''),
			       c.metadata::jsonb, $3, $4, $5, $5
			FROM unnest($6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[])
			     AS c(id, object_id, asset_id, label, split, metadata)
			ON CONFLICT (dataset_id, object_id) `+onConflict+`
			RETURNING object_id, xmax = 0
		`, tenantID, datasetID, source, actor, now, ids, objects, assets, labels, splits, metadata)
		if err != nil {
			return result, fmt.Errorf("add members: %w", err)
		}
		for rows.Next() {
			var objectID string
			var inserted bool
			if err := rows.Scan(&objectID, &inserted); err != nil {
				rows.Close()
				return result, err
			}
			if inserted {
				result.Outcomes[index[objectID]] = model.MemberAdded
				added++
			} else {
				result.Outcomes[index[objectID]] = model.MemberUpdated
				updated++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return result, fmt.Errorf("add members: %w", err)
		}

		if added+updated > 0 {
			result.Dataset, err = scanDataset(tx.QueryRow(ctx, `
				UPDATE datasets SET member_count = member_count + $3, members_revision = members_revision + 1,
				                    revision = revision + 1, lifecycle_status = `+leaveReady+`,
				                    updated_by = $4, updated_at = $5
				WHERE tenant_id = $1 AND id = $2
				RETURNING `+datasetColumns, tenantID, datasetID, added, actor, now))
			if err != nil {
				return result, fmt.Errorf("count members: %w", err)
			}
		}
	}

	if cp != nil {
		rejected := slices.Clone(cp.Rejected)
		var skipped int64
		for i, outcome := range result.Outcomes {
			switch outcome {
			case model.MemberDuplicate:
				rejected = append(rejected, model.ImportError{Line: changes[i].Line, Code: model.LineDuplicate,
					Detail: "the object is already a member of the dataset"})
			case model.MemberSkipped:
				skipped++
			}
		}
		if err := writeImportProgress(ctx, tx, cp, added, updated, skipped, rejected, now); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit add members: %w", err)
	}
	return result, nil
}

func (r *DatasetRepository) RemoveMember(ctx context.Context, tenantID, datasetID, memberID, actor string, now time.Time) (model.Dataset, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.Dataset{}, fmt.Errorf("begin remove member: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := lockDraft(ctx, tx, tenantID, datasetID); err != nil {
		return model.Dataset{}, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM dataset_members WHERE tenant_id = $1 AND dataset_id = $2 AND id = $3`,
		tenantID, datasetID, memberID)
	if err != nil {
		return model.Dataset{}, fmt.Errorf("remove member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.Dataset{}, model.ErrMemberNotFound
	}
	d, err := scanDataset(tx.QueryRow(ctx, `
		UPDATE datasets SET member_count = member_count - 1, members_revision = members_revision + 1,
		                    revision = revision + 1, lifecycle_status = `+leaveReady+`, updated_by = $3, updated_at = $4
		WHERE tenant_id = $1 AND id = $2
		RETURNING `+datasetColumns, tenantID, datasetID, actor, now))
	if err != nil {
		return model.Dataset{}, fmt.Errorf("count members: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Dataset{}, fmt.Errorf("commit remove member: %w", err)
	}
	return d, nil
}

// ListMembers pages a dataset's draft by member ID, with each object's
// namespace and path.
func (r *DatasetRepository) ListMembers(ctx context.Context, tenantID, datasetID, after string, limit int) (model.MemberPage, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT m.id, m.tenant_id, m.dataset_id, m.object_id, COALESCE(m.asset_id, ''), COALESCE(m.label, ''),
		       COALESCE(m.split, ''), m.metadata, m.source, m.added_by, m.created_at, m.updated_at, o.dataset, o.path
		FROM dataset_members AS m
		JOIN catalog_objects AS o ON o.id = m.object_id AND o.tenant_id = m.tenant_id
		WHERE m.tenant_id = $1 AND m.dataset_id = $2 AND m.id > $3
		ORDER BY m.id
		LIMIT $4
	`, tenantID, datasetID, after, limit+1)
	if err != nil {
		return model.MemberPage{}, fmt.Errorf("list members: %w", err)
	}
	members, err := collect(rows, func(row pgx.Row) (model.DatasetMember, error) {
		var m model.DatasetMember
		var metadata []byte
		err := row.Scan(&m.ID, &m.TenantID, &m.DatasetID, &m.ObjectID, &m.AssetID, &m.Label, &m.Split, &metadata,
			&m.Source, &m.AddedBy, &m.CreatedAt, &m.UpdatedAt, &m.Namespace, &m.Path)
		if err != nil {
			return m, err
		}
		m.Metadata, err = decodeMetadata(metadata)
		return m, err
	})
	if err != nil {
		return model.MemberPage{}, err
	}
	page := model.MemberPage{Members: members}
	if len(members) > limit {
		page.Members = members[:limit]
		page.After = page.Members[limit-1].ID
	}
	return page, nil
}

// ─── Versions ─────────────────────────────────────────────────────────

const versionColumns = `id, tenant_id, dataset_id, version, status, validation_status, member_count, total_bytes,
	manifest_sha256, manifest_format, members_revision, COALESCE(profile_id, ''), profile_version, config,
	COALESCE(parent_version_id, ''), derivation, COALESCE(latest_report_id, ''), COALESCE(idempotency_key, ''),
	created_by, created_at, expired_at, COALESCE(superseded_by, ''), COALESCE(superseded_reason, '')`

func scanVersion(row pgx.Row) (model.DatasetVersion, error) {
	var v model.DatasetVersion
	var config, derivation []byte
	err := row.Scan(&v.ID, &v.TenantID, &v.DatasetID, &v.Version, &v.Status, &v.ValidationStatus, &v.MemberCount,
		&v.TotalBytes, &v.ManifestSHA256, &v.ManifestFormat, &v.MembersRevision, &v.ProfileID, &v.ProfileVersion,
		&config, &v.ParentVersionID, &derivation, &v.LatestReportID, &v.IdempotencyKey, &v.CreatedBy, &v.CreatedAt, &v.ExpiredAt,
		&v.SupersededBy, &v.SupersededReason)
	if err != nil {
		return v, err
	}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &v.Config); err != nil {
			return v, fmt.Errorf("decode version config: %w", err)
		}
	}
	if len(derivation) > 0 {
		v.Derivation = &model.Derivation{}
		if err := json.Unmarshal(derivation, v.Derivation); err != nil {
			return v, fmt.Errorf("decode version derivation: %w", err)
		}
	}
	return v, nil
}

// CreateSnapshot fixes the dataset's draft as its next version, in one
// transaction under the dataset's lock: the version's members are copied
// by the database from the draft (each pinned version, or the object's
// current one), then read back in canonical order to hash, so no list of
// members is held in memory. An idempotency key used before returns the
// version it made.
func (r *DatasetRepository) CreateSnapshot(ctx context.Context, in interfaces.SnapshotInput) (model.DatasetVersion, bool, error) {
	if in.IdempotencyKey != "" {
		v, err := r.versionByKey(ctx, r.db.Pool, in.TenantID, in.DatasetID, in.IdempotencyKey)
		if err == nil || !errors.Is(err, model.ErrDatasetVersionNotFound) {
			return v, false, err
		}
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.DatasetVersion{}, false, fmt.Errorf("begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	d, err := lockDraft(ctx, tx, in.TenantID, in.DatasetID)
	if err != nil {
		return model.DatasetVersion{}, false, err
	}
	if in.IdempotencyKey != "" {
		// A request with the same key that committed while this one waited
		// for the lock.
		v, err := r.versionByKey(ctx, tx, in.TenantID, in.DatasetID, in.IdempotencyKey)
		if err == nil || !errors.Is(err, model.ErrDatasetVersionNotFound) {
			return v, false, err
		}
	}
	if d.MemberCount == 0 {
		return model.DatasetVersion{}, false, model.ErrNoMembers
	}
	if in.MaxMembers > 0 && d.MemberCount > in.MaxMembers {
		return model.DatasetVersion{}, false, fmt.Errorf("%w (%d members, at most %d)", model.ErrSnapshotTooLarge, d.MemberCount, in.MaxMembers)
	}
	var unresolved int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM dataset_members AS m
		JOIN catalog_objects AS o ON o.id = m.object_id AND o.tenant_id = m.tenant_id
		WHERE m.tenant_id = $1 AND m.dataset_id = $2 AND m.asset_id IS NULL AND o.current_asset_id IS NULL
	`, in.TenantID, in.DatasetID).Scan(&unresolved); err != nil {
		return model.DatasetVersion{}, false, fmt.Errorf("check members: %w", err)
	}
	if unresolved > 0 {
		return model.DatasetVersion{}, false, fmt.Errorf("%w: %d members have no current version and are not pinned", model.ErrUnresolvedMembers, unresolved)
	}

	// The dataset's configuration as the version was taken.
	config, err := json.Marshal(map[string]any{
		"name": d.Name, "description": d.Description, "tags": nonNilStrings(d.Tags), "metadata": nonNilMetadata(d.Metadata),
	})
	if err != nil {
		return model.DatasetVersion{}, false, err
	}
	var derivation []byte
	if d.Derivation != nil {
		if derivation, err = json.Marshal(d.Derivation); err != nil {
			return model.DatasetVersion{}, false, err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO dataset_versions (id, tenant_id, dataset_id, version, status, member_count, total_bytes, manifest_sha256,
		                              manifest_format, members_revision, profile_id, profile_version, config,
		                              parent_version_id, derivation, idempotency_key, created_by, created_at)
		VALUES ($1, $2, $3, $4, 'FINALIZED', 0, 0, '', $5, $6, NULLIF($7, ''), $8, $9, NULLIF($10, ''), $11, NULLIF($12, ''), $13, $14)
	`, in.VersionID, in.TenantID, in.DatasetID, d.NextVersion, model.ManifestFormatV1, d.MembersRevision, d.ProfileID,
		d.ProfileVersion, config, d.CurrentVersionID, derivation, in.IdempotencyKey, in.Actor, in.Now); err != nil {
		return model.DatasetVersion{}, false, fmt.Errorf("create version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO dataset_version_members (version_id, tenant_id, ordinal, object_id, asset_id, object_version, path,
		                                     label, split, size_bytes, content_type, checksum_sha256, metadata, member_metadata)
		SELECT $1, m.tenant_id, row_number() OVER (ORDER BY m.object_id), m.object_id, a.id, a.version, o.path,
		       m.label, m.split, a.size_bytes, a.content_type, a.checksum_sha256,
		       COALESCE(a.metadata, '{}'::jsonb) || m.metadata, m.metadata
		FROM dataset_members AS m
		JOIN catalog_objects AS o ON o.id = m.object_id AND o.tenant_id = m.tenant_id
		JOIN assets AS a ON a.id = COALESCE(m.asset_id, o.current_asset_id) AND a.tenant_id = m.tenant_id
		WHERE m.tenant_id = $2 AND m.dataset_id = $3
	`, in.VersionID, in.TenantID, in.DatasetID); err != nil {
		return model.DatasetVersion{}, false, fmt.Errorf("copy members: %w", err)
	}

	sum, count, total, err := hashVersion(ctx, tx, in.VersionID)
	if err != nil {
		return model.DatasetVersion{}, false, err
	}
	v, err := scanVersion(tx.QueryRow(ctx, `
		UPDATE dataset_versions SET manifest_sha256 = $2, member_count = $3, total_bytes = $4
		WHERE id = $1
		RETURNING `+versionColumns, in.VersionID, sum, count, total))
	if err != nil {
		return model.DatasetVersion{}, false, fmt.Errorf("seal version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE datasets SET current_version_id = $3, next_version = next_version + 1, revision = revision + 1,
		                    lifecycle_status = `+leaveReady+`, updated_by = $4, updated_at = $5
		WHERE tenant_id = $1 AND id = $2
	`, in.TenantID, in.DatasetID, v.ID, in.Actor, in.Now); err != nil {
		return model.DatasetVersion{}, false, fmt.Errorf("make version current: %w", err)
	}
	if err := r.linkVersion(ctx, tx, d, v); err != nil {
		return model.DatasetVersion{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) && in.IdempotencyKey != "" {
			v, err := r.versionByKey(ctx, r.db.Pool, in.TenantID, in.DatasetID, in.IdempotencyKey)
			return v, false, err
		}
		return model.DatasetVersion{}, false, fmt.Errorf("commit snapshot: %w", err)
	}
	return v, true, nil
}

// hashVersion streams a version's members in canonical order through
// SHA-256, adding up their count and bytes as it goes.
func hashVersion(ctx context.Context, tx pgx.Tx, versionID string) (string, int64, int64, error) {
	rows, err := tx.Query(ctx, `SELECT `+versionMemberColumns+` FROM dataset_version_members WHERE version_id = $1 ORDER BY ordinal`, versionID)
	if err != nil {
		return "", 0, 0, fmt.Errorf("read version members: %w", err)
	}
	defer rows.Close()
	h := sha256.New()
	var count, total int64
	for rows.Next() {
		m, err := scanVersionMember(rows)
		if err != nil {
			return "", 0, 0, err
		}
		h.Write(m.CanonicalLine())
		count++
		total += m.SizeBytes
	}
	if err := rows.Err(); err != nil {
		return "", 0, 0, fmt.Errorf("read version members: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), count, total, nil
}

// linkVersion records a new version in the lineage graph: its dataset
// contains it and, for a derived dataset, it derives from the source
// versions, through the transformation if one is named.
func (r *DatasetRepository) linkVersion(ctx context.Context, tx pgx.Tx, d model.Dataset, v model.DatasetVersion) error {
	versionRef := model.LineageRef{Kind: model.NodeDatasetVersion, RefID: v.ID}
	err := link(ctx, tx, d.TenantID, model.LineageEdge{
		From: model.LineageRef{Kind: model.NodeDataset, RefID: d.ID}, To: versionRef, Relation: model.RelContains,
		Actor: v.CreatedBy, Details: map[string]any{"version": v.Version, "manifest_sha256": v.ManifestSHA256, "members": v.MemberCount},
	}, v.CreatedAt, true)
	if err != nil || d.Derivation == nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, manifest_sha256 FROM dataset_versions WHERE tenant_id = $1 AND id = ANY($2)`,
		d.TenantID, d.Derivation.SourceVersionIDs)
	if err != nil {
		return fmt.Errorf("read source versions: %w", err)
	}
	inputs := map[string]string{}
	for rows.Next() {
		var id, sum string
		if err := rows.Scan(&id, &sum); err != nil {
			rows.Close()
			return err
		}
		inputs[id] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, source := range d.Derivation.SourceVersionIDs {
		details := map[string]any{"input_manifest_sha256": inputs[source], "output_manifest_sha256": v.ManifestSHA256}
		if t := d.Derivation.Transformation; t != nil {
			details["transformation"] = t.Name + "@" + t.Version
		}
		err := link(ctx, tx, d.TenantID, model.LineageEdge{
			From: versionRef, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: source},
			Relation: model.RelDerivedFrom, Actor: v.CreatedBy, Details: details,
		}, v.CreatedAt, true)
		if err != nil {
			return err
		}
	}
	if t := d.Derivation.Transformation; t != nil {
		return link(ctx, tx, d.TenantID, model.LineageEdge{
			From: versionRef, To: transformationRef(*t), Relation: model.RelTransformedBy, Actor: v.CreatedBy,
			Details: transformationDetails(*t),
		}, v.CreatedAt, true)
	}
	return nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (r *DatasetRepository) versionByKey(ctx context.Context, q querier, tenantID, datasetID, key string) (model.DatasetVersion, error) {
	v, err := scanVersion(q.QueryRow(ctx, `
		SELECT `+versionColumns+` FROM dataset_versions WHERE tenant_id = $1 AND dataset_id = $2 AND idempotency_key = $3
	`, tenantID, datasetID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return v, model.ErrDatasetVersionNotFound
	}
	if err != nil {
		return v, fmt.Errorf("find version by key: %w", err)
	}
	return v, nil
}

func (r *DatasetRepository) GetVersion(ctx context.Context, tenantID, versionID string) (model.DatasetVersion, error) {
	v, err := scanVersion(r.db.Pool.QueryRow(ctx,
		`SELECT `+versionColumns+` FROM dataset_versions WHERE tenant_id = $1 AND id = $2`, tenantID, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return v, model.ErrDatasetVersionNotFound
	}
	if err != nil {
		return v, fmt.Errorf("get version: %w", err)
	}
	return v, nil
}

// ListVersions pages a dataset's versions, newest first: those numbered
// below before (0: from the newest).
func (r *DatasetRepository) ListVersions(ctx context.Context, tenantID, datasetID string, before, limit int) ([]model.DatasetVersion, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+versionColumns+` FROM dataset_versions
		WHERE tenant_id = $1 AND dataset_id = $2 AND ($3 = 0 OR version < $3)
		ORDER BY version DESC
		LIMIT $4
	`, tenantID, datasetID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	return collect(rows, scanVersion)
}

const versionMemberColumns = `ordinal, object_id, asset_id, object_version, path, COALESCE(label, ''), COALESCE(split, ''),
	size_bytes, content_type, COALESCE(checksum_sha256, ''), metadata`

func scanVersionMember(row pgx.Row) (model.VersionMember, error) {
	var m model.VersionMember
	var metadata []byte
	err := row.Scan(&m.Ordinal, &m.ObjectID, &m.AssetID, &m.ObjectVersion, &m.Path, &m.Label, &m.Split,
		&m.SizeBytes, &m.ContentType, &m.ChecksumSHA256, &metadata)
	if err != nil {
		return m, err
	}
	m.Metadata, err = decodeMetadata(metadata)
	return m, err
}

// ListVersionMembers pages a version's members in canonical order, with
// whether each member's pinned bytes can still be read.
func (r *DatasetRepository) ListVersionMembers(ctx context.Context, tenantID, versionID string, after int64, limit int) ([]model.VersionMember, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT vm.ordinal, vm.object_id, vm.asset_id, vm.object_version, vm.path, COALESCE(vm.label, ''), COALESCE(vm.split, ''),
		       vm.size_bytes, vm.content_type, COALESCE(vm.checksum_sha256, ''), vm.metadata,
		       COALESCE(a.status, 'DELETED'), a.updated_at
		FROM dataset_version_members AS vm
		LEFT JOIN assets AS a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id
		WHERE vm.tenant_id = $1 AND vm.version_id = $2 AND vm.ordinal > $3
		ORDER BY vm.ordinal
		LIMIT $4
	`, tenantID, versionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list version members: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.VersionMember, error) {
		var m model.VersionMember
		var metadata []byte
		var status string
		var changedAt *time.Time
		err := row.Scan(&m.Ordinal, &m.ObjectID, &m.AssetID, &m.ObjectVersion, &m.Path, &m.Label, &m.Split,
			&m.SizeBytes, &m.ContentType, &m.ChecksumSHA256, &metadata, &status, &changedAt)
		if err != nil {
			return m, err
		}
		switch model.AssetStatus(status) {
		case model.AssetStatusAvailable:
			m.Availability = model.MemberAvailable
		case model.AssetStatusSoftDeleted:
			m.Availability = model.MemberDeletionPending
		default:
			// DELETED, or the asset row is gone: the bytes are erased.
			m.Availability = model.MemberErased
			m.ErasedAt = changedAt
		}
		m.Metadata, err = decodeMetadata(metadata)
		return m, err
	})
}
