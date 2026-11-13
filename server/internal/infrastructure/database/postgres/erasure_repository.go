package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ErasureRepository finds what an erasure reaches and records its
// requests, items, successor snapshots and certificates (docs/erasure.md).
type ErasureRepository struct {
	db *DB
}

func NewErasureRepository(db *DB) *ErasureRepository {
	return &ErasureRepository{db: db}
}

// ─── The plan ─────────────────────────────────────────────────────────

// Matches finds the assets an erasure reaches: those naming a subject
// (object metadata, member metadata in snapshots and drafts), those with
// a listed SHA-256, every version of a listed object, and then every
// asset with the same SHA-256 as any of them: each exact copy, on any
// connection. Deleted assets are included, so their memberships go too.
func (r *ErasureRepository) Matches(ctx context.Context, tenantID string, sel model.ErasureSelector) ([]string, error) {
	subjects, hashes, objects := orEmptyStrings(sel.SubjectIDs), orEmptyStrings(sel.SHA256s), orEmptyStrings(sel.ObjectIDs)
	return collectStrings(r.db.Pool.Query(ctx, `
		WITH seed AS (
		    SELECT a.id FROM assets a
		    WHERE a.tenant_id = $1 AND (a.checksum_sha256 = ANY($2) OR a.object_id = ANY($3)
		        OR (cardinality($4::text[]) > 0 AND a.metadata->>$5 = ANY($4)))
		    UNION
		    SELECT m.asset_id FROM dataset_version_members m
		    WHERE m.tenant_id = $1 AND cardinality($4::text[]) > 0 AND COALESCE(m.member_metadata->>$5, m.metadata->>$5) = ANY($4)
		    UNION
		    SELECT COALESCE(dm.asset_id, o.current_asset_id) FROM dataset_members dm
		    JOIN catalog_objects o ON o.tenant_id = dm.tenant_id AND o.id = dm.object_id
		    WHERE dm.tenant_id = $1 AND cardinality($4::text[]) > 0 AND dm.metadata->>$5 = ANY($4)
		        AND COALESCE(dm.asset_id, o.current_asset_id) IS NOT NULL
		), hashes AS (
		    SELECT DISTINCT a.checksum_sha256 FROM assets a WHERE a.id IN (SELECT id FROM seed) AND a.checksum_sha256 IS NOT NULL
		)
		SELECT id FROM seed
		UNION
		SELECT a.id FROM assets a WHERE a.tenant_id = $1 AND a.checksum_sha256 IN (SELECT checksum_sha256 FROM hashes)
		ORDER BY 1`, tenantID, hashes, objects, subjects, sel.SubjectKey))
}

func orEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Objects lists the matched assets that still have bytes, with where.
func (r *ErasureRepository) Objects(ctx context.Context, tenantID string, assetIDs []string) ([]model.PlanObject, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT a.id, COALESCE(a.object_id, ''), COALESCE(a.version, 0), COALESCE(o.path, a.file_name), COALESCE(a.checksum_sha256, ''),
		       a.size_bytes, COALESCE(a.storage_connection_id, 'platform'), a.status
		FROM assets a LEFT JOIN catalog_objects o ON o.tenant_id = a.tenant_id AND o.id = a.object_id
		WHERE a.tenant_id = $1 AND a.id = ANY($2) AND a.status <> 'DELETED'
		ORDER BY 4, 3`, tenantID, assetIDs)
	if err != nil {
		return nil, fmt.Errorf("list erasure objects: %w", err)
	}
	defer rows.Close()
	out := []model.PlanObject{}
	for rows.Next() {
		var o model.PlanObject
		if err := rows.Scan(&o.AssetID, &o.ObjectID, &o.Version, &o.Path, &o.SHA256, &o.SizeBytes, &o.ConnectionID, &o.Status); err != nil {
			return nil, err
		}
		o.HeldBy = []string{}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Versions lists the snapshots pinning matched assets, oldest first.
func (r *ErasureRepository) Versions(ctx context.Context, tenantID string, assetIDs []string) ([]model.PlanVersion, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT v.dataset_id, d.name, v.id, v.version, count(*), COALESCE(d.current_version_id = v.id, false), COALESCE(v.superseded_by, '')
		FROM dataset_version_members m
		JOIN dataset_versions v ON v.tenant_id = m.tenant_id AND v.id = m.version_id
		JOIN datasets d ON d.tenant_id = v.tenant_id AND d.id = v.dataset_id
		WHERE m.tenant_id = $1 AND m.asset_id = ANY($2)
		GROUP BY v.dataset_id, d.name, v.id, v.version, d.current_version_id, v.superseded_by
		ORDER BY d.name, v.version`, tenantID, assetIDs)
	if err != nil {
		return nil, fmt.Errorf("list pinning versions: %w", err)
	}
	defer rows.Close()
	out := []model.PlanVersion{}
	for rows.Next() {
		var v model.PlanVersion
		if err := rows.Scan(&v.DatasetID, &v.DatasetName, &v.VersionID, &v.Version, &v.Members, &v.Current, &v.SupersededBy); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Drafts lists datasets whose draft holds matched assets (pinned, or
// following an object whose current version is one).
func (r *ErasureRepository) Drafts(ctx context.Context, tenantID string, assetIDs []string) ([]model.PlanDraft, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT d.id, d.name, count(*)
		FROM dataset_members dm
		JOIN catalog_objects o ON o.tenant_id = dm.tenant_id AND o.id = dm.object_id
		JOIN datasets d ON d.tenant_id = dm.tenant_id AND d.id = dm.dataset_id
		WHERE dm.tenant_id = $1 AND COALESCE(dm.asset_id, o.current_asset_id) = ANY($2)
		GROUP BY d.id, d.name ORDER BY d.name`, tenantID, assetIDs)
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	defer rows.Close()
	out := []model.PlanDraft{}
	for rows.Next() {
		var d model.PlanDraft
		if err := rows.Scan(&d.DatasetID, &d.DatasetName, &d.Members); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Models lists the models of runs that read any of the versions.
func (r *ErasureRepository) Models(ctx context.Context, tenantID string, versionIDs []string) ([]model.PlanModel, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT ON (mo.id) mo.id, mo.name, mo.version, t.id, t.name, i.version_id
		FROM training_run_inputs i
		JOIN training_runs t ON t.tenant_id = i.tenant_id AND t.id = i.run_id
		JOIN models mo ON mo.tenant_id = t.tenant_id AND mo.run_id = t.id
		WHERE i.tenant_id = $1 AND i.version_id = ANY($2)
		ORDER BY mo.id`, tenantID, versionIDs)
	if err != nil {
		return nil, fmt.Errorf("list trained models: %w", err)
	}
	defer rows.Close()
	out := []model.PlanModel{}
	for rows.Next() {
		var m model.PlanModel
		if err := rows.Scan(&m.ID, &m.Name, &m.Version, &m.RunID, &m.RunName, &m.VersionID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ─── Executing ────────────────────────────────────────────────────────

// Successor makes a new version of a snapshot's dataset with every member
// except the erased assets, in the same order, and marks the snapshot
// superseded by it. A successor of the dataset's current version becomes
// current. It returns the successor, or found false when the snapshot
// no longer pins any of them or is superseded already.
func (r *ErasureRepository) Successor(ctx context.Context, tenantID, versionID string, erased []string, erasureID, actor, successorID string,
	now time.Time) (model.Successor, bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return model.Successor{}, false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var datasetID, superseded string
	var version int
	if err := tx.QueryRow(ctx, `SELECT dataset_id, version, COALESCE(superseded_by, '') FROM dataset_versions
		WHERE tenant_id = $1 AND id = $2`, tenantID, versionID).Scan(&datasetID, &version, &superseded); err != nil {
		return model.Successor{}, false, fmt.Errorf("read version: %w", err)
	}
	// Locked without lockDraft's archive check: an erasure supersedes
	// snapshots of archived datasets too.
	var nextVersion int
	var currentVersionID string
	err = tx.QueryRow(ctx, `SELECT next_version, COALESCE(current_version_id, '') FROM datasets WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, datasetID).Scan(&nextVersion, &currentVersionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Successor{}, false, model.ErrDatasetNotFound
	}
	if err != nil {
		return model.Successor{}, false, fmt.Errorf("lock dataset: %w", err)
	}
	var removed int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2 AND asset_id = ANY($3)`,
		tenantID, versionID, erased).Scan(&removed); err != nil {
		return model.Successor{}, false, err
	}
	if superseded != "" || removed == 0 {
		return model.Successor{}, false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO dataset_versions (id, tenant_id, dataset_id, version, status, member_count, total_bytes, manifest_sha256,
		                              manifest_format, members_revision, profile_id, profile_version, config, parent_version_id,
		                              created_by, created_at)
		SELECT $3, tenant_id, dataset_id, $4, 'FINALIZED', 0, 0, '', manifest_format, members_revision, profile_id, profile_version,
		       config, id, $5, $6
		FROM dataset_versions WHERE tenant_id = $1 AND id = $2`, tenantID, versionID, successorID, nextVersion, actor, now); err != nil {
		return model.Successor{}, false, fmt.Errorf("create successor: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO dataset_version_members (version_id, tenant_id, ordinal, object_id, asset_id, object_version, path,
		                                     label, split, size_bytes, content_type, checksum_sha256, metadata, member_metadata)
		SELECT $3, tenant_id, row_number() OVER (ORDER BY ordinal), object_id, asset_id, object_version, path,
		       label, split, size_bytes, content_type, checksum_sha256, metadata, member_metadata
		FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2 AND asset_id <> ALL($4)`,
		tenantID, versionID, successorID, erased); err != nil {
		return model.Successor{}, false, fmt.Errorf("copy kept members: %w", err)
	}
	sum, count, total, err := hashVersion(ctx, tx, successorID)
	if err != nil {
		return model.Successor{}, false, err
	}
	v, err := scanVersion(tx.QueryRow(ctx, `UPDATE dataset_versions SET manifest_sha256 = $2, member_count = $3, total_bytes = $4
		WHERE id = $1 RETURNING `+versionColumns, successorID, sum, count, total))
	if err != nil {
		return model.Successor{}, false, fmt.Errorf("seal successor: %w", err)
	}
	reason := "erasure " + erasureID
	// The superseded check above ran before the dataset lock; a successor
	// made meanwhile wins, and this one is rolled back.
	tag, err := tx.Exec(ctx, `UPDATE dataset_versions SET superseded_by = $3, superseded_at = $4, superseded_reason = $5
		WHERE tenant_id = $1 AND id = $2 AND superseded_at IS NULL`, tenantID, versionID, successorID, now, reason)
	if err != nil {
		return model.Successor{}, false, fmt.Errorf("supersede version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.Successor{}, false, nil
	}
	current := ""
	if currentVersionID == versionID {
		current = successorID
	}
	if _, err := tx.Exec(ctx, `UPDATE datasets SET next_version = next_version + 1, current_version_id = COALESCE(NULLIF($3, ''), current_version_id),
		updated_by = $4, updated_at = $5 WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID, current, actor, now); err != nil {
		return model.Successor{}, false, fmt.Errorf("advance dataset: %w", err)
	}
	ref := model.LineageRef{Kind: model.NodeDatasetVersion, RefID: successorID}
	if err := link(ctx, tx, tenantID, model.LineageEdge{From: model.LineageRef{Kind: model.NodeDataset, RefID: datasetID}, To: ref,
		Relation: model.RelContains, Actor: actor, Details: map[string]any{"version": v.Version, "manifest_sha256": v.ManifestSHA256,
			"members": v.MemberCount}}, now, true); err != nil {
		return model.Successor{}, false, err
	}
	if err := link(ctx, tx, tenantID, model.LineageEdge{From: ref, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: versionID},
		Relation: model.RelDerivedFrom, Actor: actor, Details: map[string]any{"erasure_id": erasureID, "removed": removed,
			"output_manifest_sha256": v.ManifestSHA256}}, now, true); err != nil {
		return model.Successor{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Successor{}, false, fmt.Errorf("commit successor: %w", err)
	}
	return model.Successor{DatasetID: datasetID, VersionID: versionID, Version: version, SuccessorID: successorID,
		SuccessorVersion: v.Version, Removed: removed}, true, nil
}

// SuccessorsOf lists the snapshots an erasure superseded, read back from
// the versions themselves, so a preparation run again (or two at once)
// reports every successor, whichever run made it.
func (r *ErasureRepository) SuccessorsOf(ctx context.Context, tenantID, erasureID string) ([]model.Successor, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT v.dataset_id, v.id, v.version, s.id, s.version, v.member_count - s.member_count
		FROM dataset_versions v JOIN dataset_versions s ON s.tenant_id = v.tenant_id AND s.id = v.superseded_by
		WHERE v.tenant_id = $1 AND v.superseded_reason = $2
		ORDER BY v.dataset_id, v.version`, tenantID, "erasure "+erasureID)
	if err != nil {
		return nil, fmt.Errorf("list successors: %w", err)
	}
	defer rows.Close()
	out := []model.Successor{}
	for rows.Next() {
		var s model.Successor
		if err := rows.Scan(&s.DatasetID, &s.VersionID, &s.Version, &s.SuccessorID, &s.SuccessorVersion, &s.Removed); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RemoveFromDrafts takes erased assets out of every draft: members pinned
// to one, or following an object whose current version is one. Run again,
// it finds nothing left to remove.
func (r *ErasureRepository) RemoveFromDrafts(ctx context.Context, tenantID string, erased []string, actor string, now time.Time) (int64, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	removed, err := collectStrings(tx.Query(ctx, `
		DELETE FROM dataset_members dm USING catalog_objects o
		WHERE dm.tenant_id = $1 AND o.tenant_id = dm.tenant_id AND o.id = dm.object_id
		  AND COALESCE(dm.asset_id, o.current_asset_id) = ANY($2)
		RETURNING dm.dataset_id`, tenantID, erased))
	if err != nil {
		return 0, fmt.Errorf("remove draft members: %w", err)
	}
	// One row per removed member, so a dataset appears once per member.
	if _, err := tx.Exec(ctx, `UPDATE datasets d SET member_count = d.member_count - c.n, members_revision = d.members_revision + 1,
		    revision = d.revision + 1, updated_by = $3, updated_at = $4
		FROM (SELECT id, count(*) AS n FROM unnest($2::text[]) AS id GROUP BY id) AS c
		WHERE d.tenant_id = $1 AND d.id = c.id`, tenantID, removed, actor, now); err != nil {
		return 0, fmt.Errorf("update drafts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int64(len(removed)), nil
}

// Expedite makes the erased assets' deletions due now, without their
// grace period, under the reason ERASURE. Blockers are checked again
// when each runs: a hold still holds it back.
func (r *ErasureRepository) Expedite(ctx context.Context, tenantID string, assetIDs []string, erasureID string, now time.Time) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE deletion_operations SET reason = 'ERASURE', purge_after = LEAST(purge_after, $3),
		next_attempt_at = LEAST(next_attempt_at, $3), policy_detail = $4, updated_at = $3
		WHERE tenant_id = $1 AND asset_id = ANY($2) AND state IN ('SCHEDULED', 'PROTECTED')`,
		tenantID, assetIDs, now, "erasure "+erasureID)
	if err != nil {
		return 0, fmt.Errorf("expedite deletions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeletionState is the newest deletion of an asset, and its certificate.
type DeletionState struct {
	AssetID       string
	AssetStatus   string
	DeletionID    string
	State         string
	Detail        string
	CertificateID string
	UpdatedAt     *time.Time
}

func (r *ErasureRepository) DeletionStates(ctx context.Context, tenantID string, assetIDs []string) ([]DeletionState, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT a.id, a.status, COALESCE(op.id, ''), COALESCE(op.state, ''), COALESCE(op.protected_reason, op.last_error, ''),
		       COALESCE(c.id, ''), op.updated_at
		FROM assets a
		LEFT JOIN LATERAL (SELECT * FROM deletion_operations d WHERE d.tenant_id = a.tenant_id AND d.asset_id = a.id
		                   ORDER BY d.created_at DESC LIMIT 1) op ON true
		LEFT JOIN disposition_certificates c ON c.tenant_id = a.tenant_id AND c.deletion_id = op.id
		WHERE a.tenant_id = $1 AND a.id = ANY($2)`, tenantID, assetIDs)
	if err != nil {
		return nil, fmt.Errorf("read deletion states: %w", err)
	}
	defer rows.Close()
	var out []DeletionState
	for rows.Next() {
		var d DeletionState
		if err := rows.Scan(&d.AssetID, &d.AssetStatus, &d.DeletionID, &d.State, &d.Detail, &d.CertificateID, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Certificate is a per-asset disposition certificate's ID and body hash.
func (r *ErasureRepository) Certificate(ctx context.Context, tenantID, id string) (string, error) {
	var sum string
	err := r.db.Pool.QueryRow(ctx, `SELECT body_sha256 FROM disposition_certificates WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return sum, err
}

// TombstoneSeq is the audit chain position of an erasure's tombstone.
func (r *ErasureRepository) TombstoneSeq(ctx context.Context, tenantID, erasureID string) (int64, error) {
	var seq int64
	err := r.db.Pool.QueryRow(ctx, `SELECT COALESCE(max(seq), 0) FROM audit_events
		WHERE chain = $1 AND action = 'erasure.executed' AND resource_id = $2`, tenantID, erasureID).Scan(&seq)
	return seq, err
}

// ─── Requests and items ───────────────────────────────────────────────

const erasureColumns = `id, tenant_id, kind, state, reason, reference, institution, subject_ids, subject_hashes, sha256s, object_ids,
	subject_key, plan, successors, requested_by, COALESCE(approved_by, ''), approved_note, approved_at, COALESCE(executed_by, ''),
	executed_at, prepared_at, completed_at, note, certificate_body IS NOT NULL, created_at, updated_at`

func scanErasure(row pgx.Row) (model.Erasure, error) {
	var e model.Erasure
	var subjects, hashes, sums, objects, plan, successors []byte
	err := row.Scan(&e.ID, &e.TenantID, &e.Kind, &e.State, &e.Reason, &e.Reference, &e.Institution, &subjects, &hashes, &sums, &objects,
		&e.Selector.SubjectKey, &plan, &successors, &e.RequestedBy, &e.ApprovedBy, &e.ApprovedNote, &e.ApprovedAt, &e.ExecutedBy,
		&e.ExecutedAt, &e.PreparedAt, &e.CompletedAt, &e.Note, &e.Certified, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return e, err
	}
	for _, f := range []struct {
		raw  []byte
		into any
	}{{subjects, &e.Selector.SubjectIDs}, {hashes, &e.SubjectHashes}, {sums, &e.Selector.SHA256s}, {objects, &e.Selector.ObjectIDs},
		{plan, &e.Plan}, {successors, &e.Successors}} {
		if err := json.Unmarshal(f.raw, f.into); err != nil {
			return e, fmt.Errorf("decode erasure: %w", err)
		}
	}
	return e, nil
}

func jsonOf(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (r *ErasureRepository) Create(ctx context.Context, e model.Erasure) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO erasure_requests (id, tenant_id, kind, state, reason, reference, institution,
		subject_ids, subject_hashes, sha256s, object_ids, subject_key, plan, requested_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)`,
		e.ID, e.TenantID, e.Kind, e.State, e.Reason, e.Reference, e.Institution, jsonOf(e.Selector.SubjectIDs), jsonOf(e.SubjectHashes),
		jsonOf(e.Selector.SHA256s), jsonOf(e.Selector.ObjectIDs), e.Selector.SubjectKey, jsonOf(e.Plan), e.RequestedBy, e.CreatedAt)
	if err != nil {
		return fmt.Errorf("record erasure: %w", err)
	}
	return nil
}

func (r *ErasureRepository) Get(ctx context.Context, tenantID, id string) (model.Erasure, error) {
	e, err := scanErasure(r.db.Pool.QueryRow(ctx, `SELECT `+erasureColumns+` FROM erasure_requests WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return e, model.ErrErasureNotFound
	}
	return e, err
}

func (r *ErasureRepository) List(ctx context.Context, tenantID, state string, limit int) ([]model.Erasure, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+erasureColumns+` FROM erasure_requests
		WHERE tenant_id = $1 AND ($2 = '' OR state = $2) ORDER BY created_at DESC LIMIT $3`, tenantID, state, limit)
	if err != nil {
		return nil, fmt.Errorf("list erasures: %w", err)
	}
	defer rows.Close()
	out := []model.Erasure{}
	for rows.Next() {
		e, err := scanErasure(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Open lists erasures still deleting, waiting or failed, for the worker,
// those it looked at longest ago first.
func (r *ErasureRepository) Open(ctx context.Context, limit int) ([]model.Erasure, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+erasureColumns+` FROM erasure_requests
		WHERE state IN ('DELETING', 'WAITING_ON_HOLD', 'FAILED') ORDER BY updated_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list open erasures: %w", err)
	}
	defer rows.Close()
	var out []model.Erasure
	for rows.Next() {
		e, err := scanErasure(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Touch marks an erasure looked at, so Open reaches the others before it
// comes round again.
func (r *ErasureRepository) Touch(ctx context.Context, tenantID, id string) error {
	if _, err := r.db.Pool.Exec(ctx, `UPDATE erasure_requests SET updated_at = now() WHERE tenant_id = $1 AND id = $2`, tenantID, id); err != nil {
		return fmt.Errorf("touch erasure: %w", err)
	}
	return nil
}

// Transition moves an erasure from one of the states to another,
// atomically; false when it was not in any of them. It writes only the
// fields a transition changes: the plan, the executor and the successors
// belong to Claim and SetPrepared, and a stale read must not undo them.
func (r *ErasureRepository) Transition(ctx context.Context, tenantID, id string, from []string, to string, change func(*model.Erasure)) (bool, error) {
	e, err := r.Get(ctx, tenantID, id)
	if err != nil {
		return false, err
	}
	change(&e)
	tag, err := r.db.Pool.Exec(ctx, `UPDATE erasure_requests SET state = $4, approved_by = NULLIF($5, ''),
		approved_note = $6, approved_at = $7, completed_at = $8, note = $9, subject_ids = $10, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND state = ANY($3)`,
		tenantID, id, from, to, e.ApprovedBy, e.ApprovedNote, e.ApprovedAt, e.CompletedAt, e.Note, jsonOf(e.Selector.SubjectIDs))
	if err != nil {
		return false, fmt.Errorf("update erasure: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Claim moves an erasure from one of the states to DELETING before any of
// its work is done, recording the plan and who executes it, and lists its
// objects as items in the same transaction, so the worker never sees a
// DELETING erasure without them. With secondPerson, only a request
// approved by someone other than its requester is claimed. False when the
// row did not qualify: cancelled, executed or unapproved meanwhile.
func (r *ErasureRepository) Claim(ctx context.Context, tenantID, id string, from []string, secondPerson bool, plan model.ErasurePlan,
	executor string, items []model.ErasureItem, now time.Time) (bool, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	tag, err := tx.Exec(ctx, `UPDATE erasure_requests SET state = 'DELETING', plan = $5, executed_by = $6, executed_at = $7, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND state = ANY($3)
		  AND (NOT $4 OR (approved_by IS NOT NULL AND approved_by <> requested_by))`,
		tenantID, id, from, secondPerson, jsonOf(plan), executor, now)
	if err != nil {
		return false, fmt.Errorf("claim erasure: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	for _, i := range items {
		if _, err := tx.Exec(ctx, `INSERT INTO erasure_items (erasure_id, tenant_id, asset_id, object_id, object_version, path, sha256,
			size_bytes, connection_id, state, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT (erasure_id, asset_id) DO NOTHING`,
			id, tenantID, i.AssetID, i.ObjectID, i.ObjectVersion, i.Path, i.SHA256, i.SizeBytes, i.ConnectionID, i.State, now); err != nil {
			return false, fmt.Errorf("record erasure items: %w", err)
		}
	}
	return true, tx.Commit(ctx)
}

// SetPrepared records the successor snapshots and marks the erasure
// prepared, once its drafts are emptied too. False when it was prepared
// already, by a concurrent run that recorded the same successors, or is
// no longer open. Rows from before migration 000054 may be waiting or
// failed and still unprepared, so those states qualify too.
func (r *ErasureRepository) SetPrepared(ctx context.Context, tenantID, id string, successors []model.Successor, at time.Time) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE erasure_requests SET successors = $3, prepared_at = $4, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND prepared_at IS NULL AND state IN ('DELETING', 'WAITING_ON_HOLD', 'FAILED')`,
		tenantID, id, jsonOf(successors), at)
	if err != nil {
		return false, fmt.Errorf("record successors: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetCertificate stores the signed certificate, once.
func (r *ErasureRepository) SetCertificate(ctx context.Context, tenantID, id, body, signature, keyID string) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE erasure_requests SET certificate_body = $3, certificate_signature = $4, certificate_key_id = $5
		WHERE tenant_id = $1 AND id = $2 AND certificate_body IS NULL`, tenantID, id, body, signature, keyID)
	return err
}

func (r *ErasureRepository) CertificateOf(ctx context.Context, tenantID, id string) (string, string, string, error) {
	var body, sig, key *string
	err := r.db.Pool.QueryRow(ctx, `SELECT certificate_body, certificate_signature, certificate_key_id FROM erasure_requests
		WHERE tenant_id = $1 AND id = $2`, tenantID, id).Scan(&body, &sig, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", model.ErrErasureNotFound
	}
	if err != nil || body == nil {
		return "", "", "", err
	}
	return *body, *sig, *key, nil
}

func (r *ErasureRepository) PutItems(ctx context.Context, tenantID, erasureID string, items []model.ErasureItem, now time.Time) error {
	batch := &pgx.Batch{}
	for _, i := range items {
		batch.Queue(`INSERT INTO erasure_items (erasure_id, tenant_id, asset_id, object_id, object_version, path, sha256, size_bytes,
			connection_id, deletion_id, state, reason, certificate_id, destroyed_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (erasure_id, asset_id) DO UPDATE SET deletion_id = EXCLUDED.deletion_id, state = EXCLUDED.state,
			    reason = EXCLUDED.reason, certificate_id = EXCLUDED.certificate_id, destroyed_at = EXCLUDED.destroyed_at,
			    updated_at = EXCLUDED.updated_at
			WHERE erasure_items.state <> 'DESTROYED'`,
			erasureID, tenantID, i.AssetID, i.ObjectID, i.ObjectVersion, i.Path, i.SHA256, i.SizeBytes, i.ConnectionID, i.DeletionID,
			i.State, i.Reason, i.CertificateID, i.DestroyedAt, now)
	}
	if err := r.db.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("record erasure items: %w", err)
	}
	return nil
}

func (r *ErasureRepository) Items(ctx context.Context, tenantID, erasureID string) ([]model.ErasureItem, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT asset_id, object_id, object_version, path, sha256, size_bytes, connection_id, deletion_id,
		state, reason, certificate_id, destroyed_at FROM erasure_items WHERE tenant_id = $1 AND erasure_id = $2 ORDER BY path, asset_id`,
		tenantID, erasureID)
	if err != nil {
		return nil, fmt.Errorf("list erasure items: %w", err)
	}
	defer rows.Close()
	out := []model.ErasureItem{}
	for rows.Next() {
		var i model.ErasureItem
		if err := rows.Scan(&i.AssetID, &i.ObjectID, &i.ObjectVersion, &i.Path, &i.SHA256, &i.SizeBytes, &i.ConnectionID, &i.DeletionID,
			&i.State, &i.Reason, &i.CertificateID, &i.DestroyedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
