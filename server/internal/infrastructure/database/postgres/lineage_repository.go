package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Lineage is a graph in two tables. A node stands for a record that lives
// elsewhere (a dataset, a version, an import, a report, an object version,
// a transformation); its ID is a hash of the tenant, kind and record ID,
// so writing a node twice is writing it once, and the same record in two
// tenants is two nodes. Edges are unique per (from, to, relation).
//
// A version's object versions are not edges: they are the version's
// member rows (dataset_version_members), which already index them.

// maxCycleDepth bounds the reachability check before an acyclic edge is
// written. Provenance chains are short; a longer one is refused rather
// than checked partially.
const maxCycleDepth = 64

// maxDetailsBytes is the edge and node details' size limit (the CHECK).
const maxDetailsBytes = 4096

func nodeID(tenantID string, ref model.LineageRef) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + string(ref.Kind) + "\x00" + ref.RefID))
	return hex.EncodeToString(sum[:16])
}

// link writes an edge and its nodes, in the caller's transaction. Edges of
// acyclic relations are refused when the graph already leads from their
// target back to their source. trusted skips checking that the records
// exist: the caller wrote or read them in this transaction.
func link(ctx context.Context, tx pgx.Tx, tenantID string, e model.LineageEdge, now time.Time, trusted bool) error {
	if !model.ValidLineage(e.Relation, e.From.Kind, e.To.Kind) {
		return fmt.Errorf("%w: %s %s %s", model.ErrInvalidLineage, e.From.Kind, e.Relation, e.To.Kind)
	}
	if e.From.RefID == "" || e.To.RefID == "" {
		return fmt.Errorf("%w: both ends need a record ID", model.ErrInvalidLineage)
	}
	from, to := nodeID(tenantID, e.From), nodeID(tenantID, e.To)
	if from == to {
		return model.ErrLineageCycle
	}
	if !trusted {
		for _, ref := range []model.LineageRef{e.From, e.To} {
			ok, err := refExists(ctx, tx, tenantID, ref)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: no %s %q", model.ErrInvalidLineage, ref.Kind, ref.RefID)
			}
		}
	}
	details, err := json.Marshal(e.Details)
	if err != nil {
		return fmt.Errorf("encode lineage details: %w", err)
	}
	if e.Details == nil {
		details = []byte(`{}`)
	}
	if len(details) > maxDetailsBytes {
		return fmt.Errorf("%w: details are larger than %d bytes", model.ErrInvalidLineage, maxDetailsBytes)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO lineage_nodes (id, tenant_id, kind, ref_id, created_at)
		VALUES ($1, $3, $4, $5, $8), ($2, $3, $6, $7, $8)
		ON CONFLICT (id) DO NOTHING
	`, from, to, tenantID, string(e.From.Kind), e.From.RefID, string(e.To.Kind), e.To.RefID, now); err != nil {
		return fmt.Errorf("write lineage nodes: %w", err)
	}

	if e.Relation.Acyclic() {
		// One writer of acyclic edges per tenant at a time, so two edges
		// that would close a cycle together cannot both pass the check.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('lineage:' || $1))`, tenantID); err != nil {
			return fmt.Errorf("lock lineage: %w", err)
		}
		var cycle, tooDeep bool
		err := tx.QueryRow(ctx, `
			WITH RECURSIVE reach(node, depth) AS (
			    SELECT $1::text, 0
			  UNION
			    SELECT e.to_node, r.depth + 1
			    FROM reach AS r JOIN lineage_edges AS e ON e.from_node = r.node
			    WHERE e.relation = ANY($3) AND r.depth < $4
			)
			SELECT EXISTS (SELECT 1 FROM reach WHERE node = $2),
			       EXISTS (SELECT 1 FROM reach WHERE depth = $4)
		`, to, from, acyclicRelations(), maxCycleDepth).Scan(&cycle, &tooDeep)
		if err != nil {
			return fmt.Errorf("check lineage cycle: %w", err)
		}
		if cycle {
			return fmt.Errorf("%w: %s %s %s", model.ErrLineageCycle, e.From.Kind, e.Relation, e.To.Kind)
		}
		if tooDeep {
			return fmt.Errorf("%w: the provenance chain is longer than %d edges", model.ErrInvalidLineage, maxCycleDepth)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO lineage_edges (from_node, to_node, relation, tenant_id, actor, job_id, details, created_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8)
		ON CONFLICT (from_node, to_node, relation) DO NOTHING
	`, from, to, string(e.Relation), tenantID, e.Actor, e.JobID, details, now); err != nil {
		return fmt.Errorf("write lineage edge: %w", err)
	}
	return nil
}

func acyclicRelations() []string {
	var out []string
	for _, r := range []model.LineageRelation{model.RelContains, model.RelDerivedFrom, model.RelImportedFrom,
		model.RelValidatedBy, model.RelCreatedFromSnapshot, model.RelTransformedBy, model.RelTrainedOn, model.RelProducedBy} {
		if r.Acyclic() {
			out = append(out, string(r))
		}
	}
	return out
}

// refExists tells whether the tenant has the record a node would stand
// for. A transformation is only a name: it exists once named.
func refExists(ctx context.Context, tx pgx.Tx, tenantID string, ref model.LineageRef) (bool, error) {
	var table string
	switch ref.Kind {
	case model.NodeDataset:
		table = "datasets"
	case model.NodeDatasetVersion:
		table = "dataset_versions"
	case model.NodeManifestImport:
		table = "dataset_manifest_imports"
	case model.NodeValidationReport:
		table = "validation_reports"
	case model.NodeObjectVersion:
		table = "assets"
	case model.NodeTrainingRun:
		table = "training_runs"
	case model.NodeModel:
		table = "models"
	case model.NodeTransformation:
		return true, nil
	default:
		return false, nil
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE tenant_id = $1 AND id = $2)`, tenantID, ref.RefID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check %s: %w", ref.Kind, err)
	}
	return ok, nil
}

