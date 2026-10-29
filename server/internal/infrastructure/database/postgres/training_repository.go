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

// Training runs (migration 000027) and their lineage: a run TRAINED_ON
// each dataset version it read, a model PRODUCED_BY its run. The rows and
// edges are written in one transaction.

const runColumns = `id, name, source, external_id, url, status, params, metrics, created_by, created_at, updated_at, finished_at`

func scanRun(row pgx.Row) (model.TrainingRun, error) {
	var run model.TrainingRun
	var params, metrics []byte
	err := row.Scan(&run.ID, &run.Name, &run.Source, &run.ExternalID, &run.URL, &run.Status, &params, &metrics,
		&run.CreatedBy, &run.CreatedAt, &run.UpdatedAt, &run.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return run, model.ErrTrainingRunNotFound
	}
	if err != nil {
		return run, fmt.Errorf("scan training run: %w", err)
	}
	if err := json.Unmarshal(params, &run.Params); err != nil {
		return run, fmt.Errorf("training run params: %w", err)
	}
	if err := json.Unmarshal(metrics, &run.Metrics); err != nil {
		return run, fmt.Errorf("training run metrics: %w", err)
	}
	return run, nil
}

// CreateTrainingRun records a run, the versions it trained on and the
// model it produced, if any. A run with the same source and external ID
// is answered as it is, with created false.
func (r *DatasetRepository) CreateTrainingRun(ctx context.Context, tenantID string, run model.TrainingRun, versionIDs []string, produced *model.Model) (model.TrainingRun, bool, error) {
	created := false
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		if run.ExternalID != "" {
			existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM training_runs
				WHERE tenant_id = $1 AND source = $2 AND external_id = $3`, tenantID, run.Source, run.ExternalID))
			if err == nil {
				run = existing
				return nil
			}
			if !errors.Is(err, model.ErrTrainingRunNotFound) {
				return err
			}
		}
		params, _ := json.Marshal(run.Params)
		metrics, _ := json.Marshal(run.Metrics)
		_, err := tx.Exec(ctx, `
			INSERT INTO training_runs (`+runColumns+`, tenant_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10, $11, $12)`,
			run.ID, run.Name, run.Source, run.ExternalID, run.URL, run.Status, params, metrics, run.CreatedBy,
			run.CreatedAt, run.FinishedAt, tenantID)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: a run with this source and external ID was recorded at the same moment; send it again", model.ErrImportConflict)
		}
		if err != nil {
			return fmt.Errorf("insert training run: %w", err)
		}
		for _, versionID := range versionIDs {
			var found bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dataset_versions WHERE tenant_id = $1 AND id = $2)`,
				tenantID, versionID).Scan(&found); err != nil {
				return fmt.Errorf("check dataset version: %w", err)
			}
			if !found {
				return fmt.Errorf("%w: %s", model.ErrDatasetVersionNotFound, versionID)
			}
			err := link(ctx, tx, tenantID, model.LineageEdge{
				From: model.LineageRef{Kind: model.NodeTrainingRun, RefID: run.ID}, To: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: versionID},
				Relation: model.RelTrainedOn, Actor: run.CreatedBy, Details: map[string]any{"name": run.Name, "source": string(run.Source)},
			}, run.CreatedAt, true)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO training_run_inputs (run_id, version_id, tenant_id) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, run.ID, versionID, tenantID); err != nil {
				return fmt.Errorf("record training input: %w", err)
			}
		}
		if produced != nil {
			if err := addModel(ctx, tx, tenantID, run, *produced); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return model.TrainingRun{}, false, err
	}
	out, err := r.GetTrainingRun(ctx, tenantID, run.ID)
	return out, created, err
}

func addModel(ctx context.Context, tx pgx.Tx, tenantID string, run model.TrainingRun, m model.Model) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO models (id, tenant_id, run_id, name, version, uri, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		m.ID, tenantID, run.ID, m.Name, m.Version, m.URI, m.CreatedBy, m.CreatedAt)
	if isUniqueViolation(err) {
		return model.ErrModelExists
	}
	if err != nil {
		return fmt.Errorf("insert model: %w", err)
	}
	return link(ctx, tx, tenantID, model.LineageEdge{
		From: model.LineageRef{Kind: model.NodeModel, RefID: m.ID}, To: model.LineageRef{Kind: model.NodeTrainingRun, RefID: run.ID},
		Relation: model.RelProducedBy, Actor: m.CreatedBy, Details: map[string]any{"name": m.Name, "version": m.Version},
	}, m.CreatedAt, true)
}

