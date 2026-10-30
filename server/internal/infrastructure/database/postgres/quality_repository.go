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

// QualityRepository keeps column profiles, drift, leakage checks, quality
// settings and release labels (docs/quality.md).
type QualityRepository struct {
	db *DB
}

func NewQualityRepository(db *DB) *QualityRepository {
	return &QualityRepository{db: db}
}

// ─── Settings ─────────────────────────────────────────────────────────

func (r *QualityRepository) Settings(ctx context.Context, tenantID, datasetID string) (model.QualitySettings, error) {
	s := model.DefaultQualitySettings(datasetID)
	s.TenantID = tenantID
	err := r.db.Pool.QueryRow(ctx, `SELECT profile_snapshots, max_psi, max_null_change, max_row_change, subject_key, subject_pattern,
		updated_by, updated_at FROM dataset_quality WHERE tenant_id = $1 AND dataset_id = $2`, tenantID, datasetID).
		Scan(&s.ProfileSnapshots, &s.MaxPSI, &s.MaxNullChange, &s.MaxRowChange, &s.SubjectKey, &s.SubjectPattern, &s.UpdatedBy, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read quality settings: %w", err)
	}
	return s, nil
}

func (r *QualityRepository) PutSettings(ctx context.Context, s model.QualitySettings) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO dataset_quality (dataset_id, tenant_id, profile_snapshots, max_psi, max_null_change,
		max_row_change, subject_key, subject_pattern, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (dataset_id) DO UPDATE SET profile_snapshots = EXCLUDED.profile_snapshots, max_psi = EXCLUDED.max_psi,
		    max_null_change = EXCLUDED.max_null_change, max_row_change = EXCLUDED.max_row_change, subject_key = EXCLUDED.subject_key,
		    subject_pattern = EXCLUDED.subject_pattern, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at
		WHERE dataset_quality.tenant_id = EXCLUDED.tenant_id`,
		s.DatasetID, s.TenantID, s.ProfileSnapshots, s.MaxPSI, s.MaxNullChange, s.MaxRowChange, s.SubjectKey, s.SubjectPattern, s.UpdatedBy, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save quality settings: %w", err)
	}
	return nil
}

// ─── Profiles ─────────────────────────────────────────────────────────

const versionProfileColumns = `version_id, tenant_id, dataset_id, state, tables, skipped, files_profiled, bytes_read, other_files,
	COALESCE(baseline_version_id, ''), drift, drifted, error, attempts, created_at, finished_at`

func scanVersionProfile(row pgx.Row) (model.VersionProfile, error) {
	var p model.VersionProfile
	var tables, skipped, drift []byte
	err := row.Scan(&p.VersionID, &p.TenantID, &p.DatasetID, &p.State, &tables, &skipped, &p.FilesProfiled, &p.BytesRead, &p.OtherFiles,
		&p.BaselineVersionID, &drift, &p.Drifted, &p.Error, &p.Attempts, &p.CreatedAt, &p.FinishedAt)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(tables, &p.Tables); err != nil {
		return p, fmt.Errorf("decode profile tables: %w", err)
	}
	if err := json.Unmarshal(skipped, &p.Skipped); err != nil {
		return p, fmt.Errorf("decode skipped files: %w", err)
	}
	if len(drift) > 0 {
		p.Drift = &model.DriftReport{}
		if err := json.Unmarshal(drift, p.Drift); err != nil {
			return p, fmt.Errorf("decode drift: %w", err)
		}
	}
	return p, nil
}

// Profile returns a version's profile; found is false when the version
// was never queued (its dataset does not profile snapshots).
func (r *QualityRepository) Profile(ctx context.Context, tenantID, versionID string) (model.VersionProfile, bool, error) {
	p, err := scanVersionProfile(r.db.Pool.QueryRow(ctx, `SELECT `+versionProfileColumns+` FROM version_profiles
		WHERE tenant_id = $1 AND version_id = $2`, tenantID, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("read profile: %w", err)
	}
	return p, true, nil
}

// QueueProfile asks for a version to be profiled (again).
func (r *QualityRepository) QueueProfile(ctx context.Context, tenantID, datasetID, versionID string) error {
	_, err := r.db.Pool.Exec(ctx, `INSERT INTO version_profiles (version_id, tenant_id, dataset_id, state) VALUES ($1, $2, $3, 'PENDING')
		ON CONFLICT (version_id) DO UPDATE SET state = 'PENDING', attempts = 0, error = '', lease_owner = NULL, lease_until = NULL
		WHERE version_profiles.tenant_id = EXCLUDED.tenant_id AND version_profiles.state <> 'RUNNING'`, versionID, tenantID, datasetID)
	if err != nil {
		return fmt.Errorf("queue profile: %w", err)
	}
	return nil
}

// ClaimProfile leases the oldest pending profile (or one whose worker
// stopped). A profile tried maxAttempts times fails instead.
func (r *QualityRepository) ClaimProfile(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (model.VersionProfile, bool, error) {
	if _, err := r.db.Pool.Exec(ctx, `UPDATE version_profiles SET state = 'FAILED', finished_at = now(), lease_owner = NULL,
		error = 'gave up after ' || attempts || ' attempts'
		WHERE state = 'RUNNING' AND lease_until < now() AND attempts >= $1`, maxAttempts); err != nil {
		return model.VersionProfile{}, false, fmt.Errorf("fail stuck profiles: %w", err)
	}
	p, err := scanVersionProfile(r.db.Pool.QueryRow(ctx, `UPDATE version_profiles SET state = 'RUNNING', lease_owner = $1,
		lease_until = now() + $2::interval, attempts = attempts + 1
		WHERE version_id = (SELECT version_id FROM version_profiles
		    WHERE state = 'PENDING' OR (state = 'RUNNING' AND lease_until < now())
		    ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+versionProfileColumns, owner, fmt.Sprintf("%d seconds", int(lease.Seconds()))))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("claim profile: %w", err)
	}
	return p, true, nil
}