// AddLineage records one edge between records of the tenant, checking
// that both exist.
func (r *DatasetRepository) AddLineage(ctx context.Context, tenantID string, e model.LineageEdge, now time.Time) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin lineage: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := link(ctx, tx, tenantID, e, now, false); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit lineage: %w", err)
	}
	return nil
}

// lineageCursor is a position in a traversal's order.
type lineageCursor struct {
	Depth    int    `json:"d"`
	From     string `json:"f"`
	To       string `json:"t"`
	Relation string `json:"r"`
}

// Lineage walks the tenant's graph from start, upstream (along edges) or
// downstream (against them), to MaxDepth edges away. Each edge is listed
// once, at its shortest distance, ordered by distance; at most MaxEdges
// edges are read, and the page says when the walk stopped there.
func (r *DatasetRepository) Lineage(ctx context.Context, tenantID string, start model.LineageRef, q interfaces.LineageQuery) (model.LineagePage, error) {
	after := lineageCursor{}
	if q.After != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.After)
		if err != nil || json.Unmarshal(raw, &after) != nil {
			return model.LineagePage{}, model.ErrInvalidCursor
		}
	}
	near, far := "from_node", "to_node"
	if !q.Upstream {
		near, far = "to_node", "from_node"
	}
	rows, err := r.db.Pool.Query(ctx, `
		WITH RECURSIVE walk(from_node, to_node, relation, depth) AS (
		    SELECT e.from_node, e.to_node, e.relation, 1
		    FROM lineage_edges AS e WHERE e.tenant_id = $1 AND e.`+near+` = $2
		  UNION
		    SELECT e.from_node, e.to_node, e.relation, w.depth + 1
		    FROM walk AS w JOIN lineage_edges AS e ON e.tenant_id = $1 AND e.`+near+` = w.`+far+`
		    WHERE w.depth < $3
		),
		bounded AS (SELECT * FROM walk LIMIT $4),
		shortest AS (
		    SELECT DISTINCT ON (from_node, to_node, relation) from_node, to_node, relation, depth
		    FROM bounded ORDER BY from_node, to_node, relation, depth
		)
		SELECT s.depth, s.from_node, s.to_node, s.relation, f.kind, f.ref_id, t.kind, t.ref_id,
		       e.actor, COALESCE(e.job_id, ''), e.details, e.created_at, (SELECT count(*) FROM bounded)
		FROM shortest AS s
		JOIN lineage_edges AS e ON e.from_node = s.from_node AND e.to_node = s.to_node AND e.relation = s.relation
		JOIN lineage_nodes AS f ON f.id = s.from_node
		JOIN lineage_nodes AS t ON t.id = s.to_node
		WHERE (s.depth, s.from_node, s.to_node, s.relation) > ($5, $6, $7, $8)
		ORDER BY s.depth, s.from_node, s.to_node, s.relation
		LIMIT $9
	`, tenantID, nodeID(tenantID, start), q.MaxDepth, q.MaxEdges, after.Depth, after.From, after.To, after.Relation, q.Limit+1)
	if err != nil {
		return model.LineagePage{}, fmt.Errorf("walk lineage: %w", err)
	}
	defer rows.Close()

	var page model.LineagePage
	var positions []lineageCursor
	for rows.Next() {
		var e model.LineageEdge
		var c lineageCursor
		var fromKind, toKind, relation string
		var details []byte
		var read int64
		if err := rows.Scan(&c.Depth, &c.From, &c.To, &relation, &fromKind, &e.From.RefID, &toKind, &e.To.RefID,
			&e.Actor, &e.JobID, &details, &e.CreatedAt, &read); err != nil {
			return model.LineagePage{}, err
		}
		c.Relation = relation
		e.Depth = c.Depth
		e.From.Kind, e.To.Kind, e.Relation = model.LineageKind(fromKind), model.LineageKind(toKind), model.LineageRelation(relation)
		if len(details) > 0 {
			if err := json.Unmarshal(details, &e.Details); err != nil {
				return model.LineagePage{}, fmt.Errorf("decode lineage details: %w", err)
			}
		}
		page.Truncated = read >= int64(q.MaxEdges)
		page.Edges = append(page.Edges, e)
		positions = append(positions, c)
	}
	if err := rows.Err(); err != nil {
		return model.LineagePage{}, fmt.Errorf("walk lineage: %w", err)
	}
	if len(page.Edges) > q.Limit {
		page.Edges = page.Edges[:q.Limit]
		raw, _ := json.Marshal(positions[q.Limit-1])
		page.After = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
