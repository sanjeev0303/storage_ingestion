package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Dataset branches and merges (migration 000025).

// CreateBranch creates a branch of parent named name, its draft holding
// exactly the members of version fork: rows only, no bytes are copied.
func (r *DatasetRepository) CreateBranch(ctx context.Context, parent model.Dataset, name, forkVersionID, branchID, actor string, now time.Time) (model.Dataset, error) {
	var branch model.Dataset
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		branch, err = scanDataset(tx.QueryRow(ctx, `
			INSERT INTO datasets (id, tenant_id, name, completion_policy, status, created_by, created_at, updated_at,
			                      description, tags, metadata, profile_id, profile_version, storage_connection_id,
			                      parent_dataset_id, branch_name, fork_version_id, merge_base_version_id, updated_by)
			SELECT $3, tenant_id, name || '@' || $4, completion_policy, 'OPEN', $5, $6, $6,
			       description, tags, metadata, profile_id, profile_version, storage_connection_id,
			       id, $4, $7, $7, $5
			FROM datasets WHERE tenant_id = $1 AND id = $2
			RETURNING `+datasetColumns, parent.TenantID, parent.ID, branchID, name, actor, now, forkVersionID))
		if isConstraint(err, "ux_datasets_branch") || isConstraint(err, "ux_datasets_tenant_name") {
			return model.ErrBranchExists
		}
		if err != nil {
			return fmt.Errorf("create branch: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO dataset_members (id, tenant_id, dataset_id, object_id, asset_id, label, split, metadata,
			                             source, added_by, created_at, updated_at)
			SELECT md5(random()::text || object_id), tenant_id, $2, object_id, asset_id, label, split, member_metadata,
			       'branch:' || version_id, $3, $4, $4
			FROM dataset_version_members WHERE version_id = $1`, forkVersionID, branchID, actor, now)
		if err != nil {
			return fmt.Errorf("copy branch members: %w", err)
		}
		// Lineage: the branch was created from that snapshot of its parent.
		err = link(ctx, tx, parent.TenantID, model.LineageEdge{
			From: model.LineageRef{Kind: model.NodeDataset, RefID: branchID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: forkVersionID},
			Relation: model.RelCreatedFromSnapshot, Actor: actor, Details: map[string]any{"branch": name},
		}, now, true)
		if err != nil {
			return err
		}
		branch, err = scanDataset(tx.QueryRow(ctx, `
			UPDATE datasets SET member_count = $2, members_revision = 1 WHERE id = $1 RETURNING `+datasetColumns,
			branchID, tag.RowsAffected()))
		return err
	})
	return branch, err
}

// ListBranches lists a dataset's branches by name.
func (r *DatasetRepository) ListBranches(ctx context.Context, tenantID, parentID string) ([]model.Dataset, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+datasetColumns+` FROM datasets WHERE tenant_id = $1 AND parent_dataset_id = $2 ORDER BY branch_name`,
		tenantID, parentID)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	return collect(rows, scanDataset)
}

// mergeRow is one object the source changed since the base, with all three
// sides.
type mergeRow struct {
	objectID, namespace, path string
	base, source, target      model.MergeSide
}

func sameSide(a, b model.MergeSide) bool {
	if a.Present != b.Present {
		return false
	}
	if !a.Present {
		return true
	}
	return a.AssetID == b.AssetID && a.Label == b.Label && a.Split == b.Split && maps.Equal(a.Metadata, b.Metadata)
}

// The objects whose membership differs between the base and the source
// version, with the target draft's membership beside them. A draft member
// that follows its object's current version is compared at that version.
const mergeRowsSQL = `
	WITH b AS (SELECT object_id, asset_id, label, split, member_metadata AS metadata
	           FROM dataset_version_members WHERE version_id = $1),
	     s AS (SELECT object_id, asset_id, label, split, member_metadata AS metadata
	           FROM dataset_version_members WHERE version_id = $2),
	     changed AS (
	         SELECT COALESCE(s.object_id, b.object_id) AS object_id,
	                b.object_id IS NOT NULL AS b_in, b.asset_id AS b_asset, b.label AS b_label, b.split AS b_split, b.metadata AS b_meta,
	                s.object_id IS NOT NULL AS s_in, s.asset_id AS s_asset, s.label AS s_label, s.split AS s_split, s.metadata AS s_meta
	         FROM b FULL OUTER JOIN s ON s.object_id = b.object_id
	         WHERE (b.object_id IS NULL) <> (s.object_id IS NULL)
	            OR (b.asset_id, b.label, b.split, b.metadata) IS DISTINCT FROM (s.asset_id, s.label, s.split, s.metadata))
	SELECT c.object_id, COALESCE(o.dataset, ''), COALESCE(o.path, ''),
	       c.b_in, COALESCE(c.b_asset, ''), COALESCE(c.b_label, ''), COALESCE(c.b_split, ''), c.b_meta,
	       c.s_in, COALESCE(c.s_asset, ''), COALESCE(c.s_label, ''), COALESCE(c.s_split, ''), c.s_meta,
	       m.object_id IS NOT NULL, COALESCE(m.asset_id, o.current_asset_id, ''), COALESCE(m.label, ''), COALESCE(m.split, ''), m.metadata
	FROM changed c
	LEFT JOIN dataset_members m ON m.dataset_id = $3 AND m.object_id = c.object_id
	LEFT JOIN catalog_objects o ON o.id = c.object_id
	ORDER BY c.object_id`

func decodeSideMetadata(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode member metadata: %w", err)
	}
	return out, nil
}

// Merge brings the changes the source version made since the base into the
// target's draft, three-way:
//   - an object the target left as it was at the base takes the source's
//     membership (added, updated or removed);
//   - an object the target already has the source's way is left alone;
//   - an object both changed, differently, is a conflict, resolved by the
//     strategy: FAIL changes nothing at all, SOURCE takes the source's side,
//     TARGET keeps the target's.
//
// On success the branch's merge base moves to the source version and the
// merge is recorded. A repeated idempotency key returns the recorded merge.
func (r *DatasetRepository) Merge(ctx context.Context, in model.MergeInput) (model.MergeResult, error) {
	res := model.MergeResult{
		TargetID: in.TargetID, SourceID: in.SourceID, SourceVersionID: in.SourceVersionID,
		BaseVersionID: in.BaseVersionID, Strategy: in.Strategy, CreatedBy: in.Actor, CreatedAt: in.Now,
	}
	if in.IdempotencyKey != "" {
		done, found, err := r.mergeByKey(ctx, in.TenantID, in.TargetID, in.IdempotencyKey)
		if err != nil || found {
			return done, err
		}
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin merge: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := lockDraft(ctx, tx, in.TenantID, in.TargetID); err != nil {
		return res, err
	}

	rows, err := tx.Query(ctx, mergeRowsSQL, in.BaseVersionID, in.SourceVersionID, in.TargetID)
	if err != nil {
		return res, fmt.Errorf("compare merge sides: %w", err)
	}
	var changes []mergeRow
	for rows.Next() {
		var c mergeRow
		var bMeta, sMeta, tMeta []byte
		err := rows.Scan(&c.objectID, &c.namespace, &c.path,
			&c.base.Present, &c.base.AssetID, &c.base.Label, &c.base.Split, &bMeta,
			&c.source.Present, &c.source.AssetID, &c.source.Label, &c.source.Split, &sMeta,
			&c.target.Present, &c.target.AssetID, &c.target.Label, &c.target.Split, &tMeta)
		if err != nil {
			rows.Close()
			return res, err
		}
		for _, side := range []struct {
			raw []byte
			to  *map[string]string
		}{{bMeta, &c.base.Metadata}, {sMeta, &c.source.Metadata}, {tMeta, &c.target.Metadata}} {
			if *side.to, err = decodeSideMetadata(side.raw); err != nil {
				rows.Close()
				return res, err
			}
		}
		changes = append(changes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("compare merge sides: %w", err)
	}

	var upserts []mergeRow
	var removals []string
	for _, c := range changes {
		take := false
		switch {
		case sameSide(c.target, c.base):
			take = true
		case sameSide(c.target, c.source):
			res.AlreadySame++
		default:
			res.Conflicts++
			if len(res.ConflictList) < in.MaxConflicts {
				res.ConflictList = append(res.ConflictList, model.MergeConflict{ObjectID: c.objectID, Namespace: c.namespace,
					Path: c.path, Base: c.base, Source: c.source, Target: c.target})
			}
			take = in.Strategy == model.MergeSource
		}
		if !take {
			continue
		}
		switch {
		case !c.source.Present:
			removals = append(removals, c.objectID)
			res.Removed++
		case !c.target.Present:
			upserts = append(upserts, c)
			res.Added++
		default:
			upserts = append(upserts, c)
			res.Updated++
		}
	}
	if in.DryRun || (res.Conflicts > 0 && in.Strategy == model.MergeFail) {
		return res, nil
	}

	if len(upserts) > 0 {
		n := len(upserts)
		ids, objects, assets, labels, splits, metadata := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
		for i, c := range upserts {
			raw, err := json.Marshal(nonNilMetadata(c.source.Metadata))
			if err != nil {
				return res, err
			}
			ids[i], objects[i], assets[i], labels[i], splits[i], metadata[i] = newRowID(), c.objectID, c.source.AssetID, c.source.Label, c.source.Split, string(raw)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO dataset_members (id, tenant_id, dataset_id, object_id, asset_id, label, split, metadata,
			                             source, added_by, created_at, updated_at)
			SELECT c.id, $1, $2, c.object_id, NULLIF(c.asset_id, ''), NULLIF(c.label, ''), NULLIF(c.split, ''),
			       c.metadata::jsonb, $3, $4, $5, $5
			FROM unnest($6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[])
			     AS c(id, object_id, asset_id, label, split, metadata)
			ON CONFLICT (dataset_id, object_id) DO UPDATE SET asset_id = EXCLUDED.asset_id, label = EXCLUDED.label,
			    split = EXCLUDED.split, metadata = EXCLUDED.metadata, source = EXCLUDED.source,
			    added_by = EXCLUDED.added_by, updated_at = EXCLUDED.updated_at`,
			in.TenantID, in.TargetID, "merge:"+in.SourceVersionID, in.Actor, in.Now, ids, objects, assets, labels, splits, metadata)
		if err != nil {
			return res, fmt.Errorf("merge members: %w", err)
		}
	}
	if len(removals) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM dataset_members WHERE tenant_id = $1 AND dataset_id = $2 AND object_id = ANY($3)`,
			in.TenantID, in.TargetID, removals); err != nil {
			return res, fmt.Errorf("merge removals: %w", err)
		}
	}
	if len(upserts)+len(removals) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE datasets SET member_count = (SELECT count(*) FROM dataset_members WHERE dataset_id = $2),
			                    members_revision = members_revision + 1, revision = revision + 1,
			                    lifecycle_status = `+leaveReady+`, updated_by = $3, updated_at = $4
			WHERE tenant_id = $1 AND id = $2`, in.TenantID, in.TargetID, in.Actor, in.Now); err != nil {
			return res, fmt.Errorf("count merged members: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE datasets SET merge_base_version_id = $3 WHERE tenant_id = $1 AND id = $2`,
		in.TenantID, in.BranchID, in.SourceVersionID); err != nil {
		return res, fmt.Errorf("move merge base: %w", err)
	}
	res.ID, res.Applied = newRowID(), true
	_, err = tx.Exec(ctx, `
		INSERT INTO dataset_merges (id, tenant_id, target_dataset_id, source_dataset_id, source_version_id, base_version_id,
		                            strategy, added, updated, removed, conflicts, idempotency_key, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), $13, $14)`,
		res.ID, in.TenantID, in.TargetID, in.SourceID, in.SourceVersionID, in.BaseVersionID, string(in.Strategy),
		res.Added, res.Updated, res.Removed, res.Conflicts, in.IdempotencyKey, in.Actor, in.Now)
	if isConstraint(err, "ux_dataset_merges_key") {
		// The same request finished first elsewhere: answer with its record.
		_ = tx.Rollback(ctx)
		done, _, err := r.mergeByKey(ctx, in.TenantID, in.TargetID, in.IdempotencyKey)
		return done, err
	}
	if err != nil {
		return res, fmt.Errorf("record merge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit merge: %w", err)
	}
	return res, nil
}