// ExtendProfile keeps a long profile's lease.
func (r *QualityRepository) ExtendProfile(ctx context.Context, versionID, owner string, lease time.Duration) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE version_profiles SET lease_until = now() + $3::interval
		WHERE version_id = $1 AND lease_owner = $2 AND state = 'RUNNING'`, versionID, owner, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	return err
}

// FinishProfile records the outcome, if owner still holds the lease.
func (r *QualityRepository) FinishProfile(ctx context.Context, owner string, p model.VersionProfile) error {
	tables, err := json.Marshal(p.Tables)
	if err != nil {
		return err
	}
	skipped, err := json.Marshal(p.Skipped)
	if err != nil {
		return err
	}
	var drift []byte
	if p.Drift != nil {
		if drift, err = json.Marshal(p.Drift); err != nil {
			return err
		}
	}
	_, err = r.db.Pool.Exec(ctx, `UPDATE version_profiles SET state = $3, tables = $4, skipped = $5, files_profiled = $6,
		bytes_read = $7, other_files = $8, baseline_version_id = NULLIF($9, ''), drift = $10, drifted = $11, error = $12,
		finished_at = now(), lease_owner = NULL, lease_until = NULL
		WHERE version_id = $1 AND lease_owner = $2`,
		p.VersionID, owner, p.State, tables, skipped, p.FilesProfiled, p.BytesRead, p.OtherFiles, p.BaselineVersionID, drift, p.Drifted, p.Error)
	if err != nil {
		return fmt.Errorf("finish profile: %w", err)
	}
	return nil
}

// Baseline is the version a version is compared with: its parent, or the
// one before it in its dataset.
func (r *QualityRepository) Baseline(ctx context.Context, tenantID, versionID string) (string, error) {
	var id string
	err := r.db.Pool.QueryRow(ctx, `SELECT COALESCE(v.parent_version_id, (SELECT p.id FROM dataset_versions p
		    WHERE p.tenant_id = v.tenant_id AND p.dataset_id = v.dataset_id AND p.version < v.version ORDER BY p.version DESC LIMIT 1), '')
		FROM dataset_versions v WHERE v.tenant_id = $1 AND v.id = $2`, tenantID, versionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", model.ErrDatasetVersionNotFound
	}
	return id, err
}

// ─── Members, for the metadata and subject rules and leakage ───────────

// MetadataValues counts a metadata key's values across a version's
// members (member metadata over object metadata), most common first, with
// the number of members that lack it.
func (r *QualityRepository) MetadataValues(ctx context.Context, tenantID, versionID, key string, top int) ([]model.Count, int64, int64, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT COALESCE(member_metadata->>$3, metadata->>$3) AS value, count(*)
		FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2
		GROUP BY 1 ORDER BY count(*) DESC, 1`, tenantID, versionID, key)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("count metadata values: %w", err)
	}
	defer rows.Close()
	var out []model.Count
	var distinct, missing int64
	for rows.Next() {
		var v *string
		var n int64
		if err := rows.Scan(&v, &n); err != nil {
			return nil, 0, 0, err
		}
		if v == nil {
			missing = n
			continue
		}
		distinct++
		if len(out) < top {
			out = append(out, model.Count{Value: *v, Count: n})
		}
	}
	return out, distinct, missing, rows.Err()
}