// UpdateTrainingRun sets a run's status and merges in metrics; a model,
// when given, is recorded as produced by it. A finished or failed run
// keeps its status.
func (r *DatasetRepository) UpdateTrainingRun(ctx context.Context, tenantID, runID string, status model.TrainingStatus, metrics map[string]float64, produced *model.Model, at time.Time) (model.TrainingRun, error) {
	err := withTx(ctx, r.db, func(tx pgx.Tx) error {
		run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM training_runs WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, runID))
		if err != nil {
			return err
		}
		if run.Status == model.RunRunning && status != "" && status != model.RunRunning {
			run.Status = status
			run.FinishedAt = &at
		}
		for k, v := range metrics {
			if run.Metrics == nil {
				run.Metrics = map[string]float64{}
			}
			run.Metrics[k] = v
		}
		encoded, _ := json.Marshal(run.Metrics)
		if _, err := tx.Exec(ctx, `UPDATE training_runs SET status = $3, metrics = $4, finished_at = $5, updated_at = $6
			WHERE tenant_id = $1 AND id = $2`, tenantID, runID, run.Status, encoded, run.FinishedAt, at); err != nil {
			return fmt.Errorf("update training run: %w", err)
		}
		if produced != nil {
			return addModel(ctx, tx, tenantID, run, *produced)
		}
		return nil
	})
	if err != nil {
		return model.TrainingRun{}, err
	}
	return r.GetTrainingRun(ctx, tenantID, runID)
}

// GetTrainingRun reads a run with its input versions and models.
func (r *DatasetRepository) GetTrainingRun(ctx context.Context, tenantID, runID string) (model.TrainingRun, error) {
	run, err := scanRun(r.db.Pool.QueryRow(ctx, `SELECT `+runColumns+` FROM training_runs WHERE tenant_id = $1 AND id = $2`, tenantID, runID))
	if err != nil {
		return run, err
	}
	run.TenantID = tenantID
	runs := []model.TrainingRun{run}
	if err := r.attachRunDetails(ctx, tenantID, runs); err != nil {
		return run, err
	}
	return runs[0], nil
}

// ListTrainingRuns lists runs newest first, all of them or those that
// trained on one dataset version.
func (r *DatasetRepository) ListTrainingRuns(ctx context.Context, tenantID, versionID, cursor string, limit int) (model.TrainingRunPage, error) {
	at, id, err := decodeTimeCursor(cursor)
	if err != nil {
		return model.TrainingRunPage{}, err
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+runColumns+` FROM training_runs AS t
		WHERE t.tenant_id = $1
		  AND ($2 = '' OR EXISTS (SELECT 1 FROM training_run_inputs AS i WHERE i.tenant_id = $1 AND i.run_id = t.id AND i.version_id = $2))
		  AND ($3 = '' OR (t.created_at, t.id) < ($4, $3))
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT $5`, tenantID, versionID, id, at, limit+1)
	if err != nil {
		return model.TrainingRunPage{}, fmt.Errorf("list training runs: %w", err)
	}
	runs, err := collect(rows, func(row pgx.Row) (model.TrainingRun, error) { return scanRun(row) })
	if err != nil {
		return model.TrainingRunPage{}, err
	}
	page := model.TrainingRunPage{}
	if len(runs) > limit {
		runs = runs[:limit]
		last := runs[len(runs)-1]
		page.NextCursor = encodeTimeCursor(last.CreatedAt, last.ID)
	}
	if err := r.attachRunDetails(ctx, tenantID, runs); err != nil {
		return model.TrainingRunPage{}, err
	}
	page.Runs = runs
	return page, nil
}

func (r *DatasetRepository) attachRunDetails(ctx context.Context, tenantID string, runs []model.TrainingRun) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, len(runs))
	byID := make(map[string]*model.TrainingRun, len(runs))
	for i := range runs {
		ids[i] = runs[i].ID
		runs[i].TenantID = tenantID
		runs[i].VersionIDs = []string{}
		runs[i].Models = []model.Model{}
		byID[runs[i].ID] = &runs[i]
	}
	rows, err := r.db.Pool.Query(ctx, `SELECT run_id, version_id FROM training_run_inputs
		WHERE tenant_id = $1 AND run_id = ANY($2) ORDER BY run_id, version_id`, tenantID, ids)
	if err != nil {
		return fmt.Errorf("list training inputs: %w", err)
	}
	for rows.Next() {
		var runID, versionID string
		if err := rows.Scan(&runID, &versionID); err != nil {
			rows.Close()
			return fmt.Errorf("scan training input: %w", err)
		}
		byID[runID].VersionIDs = append(byID[runID].VersionIDs, versionID)
	}
	rows.Close()
	rows, err = r.db.Pool.Query(ctx, `SELECT id, run_id, name, version, uri, created_by, created_at FROM models
		WHERE tenant_id = $1 AND run_id = ANY($2) ORDER BY created_at, id`, tenantID, ids)
	if err != nil {
		return fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m model.Model
		if err := rows.Scan(&m.ID, &m.RunID, &m.Name, &m.Version, &m.URI, &m.CreatedBy, &m.CreatedAt); err != nil {
			return fmt.Errorf("scan model: %w", err)
		}
		byID[m.RunID].Models = append(byID[m.RunID].Models, m)
	}
	return rows.Err()
}