const mergeColumns = `id, target_dataset_id, source_dataset_id, source_version_id, base_version_id, strategy,
	added, updated, removed, conflicts, created_by, created_at`

func scanMerge(row pgx.Row) (model.MergeResult, error) {
	var m model.MergeResult
	var strategy string
	err := row.Scan(&m.ID, &m.TargetID, &m.SourceID, &m.SourceVersionID, &m.BaseVersionID, &strategy,
		&m.Added, &m.Updated, &m.Removed, &m.Conflicts, &m.CreatedBy, &m.CreatedAt)
	m.Strategy, m.Applied = model.MergeStrategy(strategy), true
	return m, err
}

func (r *DatasetRepository) mergeByKey(ctx context.Context, tenantID, targetID, key string) (model.MergeResult, bool, error) {
	m, err := scanMerge(r.db.Pool.QueryRow(ctx, `
		SELECT `+mergeColumns+` FROM dataset_merges WHERE tenant_id = $1 AND target_dataset_id = $2 AND idempotency_key = $3`,
		tenantID, targetID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

// ListMerges lists the merges into a dataset, newest first.
func (r *DatasetRepository) ListMerges(ctx context.Context, tenantID, targetID string, limit int) ([]model.MergeResult, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+mergeColumns+` FROM dataset_merges WHERE tenant_id = $1 AND target_dataset_id = $2
		ORDER BY created_at DESC, id DESC LIMIT $3`, tenantID, targetID, limit)
	if err != nil {
		return nil, fmt.Errorf("list merges: %w", err)
	}
	return collect(rows, scanMerge)
}