// ScanMembers streams a version's members in order.
func (r *QualityRepository) ScanMembers(ctx context.Context, tenantID, versionID, key string, each func(model.QualityMember) error) error {
	rows, err := r.db.Pool.Query(ctx, `SELECT path, COALESCE(split, ''), COALESCE(checksum_sha256, ''),
		COALESCE(member_metadata->>$3, metadata->>$3, '')
		FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2 ORDER BY ordinal`, tenantID, versionID, key)
	if err != nil {
		return fmt.Errorf("scan members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m model.QualityMember
		if err := rows.Scan(&m.Path, &m.Split, &m.Checksum, &m.Subject); err != nil {
			return err
		}
		if err := each(m); err != nil {
			return err
		}
	}
	return rows.Err()
}

// HashOverlap counts the evaluation version's members whose SHA-256 is
// also in the training version, with up to limit examples, and how many
// evaluation members have no checksum to compare.
func (r *QualityRepository) HashOverlap(ctx context.Context, tenantID, trainID, evalID string, limit int) (int64, int64, int64, []model.LeakageExample, error) {
	var overlap, members, unhashed int64
	err := r.db.Pool.QueryRow(ctx, `SELECT
		    (SELECT count(*) FROM dataset_version_members e WHERE e.tenant_id = $1 AND e.version_id = $3 AND e.checksum_sha256 IS NOT NULL
		        AND EXISTS (SELECT 1 FROM dataset_version_members t WHERE t.tenant_id = $1 AND t.version_id = $2 AND t.checksum_sha256 = e.checksum_sha256)),
		    (SELECT count(*) FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $3),
		    (SELECT count(*) FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $3 AND checksum_sha256 IS NULL)`,
		tenantID, trainID, evalID).Scan(&overlap, &members, &unhashed)
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("count hash overlap: %w", err)
	}
	rows, err := r.db.Pool.Query(ctx, `SELECT e.path, min(t.path), e.checksum_sha256
		FROM dataset_version_members e JOIN dataset_version_members t
		    ON t.tenant_id = $1 AND t.version_id = $2 AND t.checksum_sha256 = e.checksum_sha256
		WHERE e.tenant_id = $1 AND e.version_id = $3
		GROUP BY e.ordinal, e.path, e.checksum_sha256 ORDER BY e.ordinal LIMIT $4`, tenantID, trainID, evalID, limit)
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("list hash overlap: %w", err)
	}
	defer rows.Close()
	examples := []model.LeakageExample{}
	for rows.Next() {
		var x model.LeakageExample
		if err := rows.Scan(&x.EvalPath, &x.TrainPath, &x.Key); err != nil {
			return 0, 0, 0, nil, err
		}
		examples = append(examples, x)
	}
	return overlap, members, unhashed, examples, rows.Err()
}

// ─── Leakage checks ───────────────────────────────────────────────────

const leakageColumns = `id, tenant_id, COALESCE(run_id, ''), train_version_id, eval_version_id, method, state, overlap, eval_members,
	examples, detail, parameters, created_by, created_at`

func scanLeakage(row pgx.Row) (model.LeakageCheck, error) {
	var c model.LeakageCheck
	var examples, params []byte
	err := row.Scan(&c.ID, &c.TenantID, &c.RunID, &c.TrainVersionID, &c.EvalVersionID, &c.Method, &c.State, &c.Overlap, &c.EvalMembers,
		&examples, &c.Detail, &params, &c.CreatedBy, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(examples, &c.Examples); err != nil {
		return c, err
	}
	return c, json.Unmarshal(params, &c.Parameters)
}

func (r *QualityRepository) AddLeakage(ctx context.Context, c model.LeakageCheck) error {
	examples, err := json.Marshal(c.Examples)
	if err != nil {
		return err
	}
	params, err := json.Marshal(c.Parameters)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `INSERT INTO leakage_checks (id, tenant_id, run_id, train_version_id, eval_version_id, method, state,
		overlap, eval_members, examples, detail, parameters, created_by, created_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		c.ID, c.TenantID, c.RunID, c.TrainVersionID, c.EvalVersionID, c.Method, c.State, c.Overlap, c.EvalMembers, examples, c.Detail,
		params, c.CreatedBy, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("record leakage check: %w", err)
	}
	return nil
}

// Leakage lists checks of a run, or (runID empty) those an evaluation
// version took part in, newest first.
func (r *QualityRepository) Leakage(ctx context.Context, tenantID, runID, versionID string, limit int) ([]model.LeakageCheck, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+leakageColumns+` FROM leakage_checks
		WHERE tenant_id = $1 AND ($2 = '' OR run_id = $2) AND ($3 = '' OR eval_version_id = $3 OR train_version_id = $3)
		ORDER BY created_at DESC, id LIMIT $4`, tenantID, runID, versionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list leakage checks: %w", err)
	}
	defer rows.Close()
	out := []model.LeakageCheck{}
	for rows.Next() {
		c, err := scanLeakage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *QualityRepository) RunInputs(ctx context.Context, tenantID, runID string) ([]model.RunInput, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT i.version_id, v.dataset_id, COALESCE(i.role, '') FROM training_run_inputs i
		JOIN dataset_versions v ON v.tenant_id = i.tenant_id AND v.id = i.version_id
		WHERE i.tenant_id = $1 AND i.run_id = $2 ORDER BY i.version_id`, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("list run inputs: %w", err)
	}
	defer rows.Close()
	var out []model.RunInput
	for rows.Next() {
		var in model.RunInput
		if err := rows.Scan(&in.VersionID, &in.DatasetID, &in.Role); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// RunsToCheck lists runs with a training input and an evaluation input
// not yet compared by hash.
func (r *QualityRepository) RunsToCheck(ctx context.Context, limit int) ([][2]string, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT DISTINCT t.tenant_id, t.run_id FROM training_run_inputs t
		JOIN training_run_inputs e ON e.tenant_id = t.tenant_id AND e.run_id = t.run_id AND e.role IN ('VALIDATION', 'TEST')
		WHERE t.role = 'TRAIN' AND NOT EXISTS (SELECT 1 FROM leakage_checks c WHERE c.tenant_id = t.tenant_id AND c.run_id = t.run_id
		    AND c.train_version_id = t.version_id AND c.eval_version_id = e.version_id AND c.method = 'EXACT_HASH')
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list runs to check: %w", err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var t [2]string
		if err := rows.Scan(&t[0], &t[1]); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ─── Releases ─────────────────────────────────────────────────────────

// SetRelease labels a version, once.
func (r *QualityRepository) SetRelease(ctx context.Context, tenantID, versionID, label, notes, actor string, at time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE dataset_versions SET release_label = $3, release_notes = $4, released_by = $5, released_at = $6
		WHERE tenant_id = $1 AND id = $2 AND release_label IS NULL`, tenantID, versionID, label, notes, actor, at)
	if isUniqueViolation(err) {
		return model.ErrReleaseTaken
	}
	if err != nil {
		return fmt.Errorf("label release: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: the version is already released", model.ErrReleaseTaken)
	}
	return nil
}

const releaseColumns = `release_label, id, version, COALESCE(release_notes, ''), member_count, total_bytes, COALESCE(released_by, ''), released_at`

func (r *QualityRepository) Releases(ctx context.Context, tenantID, datasetID string) ([]model.Release, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT `+releaseColumns+` FROM dataset_versions
		WHERE tenant_id = $1 AND dataset_id = $2 AND release_label IS NOT NULL ORDER BY version`, tenantID, datasetID)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer rows.Close()
	out := []model.Release{}
	for rows.Next() {
		var rel model.Release
		if err := rows.Scan(&rel.Label, &rel.VersionID, &rel.Version, &rel.Notes, &rel.Members, &rel.Bytes, &rel.ReleasedBy, &rel.ReleasedAt); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}
