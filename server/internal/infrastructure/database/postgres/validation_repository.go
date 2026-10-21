package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Validation profiles, jobs, findings and reports. A job's member pass
// commits batch by batch: findings (stable IDs, so a batch done twice
// writes them once), counters and the checkpoint together, and only while
// the job is leased to the worker that did the batch.

// ─── Profiles ─────────────────────────────────────────────────────────

const profileColumns = `id, version, tenant_id, name, description, definition, created_by, created_at`

func scanProfile(row pgx.Row) (model.ValidationProfile, error) {
	var p model.ValidationProfile
	var definition []byte
	if err := row.Scan(&p.ID, &p.Version, &p.TenantID, &p.Name, &p.Description, &definition, &p.CreatedBy, &p.CreatedAt); err != nil {
		return p, err
	}
	if err := json.Unmarshal(definition, &p.Definition); err != nil {
		return p, fmt.Errorf("decode profile: %w", err)
	}
	return p, nil
}

// CreateProfile writes a profile's next version: version 1 under p.ID for
// a new name, or the next version under the name's existing ID.
func (r *DatasetRepository) CreateProfile(ctx context.Context, p model.ValidationProfile) (model.ValidationProfile, error) {
	definition, err := json.Marshal(p.Definition)
	if err != nil {
		return p, err
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return p, fmt.Errorf("begin profile: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('profile:' || $1 || ':' || $2))`, p.TenantID, p.Name); err != nil {
		return p, fmt.Errorf("lock profile name: %w", err)
	}
	var id string
	var latest int
	err = tx.QueryRow(ctx, `
		SELECT id, version FROM validation_profiles WHERE tenant_id = $1 AND name = $2 ORDER BY version DESC LIMIT 1
	`, p.TenantID, p.Name).Scan(&id, &latest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		id, latest = p.ID, 0
	case err != nil:
		return p, fmt.Errorf("find profile: %w", err)
	}
	stored, err := scanProfile(tx.QueryRow(ctx, `
		INSERT INTO validation_profiles (id, version, tenant_id, name, description, definition, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+profileColumns, id, latest+1, p.TenantID, p.Name, p.Description, definition, p.CreatedBy, p.CreatedAt))
	if err != nil {
		return p, fmt.Errorf("create profile: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return p, fmt.Errorf("commit profile: %w", err)
	}
	return stored, nil
}

// GetProfile returns one version of a tenant's profile; version 0 is the
// latest.
func (r *DatasetRepository) GetProfile(ctx context.Context, tenantID, profileID string, version int) (model.ValidationProfile, error) {
	p, err := scanProfile(r.db.Pool.QueryRow(ctx, `
		SELECT `+profileColumns+` FROM validation_profiles
		WHERE tenant_id = $1 AND id = $2 AND ($3 = 0 OR version = $3)
		ORDER BY version DESC LIMIT 1
	`, tenantID, profileID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, model.ErrProfileNotFound
	}
	if err != nil {
		return p, fmt.Errorf("get profile: %w", err)
	}
	return p, nil
}

// ListProfiles returns the latest version of each of the tenant's
// profiles.
func (r *DatasetRepository) ListProfiles(ctx context.Context, tenantID string) ([]model.ValidationProfile, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT ON (id) `+profileColumns+` FROM validation_profiles
		WHERE tenant_id = $1 ORDER BY id, version DESC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	return collect(rows, scanProfile)
}

// ─── Jobs ─────────────────────────────────────────────────────────────

const validationColumns = `id, tenant_id, dataset_id, dataset_version_id, profile_id, profile_version, profile_snapshot,
	mode, sample_size, readiness, state, total_items, processed_items, passed_items, warning_items, failed_items,
	content_skipped, content_examined, bytes_total, bytes_read, rule_counts, checkpoint_ordinal, attempts, max_attempts,
	cancel_requested, COALESCE(last_error, ''), COALESCE(report_id, ''), COALESCE(idempotency_key, ''), created_by,
	created_at, started_at, completed_at, updated_at`

func scanValidation(row pgx.Row) (model.ValidationJob, error) {
	var j model.ValidationJob
	var profile, counts []byte
	var mode, state string
	err := row.Scan(&j.ID, &j.TenantID, &j.DatasetID, &j.DatasetVersionID, &j.ProfileID, &j.ProfileVersion, &profile,
		&mode, &j.SampleSize, &j.Readiness, &state, &j.TotalItems, &j.ProcessedItems, &j.PassedItems, &j.WarningItems,
		&j.FailedItems, &j.ContentSkipped, &j.ContentExamined, &j.BytesTotal, &j.BytesRead, &counts, &j.CheckpointOrdinal,
		&j.Attempts, &j.MaxAttempts, &j.CancelRequested, &j.LastError, &j.ReportID, &j.IdempotencyKey, &j.CreatedBy,
		&j.CreatedAt, &j.StartedAt, &j.CompletedAt, &j.UpdatedAt)
	if err != nil {
		return j, err
	}
	j.Mode, j.State = model.ValidationMode(mode), model.ValidationState(state)
	if err := json.Unmarshal(profile, &j.Profile); err != nil {
		return j, fmt.Errorf("decode profile snapshot: %w", err)
	}
	if err := json.Unmarshal(counts, &j.RuleCounts); err != nil {
		return j, fmt.Errorf("decode rule counts: %w", err)
	}
	return j, nil
}

// CreateValidation queues a validation of a dataset version, with the
// event that starts it. Validating the dataset's current version while
// the draft still matches it moves the dataset to VALIDATING. An
// idempotency key used before returns its job; maxActive caps the
// tenant's unfinished validations.
func (r *DatasetRepository) CreateValidation(ctx context.Context, job model.ValidationJob, maxActive int, ev model.OutboxEvent) (model.ValidationJob, bool, error) {
	profile, err := json.Marshal(job.Profile)
	if err != nil {
		return job, false, err
	}
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return job, false, fmt.Errorf("begin validation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	d, err := lockDraft(ctx, tx, job.TenantID, job.DatasetID)
	if err != nil {
		return job, false, err
	}
	if job.IdempotencyKey != "" {
		existing, err := scanValidation(tx.QueryRow(ctx, `
			SELECT `+validationColumns+` FROM validation_jobs WHERE dataset_id = $1 AND idempotency_key = $2
		`, job.DatasetID, job.IdempotencyKey))
		if err == nil {
			if existing.DatasetVersionID != job.DatasetVersionID || existing.ProfileID != job.ProfileID ||
				existing.ProfileVersion != job.ProfileVersion || existing.Mode != job.Mode ||
				existing.Readiness != job.Readiness || existing.SampleSize != job.SampleSize {
				return existing, false, model.ErrValidationConflict
			}
			return existing, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return job, false, fmt.Errorf("find validation: %w", err)
		}
	}

	var membersRevision, members int64
	err = tx.QueryRow(ctx, `
		SELECT members_revision, member_count FROM dataset_versions WHERE tenant_id = $1 AND id = $2 AND dataset_id = $3
	`, job.TenantID, job.DatasetVersionID, job.DatasetID).Scan(&membersRevision, &members)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, false, model.ErrDatasetVersionNotFound
	}
	if err != nil {
		return job, false, fmt.Errorf("read version: %w", err)
	}
	var active int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM validation_jobs WHERE tenant_id = $1 AND state IN ('QUEUED', 'RUNNING')
	`, job.TenantID).Scan(&active); err != nil {
		return job, false, fmt.Errorf("count validations: %w", err)
	}
	if maxActive > 0 && active >= maxActive {
		return job, false, fmt.Errorf("%w: at most %d validations at a time", model.ErrTooManyJobs, maxActive)
	}

	stored, err := scanValidation(tx.QueryRow(ctx, `
		INSERT INTO validation_jobs (id, tenant_id, dataset_id, dataset_version_id, profile_id, profile_version,
		                             profile_snapshot, mode, sample_size, readiness, state, total_items, max_attempts,
		                             next_attempt_at, idempotency_key, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'QUEUED', $11, $12, $13, NULLIF($14, ''), $15, $13, $13)
		RETURNING `+validationColumns,
		job.ID, job.TenantID, job.DatasetID, job.DatasetVersionID, job.ProfileID, job.ProfileVersion, profile,
		string(job.Mode), job.SampleSize, job.Readiness, members, job.MaxAttempts, job.CreatedAt, job.IdempotencyKey, job.CreatedBy))
	if err != nil {
		return job, false, fmt.Errorf("create validation: %w", err)
	}
	if err := insertOutbox(ctx, tx, ev); err != nil {
		return job, false, err
	}
	if d.CurrentVersionID == job.DatasetVersionID && d.MembersRevision == membersRevision &&
		(d.Lifecycle == model.LifecycleValidating || model.CanTransitionDataset(d.Lifecycle, model.LifecycleValidating)) {
		if _, err := tx.Exec(ctx, `
			UPDATE datasets SET lifecycle_status = 'VALIDATING', revision = revision + 1, updated_at = $3
			WHERE tenant_id = $1 AND id = $2
		`, job.TenantID, job.DatasetID, job.CreatedAt); err != nil {
			return job, false, fmt.Errorf("mark dataset validating: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) && job.IdempotencyKey != "" {
			existing, err := scanValidation(r.db.Pool.QueryRow(ctx, `
				SELECT `+validationColumns+` FROM validation_jobs WHERE dataset_id = $1 AND idempotency_key = $2
			`, job.DatasetID, job.IdempotencyKey))
			return existing, false, err
		}
		return job, false, fmt.Errorf("commit validation: %w", err)
	}
	return stored, true, nil
}

func (r *DatasetRepository) GetValidation(ctx context.Context, tenantID, jobID string) (model.ValidationJob, error) {
	j, err := scanValidation(r.db.Pool.QueryRow(ctx,
		`SELECT `+validationColumns+` FROM validation_jobs WHERE tenant_id = $1 AND id = $2`, tenantID, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return j, model.ErrValidationNotFound
	}
	if err != nil {
		return j, fmt.Errorf("get validation: %w", err)
	}
	return j, nil
}

// timeCursor is a position in a newest-first listing.
func encodeTimeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeTimeCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", model.ErrInvalidCursor
	}
	at, id, ok := strings.Cut(string(raw), "|")
	t, err := time.Parse(time.RFC3339Nano, at)
	if !ok || err != nil || id == "" {
		return time.Time{}, "", model.ErrInvalidCursor
	}
	return t, id, nil
}

// ListValidations pages a dataset's validations, newest first.
func (r *DatasetRepository) ListValidations(ctx context.Context, tenantID, datasetID, after string, limit int) ([]model.ValidationJob, string, error) {
	at, id, err := decodeTimeCursor(after)
	if err != nil {
		return nil, "", err
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+validationColumns+` FROM validation_jobs
		WHERE tenant_id = $1 AND dataset_id = $2 AND ($3 = '' OR (created_at, id) < ($4, $3))
		ORDER BY created_at DESC, id DESC
		LIMIT $5
	`, tenantID, datasetID, id, at, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list validations: %w", err)
	}
	jobs, err := collect(rows, scanValidation)
	if err != nil || len(jobs) <= limit {
		return jobs, "", err
	}
	jobs = jobs[:limit]
	return jobs, encodeTimeCursor(jobs[limit-1].CreatedAt, jobs[limit-1].ID), nil
}

// ClaimValidations leases due validations to owner: queued ones, and
// running ones whose lease ran out. Due jobs are ranked within their
// tenant, and a tenant gets only as many as it has room for under
// perTenant running ones, so one tenant's backlog cannot hold every
// worker. The update checks the lease again on the row it locks: of two
// workers that picked the same job, only the first takes it.
func (r *DatasetRepository) ClaimValidations(ctx context.Context, owner, jobID string, now time.Time, lease time.Duration, perTenant, limit int) ([]model.ValidationJob, error) {
	rows, err := r.db.Pool.Query(ctx, `
		UPDATE validation_jobs SET state = 'RUNNING', lease_owner = $1, lease_expires_at = $2::timestamptz + $3::interval,
		                           attempts = attempts + 1, started_at = COALESCE(started_at, $2), updated_at = $2
		WHERE id IN (
		    SELECT due.id FROM (
		        SELECT j.id, j.next_attempt_at,
		               row_number() OVER (PARTITION BY j.tenant_id ORDER BY j.next_attempt_at, j.id) AS place,
		               (SELECT count(*) FROM validation_jobs AS running
		                WHERE running.tenant_id = j.tenant_id AND running.state = 'RUNNING' AND running.lease_expires_at >= $2) AS running
		        FROM validation_jobs AS j
		        WHERE j.state IN ('QUEUED', 'RUNNING') AND j.next_attempt_at <= $2
		          AND (j.lease_expires_at IS NULL OR j.lease_expires_at < $2) AND ($4 = '' OR j.id = $4)
		    ) AS due
		    WHERE due.place + due.running <= $5
		    ORDER BY due.next_attempt_at
		    LIMIT $6
		)
		AND state IN ('QUEUED', 'RUNNING') AND (lease_expires_at IS NULL OR lease_expires_at < $2)
		RETURNING `+validationColumns, owner, now, lease, jobID, perTenant, limit)
	if err != nil {
		return nil, fmt.Errorf("claim validations: %w", err)
	}
	return collect(rows, scanValidation)
}

func (r *DatasetRepository) ExtendValidation(ctx context.Context, jobID, owner string, now time.Time, lease time.Duration) (bool, error) {
	var cancel bool
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE validation_jobs SET lease_expires_at = $3::timestamptz + $4::interval, updated_at = $3
		WHERE id = $1 AND lease_owner = $2 AND state = 'RUNNING'
		RETURNING cancel_requested
	`, jobID, owner, now, lease).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, model.ErrValidationState
	}
	if err != nil {
		return false, fmt.Errorf("extend validation lease: %w", err)
	}
	return cancel, nil
}

// MemberEvidence reads a page of a version's members with what is known
// about each object version now: whether its object and asset still exist,
// the asset's state and records, and its processing evidence.
func (r *DatasetRepository) MemberEvidence(ctx context.Context, tenantID, versionID string, after int64, limit int) ([]model.MemberEvidence, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT vm.ordinal, vm.object_id, vm.asset_id, vm.object_version, vm.path, COALESCE(vm.label, ''), COALESCE(vm.split, ''),
		       vm.size_bytes, vm.content_type, COALESCE(vm.checksum_sha256, ''), vm.metadata,
		       o.id IS NOT NULL, COALESCE(o.deleted_at IS NOT NULL, false),
		       COALESCE(a.status, ''), COALESCE(a.size_bytes, 0), COALESCE(a.checksum_sha256, ''), COALESCE(a.object_key, ''),
		       COALESCE(pj.verification_status, CASE WHEN pj.checksum_verified_at IS NOT NULL THEN 'PASSED' END, ''),
		       COALESCE(pj.checksum_source, ''), COALESCE(pj.scan_result, ''), COALESCE(a.storage_connection_id, ''),
		       COALESCE(a.storage_version_id, '')
		FROM dataset_version_members AS vm
		LEFT JOIN catalog_objects AS o ON o.id = vm.object_id AND o.tenant_id = vm.tenant_id
		LEFT JOIN assets AS a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id
		LEFT JOIN processing_jobs AS pj ON pj.asset_id = vm.asset_id AND pj.tenant_id = vm.tenant_id
		WHERE vm.tenant_id = $1 AND vm.version_id = $2 AND vm.ordinal > $3
		ORDER BY vm.ordinal
		LIMIT $4
	`, tenantID, versionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read member evidence: %w", err)
	}
	return collect(rows, func(row pgx.Row) (model.MemberEvidence, error) {
		var e model.MemberEvidence
		var metadata []byte
		var status string
		err := row.Scan(&e.Ordinal, &e.ObjectID, &e.AssetID, &e.ObjectVersion, &e.Path, &e.Label, &e.Split,
			&e.SizeBytes, &e.ContentType, &e.ChecksumSHA256, &metadata,
			&e.ObjectExists, &e.ObjectDeleted, &status, &e.AssetSizeBytes, &e.AssetChecksum, &e.ObjectKey,
			&e.VerificationStatus, &e.ChecksumSource, &e.ScanResult, &e.StorageConnectionID, &e.StorageVersionID)
		if err != nil {
			return e, err
		}
		e.AssetStatus = model.AssetStatus(status)
		e.Metadata, err = decodeMetadata(metadata)
		return e, err
	})
}

// CommitValidationBatch applies one batch of the member pass, if the job
// is still leased to the batch's worker and still at the batch's start.
func (r *DatasetRepository) CommitValidationBatch(ctx context.Context, b interfaces.ValidationBatch) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin validation batch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var raw []byte
	err = tx.QueryRow(ctx, `
		SELECT rule_counts FROM validation_jobs
		WHERE id = $1 AND lease_owner = $2 AND state = 'RUNNING' AND checkpoint_ordinal = $3
		FOR UPDATE
	`, b.JobID, b.Owner, b.From).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrValidationState
	}
	if err != nil {
		return fmt.Errorf("lock validation: %w", err)
	}
	counts := map[string]model.RuleCount{}
	if err := json.Unmarshal(raw, &counts); err != nil {
		return fmt.Errorf("decode rule counts: %w", err)
	}
	for rule, c := range b.RuleCounts {
		total := counts[rule]
		total.Checked += c.Checked
		total.Failed += c.Failed
		total.Skipped += c.Skipped
		total.Findings += c.Findings
		total.Read += c.Read
		counts[rule] = total
	}
	merged, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	if err := insertFindings(ctx, tx, b.Findings); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE validation_jobs SET checkpoint_ordinal = $2, processed_items = processed_items + $3,
		    passed_items = passed_items + $4, warning_items = warning_items + $5, failed_items = failed_items + $6,
		    content_skipped = content_skipped + $7, content_examined = content_examined + $8,
		    bytes_total = bytes_total + $9, bytes_read = bytes_read + $10, rule_counts = $11, attempts = 0, updated_at = $12
		WHERE id = $1
	`, b.JobID, b.To, b.Processed, b.Passed, b.Warnings, b.Failed, b.Skipped, b.Examined, b.BytesTotal, b.BytesRead,
		merged, b.Now); err != nil {
		return fmt.Errorf("move validation checkpoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit validation batch: %w", err)
	}
	return nil
}

// insertFindings writes findings; one with an ID already written is the
// same finding, found again.
func insertFindings(ctx context.Context, tx pgx.Tx, findings []model.Finding) error {
	if len(findings) == 0 {
		return nil
	}
	n := len(findings)
	jobs, ids, tenants, rules, severities, codes := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	objects, assets, messages, expected, observed, evidence, remediation := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	versions := make([]int32, n)
	created := make([]time.Time, n)
	for i, f := range findings {
		jobs[i], ids[i], tenants[i], rules[i], severities[i], codes[i] = f.JobID, f.ID, f.TenantID, f.RuleID, string(f.Severity), f.Code
		objects[i], assets[i], messages[i], expected[i], observed[i] = f.ObjectID, f.AssetID, f.Message, f.Expected, f.Observed
		evidence[i], remediation[i], versions[i], created[i] = f.Evidence, f.Remediation, int32(f.RuleVersion), f.CreatedAt
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO validation_findings (job_id, id, tenant_id, rule_id, rule_version, severity, code, object_id, asset_id,
		                                 message, expected, observed, evidence, remediation, created_at)
		SELECT f.job_id, f.id, f.tenant_id, f.rule_id, f.rule_version, f.severity, f.code, NULLIF(f.object_id, ''),
		       NULLIF(f.asset_id, ''), f.message, NULLIF(f.expected, ''), NULLIF(f.observed, ''), f.evidence,
		       NULLIF(f.remediation, ''), f.created_at
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::int[], $6::text[], $7::text[], $8::text[], $9::text[],
		            $10::text[], $11::text[], $12::text[], $13::text[], $14::text[], $15::timestamptz[])
		     AS f(job_id, id, tenant_id, rule_id, rule_version, severity, code, object_id, asset_id, message, expected,
		          observed, evidence, remediation, created_at)
		ON CONFLICT (job_id, id) DO NOTHING
	`, jobs, ids, tenants, rules, versions, severities, codes, objects, assets, messages, expected, observed, evidence,
		remediation, created)
	if err != nil {
		return fmt.Errorf("write findings: %w", err)
	}
	return nil
}

// Aggregates computes a version's distributions and verified-content
// groups in the database. Lists hold at most topN values; labels are
// listed whole when there are at most topN of them.
func (r *DatasetRepository) Aggregates(ctx context.Context, tenantID, versionID string, topN int) (model.VersionAggregates, error) {
	var a model.VersionAggregates
	err := r.db.Pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT asset_id), COALESCE(sum(size_bytes), 0), COALESCE(min(size_bytes), 0),
		       COALESCE(max(size_bytes), 0), count(*) FILTER (WHERE label IS NULL OR label = ''),
		       count(DISTINCT label) FILTER (WHERE label <> '')
		FROM dataset_version_members WHERE tenant_id = $1 AND version_id = $2
	`, tenantID, versionID).Scan(&a.Members, &a.DistinctAssets, &a.TotalBytes, &a.MinSize, &a.MaxSize, &a.Unlabeled, &a.LabelCount)
	if err != nil {
		return a, fmt.Errorf("count version: %w", err)
	}

	distributions := []struct {
		target *[]model.Count
		expr   string
		where  string
	}{
		{&a.Splits, `COALESCE(split, '')`, `true`},
		{&a.Labels, `label`, `label <> ''`},
		{&a.ContentTypes, `content_type`, `true`},
		{&a.Extensions, `COALESCE(lower(substring(path FROM '\.([^./]+)$')), '')`, `true`},
	}
	for _, d := range distributions {
		rows, err := r.db.Pool.Query(ctx, `
			SELECT `+d.expr+`, count(*) FROM dataset_version_members
			WHERE tenant_id = $1 AND version_id = $2 AND `+d.where+`
			GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT $3
		`, tenantID, versionID, topN)
		if err != nil {
			return a, fmt.Errorf("version distribution: %w", err)
		}
		*d.target, err = collect(rows, func(row pgx.Row) (model.Count, error) {
			var c model.Count
			err := row.Scan(&c.Value, &c.Count)
			return c, err
		})
		if err != nil {
			return a, err
		}
	}

	// The evidence behind the members, counted by the database.
	evidence := []struct {
		target *[]model.Count
		expr   string
	}{
		{&a.Verification, `COALESCE(pj.verification_status, CASE WHEN pj.checksum_verified_at IS NOT NULL THEN 'PASSED' END, 'NONE')`},
		{&a.ScanResults, `COALESCE(pj.scan_result, 'NONE')`},
		{&a.AssetStatuses, `COALESCE(a.status, 'MISSING')`},
	}
	for _, e := range evidence {
		rows, err := r.db.Pool.Query(ctx, `
			SELECT `+e.expr+`, count(*)
			FROM dataset_version_members AS vm
			LEFT JOIN assets AS a ON a.id = vm.asset_id AND a.tenant_id = vm.tenant_id
			LEFT JOIN processing_jobs AS pj ON pj.asset_id = vm.asset_id AND pj.tenant_id = vm.tenant_id
			WHERE vm.tenant_id = $1 AND vm.version_id = $2
			GROUP BY 1 ORDER BY 2 DESC, 1
		`, tenantID, versionID)
		if err != nil {
			return a, fmt.Errorf("count evidence: %w", err)
		}
		*e.target, err = collect(rows, func(row pgx.Row) (model.Count, error) {
			var c model.Count
			err := row.Scan(&c.Value, &c.Count)
			return c, err
		})
		if err != nil {
			return a, err
		}
	}

	// Verified content: members whose checksum processing verified against
	// the stored bytes. Unverified checksums never make two members equal.
	const verified = `
		SELECT vm.checksum_sha256 AS checksum, vm.object_id, COALESCE(vm.split, '') AS split
		FROM dataset_version_members AS vm
		JOIN processing_jobs AS pj ON pj.asset_id = vm.asset_id AND pj.tenant_id = vm.tenant_id
		WHERE vm.tenant_id = $1 AND vm.version_id = $2 AND COALESCE(vm.checksum_sha256, '') <> ''
		  AND (pj.verification_status = 'PASSED' OR (pj.verification_status IS NULL AND pj.checksum_verified_at IS NOT NULL))`
	if err := r.db.Pool.QueryRow(ctx, `SELECT count(*) FROM (`+verified+`) AS v`, tenantID, versionID).Scan(&a.VerifiedChecksums); err != nil {
		return a, fmt.Errorf("count verified checksums: %w", err)
	}
	groups := []struct {
		target *[]model.ChecksumGroup
		total  *int64
		having string
	}{
		{&a.Duplicates, &a.DuplicateGroups, `count(*) > 1`},
		{&a.Leaks, &a.LeakGroups, `count(DISTINCT split) FILTER (WHERE split <> '') > 1`},
	}
	for _, g := range groups {
		rows, err := r.db.Pool.Query(ctx, `
			SELECT checksum, count(*), (array_agg(object_id ORDER BY object_id))[1:5],
			       array_agg(DISTINCT split) FILTER (WHERE split <> ''), count(*) OVER ()
			FROM (`+verified+`) AS v
			GROUP BY checksum HAVING `+g.having+`
			ORDER BY count(*) DESC, checksum
			LIMIT $3
		`, tenantID, versionID, topN)
		if err != nil {
			return a, fmt.Errorf("group verified checksums: %w", err)
		}
		*g.target, err = collect(rows, func(row pgx.Row) (model.ChecksumGroup, error) {
			var c model.ChecksumGroup
			err := row.Scan(&c.Checksum, &c.Members, &c.ObjectIDs, &c.Splits, g.total)
			if c.Splits == nil {
				c.Splits = []string{}
			}
			return c, err
		})
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

// settleDataset moves a VALIDATING dataset to the outcome of a validation
// that ended: outcome itself when the validated version is still current
// and the draft still matches it, DRAFT otherwise. While another
// validation of the current version is unfinished the dataset stays
// VALIDATING.
func settleDataset(ctx context.Context, tx pgx.Tx, tenantID, datasetID, versionID, jobID string, outcome model.DatasetLifecycle, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE datasets AS d SET
		    lifecycle_status = CASE WHEN d.current_version_id = $3
		                             AND d.members_revision = (SELECT members_revision FROM dataset_versions WHERE id = $3)
		                            THEN $5 ELSE 'DRAFT' END,
		    revision = d.revision + 1, updated_at = $6
		WHERE d.tenant_id = $1 AND d.id = $2 AND d.lifecycle_status = 'VALIDATING'
		  AND NOT EXISTS (SELECT 1 FROM validation_jobs AS j
		                  WHERE j.dataset_id = d.id AND j.dataset_version_id = d.current_version_id AND j.id <> $4
		                    AND j.state IN ('QUEUED', 'RUNNING'))
	`, tenantID, datasetID, versionID, jobID, string(outcome), now)
	if err != nil {
		return fmt.Errorf("settle dataset: %w", err)
	}
	return nil
}

// CompleteValidation stores a finished job's report and dataset-level
// findings, ends the job, records the outcome on the version and dataset,
// and links the version to its report. Completing a job a second time
// (a retried delivery) changes nothing.
func (r *DatasetRepository) CompleteValidation(ctx context.Context, job model.ValidationJob, owner string, report model.ValidationReport, findings []model.Finding) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete validation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var state, leaseOwner, reportID string
	err = tx.QueryRow(ctx, `
		SELECT state, COALESCE(lease_owner, ''), COALESCE(report_id, '') FROM validation_jobs WHERE id = $1 FOR UPDATE
	`, job.ID).Scan(&state, &leaseOwner, &reportID)
	if err != nil {
		return fmt.Errorf("lock validation: %w", err)
	}
	if reportID != "" {
		return nil
	}
	if state != string(model.ValidationRunning) || leaseOwner != owner {
		return model.ErrValidationState
	}
	if err := insertFindings(ctx, tx, findings); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO validation_reports (id, tenant_id, job_id, dataset_id, dataset_version_id, profile_id, profile_version,
		                                mode, status, readiness, schema_version, body, summary, started_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, report.ID, report.TenantID, report.JobID, report.DatasetID, report.DatasetVersionID, report.ProfileID,
		report.ProfileVersion, string(report.Mode), string(report.Status), report.Readiness, report.SchemaVersion,
		[]byte(report.Body), report.Summary, report.StartedAt, report.CompletedAt); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	jobState := model.ValidationCompleted
	switch report.Status {
	case model.LifecycleReadyWithWarnings:
		jobState = model.ValidationCompletedWithWarnings
	case model.LifecycleFailed:
		jobState = model.ValidationFailed
	}
	if _, err := tx.Exec(ctx, `
		UPDATE validation_jobs SET state = $2, report_id = $3, lease_owner = NULL, lease_expires_at = NULL,
		                           completed_at = $4, updated_at = $4
		WHERE id = $1
	`, job.ID, string(jobState), report.ID, report.CompletedAt); err != nil {
		return fmt.Errorf("end validation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE dataset_versions SET validation_status = $2, latest_report_id = $3 WHERE id = $1
	`, job.DatasetVersionID, string(report.Status), report.ID); err != nil {
		return fmt.Errorf("record version outcome: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE datasets SET latest_report_id = $3 WHERE tenant_id = $1 AND id = $2
	`, job.TenantID, job.DatasetID, report.ID); err != nil {
		return fmt.Errorf("record latest report: %w", err)
	}
	if err := settleDataset(ctx, tx, job.TenantID, job.DatasetID, job.DatasetVersionID, job.ID, report.Status, report.CompletedAt); err != nil {
		return err
	}
	err = link(ctx, tx, job.TenantID, model.LineageEdge{
		From: model.LineageRef{Kind: model.NodeDatasetVersion, RefID: job.DatasetVersionID},
		To:   model.LineageRef{Kind: model.NodeValidationReport, RefID: report.ID}, Relation: model.RelValidatedBy,
		Actor: job.CreatedBy, JobID: job.ID,
		Details: map[string]any{"profile": job.ProfileID, "profile_version": job.ProfileVersion, "mode": string(job.Mode),
			"status": string(report.Status)},
	}, report.CompletedAt, true)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit validation: %w", err)
	}
	return nil
}

// FailValidation ends a job that could not run (no report).
func (r *DatasetRepository) FailValidation(ctx context.Context, jobID, owner, lastError string, now time.Time) error {
	return r.endValidation(ctx, `id = $1 AND lease_owner = $2 AND state = 'RUNNING'`, model.ValidationFailed, lastError, now, jobID, owner)
}

// StopValidation ends a running job its worker found cancelled.
func (r *DatasetRepository) StopValidation(ctx context.Context, jobID, owner string, now time.Time) error {
	return r.endValidation(ctx, `id = $1 AND lease_owner = $2 AND state = 'RUNNING'`, model.ValidationCancelled, "", now, jobID, owner)
}

func (r *DatasetRepository) endValidation(ctx context.Context, where string, state model.ValidationState, lastError string,
	now time.Time, args ...any) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin end validation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	n := len(args)
	var tenantID, datasetID, versionID, jobID string
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		UPDATE validation_jobs SET state = $%d, last_error = COALESCE(NULLIF($%d, ''), last_error), lease_owner = NULL,
		                           lease_expires_at = NULL, completed_at = $%d, updated_at = $%d
		WHERE %s
		RETURNING tenant_id, dataset_id, dataset_version_id, id
	`, n+1, n+2, n+3, n+3, where), append(args, string(state), lastError, now)...).Scan(&tenantID, &datasetID, &versionID, &jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrValidationState
	}
	if err != nil {
		return fmt.Errorf("end validation: %w", err)
	}
	if err := settleDataset(ctx, tx, tenantID, datasetID, versionID, jobID, model.LifecycleDraft, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit end validation: %w", err)
	}
	return nil
}

// RetryValidation gives up owner's lease until next; the checkpoint stays.
func (r *DatasetRepository) RetryValidation(ctx context.Context, jobID, owner string, next time.Time, lastError string, now time.Time) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE validation_jobs SET lease_owner = NULL, lease_expires_at = NULL, next_attempt_at = $3, last_error = $4, updated_at = $5
		WHERE id = $1 AND lease_owner = $2 AND state = 'RUNNING'
	`, jobID, owner, next, lastError, now)
	if err != nil {
		return fmt.Errorf("retry validation: %w", err)
	}
	return nil
}

// CancelValidation cancels a job no worker holds at once; a worker holding
// one stops at its next batch.
func (r *DatasetRepository) CancelValidation(ctx context.Context, tenantID, jobID string, now time.Time) (model.ValidationJob, error) {
	j, err := r.GetValidation(ctx, tenantID, jobID)
	if err != nil {
		return j, err
	}
	if j.State.Terminal() {
		return j, model.ErrValidationState
	}
	err = r.endValidation(ctx, `tenant_id = $1 AND id = $2 AND state IN ('QUEUED', 'RUNNING')
		AND (state = 'QUEUED' OR lease_expires_at IS NULL OR lease_expires_at < $3)`,
		model.ValidationCancelled, "", now, tenantID, jobID, now)
	if errors.Is(err, model.ErrValidationState) {
		// Held by a worker: ask it to stop.
		if _, err := r.db.Pool.Exec(ctx, `
			UPDATE validation_jobs SET cancel_requested = true, updated_at = $3
			WHERE tenant_id = $1 AND id = $2 AND state IN ('QUEUED', 'RUNNING')
		`, tenantID, jobID, now); err != nil {
			return j, fmt.Errorf("request cancellation: %w", err)
		}
	} else if err != nil {
		return j, err
	}
	return r.GetValidation(ctx, tenantID, jobID)
}

// ─── Reports and findings ─────────────────────────────────────────────

const reportColumns = `id, tenant_id, job_id, dataset_id, dataset_version_id, profile_id, profile_version, mode, status,
	readiness, schema_version, body, summary, started_at, completed_at`

func scanReport(row pgx.Row) (model.ValidationReport, error) {
	var rep model.ValidationReport
	var mode, status string
	var body []byte
	err := row.Scan(&rep.ID, &rep.TenantID, &rep.JobID, &rep.DatasetID, &rep.DatasetVersionID, &rep.ProfileID,
		&rep.ProfileVersion, &mode, &status, &rep.Readiness, &rep.SchemaVersion, &body, &rep.Summary, &rep.StartedAt, &rep.CompletedAt)
	rep.Mode, rep.Status, rep.Body = model.ValidationMode(mode), model.DatasetLifecycle(status), body
	return rep, err
}

func (r *DatasetRepository) GetReport(ctx context.Context, tenantID, reportID string) (model.ValidationReport, error) {
	rep, err := scanReport(r.db.Pool.QueryRow(ctx,
		`SELECT `+reportColumns+` FROM validation_reports WHERE tenant_id = $1 AND id = $2`, tenantID, reportID))
	if errors.Is(err, pgx.ErrNoRows) {
		return rep, model.ErrReportNotFound
	}
	if err != nil {
		return rep, fmt.Errorf("get report: %w", err)
	}
	return rep, nil
}

// ListReports pages a dataset's reports, newest first, optionally only
// readiness assessments.
func (r *DatasetRepository) ListReports(ctx context.Context, tenantID, datasetID string, readinessOnly bool, after string, limit int) ([]model.ValidationReport, string, error) {
	at, id, err := decodeTimeCursor(after)
	if err != nil {
		return nil, "", err
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+reportColumns+` FROM validation_reports
		WHERE tenant_id = $1 AND dataset_id = $2 AND (NOT $3 OR readiness) AND ($4 = '' OR (completed_at, id) < ($5, $4))
		ORDER BY completed_at DESC, id DESC
		LIMIT $6
	`, tenantID, datasetID, readinessOnly, id, at, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list reports: %w", err)
	}
	reports, err := collect(rows, scanReport)
	if err != nil || len(reports) <= limit {
		return reports, "", err
	}
	reports = reports[:limit]
	return reports, encodeTimeCursor(reports[limit-1].CompletedAt, reports[limit-1].ID), nil
}

// ListFindings pages a job's findings by ID, optionally of one severity
// or rule.
func (r *DatasetRepository) ListFindings(ctx context.Context, tenantID, jobID string, f interfaces.FindingFilter) (model.FindingPage, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT job_id, id, tenant_id, rule_id, rule_version, severity, code, COALESCE(object_id, ''), COALESCE(asset_id, ''),
		       message, COALESCE(expected, ''), COALESCE(observed, ''), evidence, COALESCE(remediation, ''), created_at
		FROM validation_findings
		WHERE tenant_id = $1 AND job_id = $2 AND ($3 = '' OR severity = $3) AND ($4 = '' OR rule_id = $4) AND id > $5
		ORDER BY id
		LIMIT $6
	`, tenantID, jobID, string(f.Severity), f.RuleID, f.After, f.Limit+1)
	if err != nil {
		return model.FindingPage{}, fmt.Errorf("list findings: %w", err)
	}
	findings, err := collect(rows, func(row pgx.Row) (model.Finding, error) {
		var f model.Finding
		var severity string
		err := row.Scan(&f.JobID, &f.ID, &f.TenantID, &f.RuleID, &f.RuleVersion, &severity, &f.Code, &f.ObjectID, &f.AssetID,
			&f.Message, &f.Expected, &f.Observed, &f.Evidence, &f.Remediation, &f.CreatedAt)
		f.Severity = model.Severity(severity)
		return f, err
	})
	if err != nil {
		return model.FindingPage{}, err
	}
	page := model.FindingPage{Findings: findings}
	if len(findings) > f.Limit {
		page.Findings = findings[:f.Limit]
		page.After = page.Findings[f.Limit-1].ID
	}
	return page, nil
}

// ─── Recovery and gauges ──────────────────────────────────────────────

// ReleaseStuckDatasets returns datasets VALIDATING with no unfinished
// validation to DRAFT. Ending a job settles its dataset in the same
// transaction, so this only repairs what an operator's direct change or
// an older bug left behind.
func (r *DatasetRepository) ReleaseStuckDatasets(ctx context.Context, now time.Time) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE datasets AS d SET lifecycle_status = 'DRAFT', revision = d.revision + 1, updated_at = $1
		WHERE d.lifecycle_status = 'VALIDATING'
		  AND NOT EXISTS (SELECT 1 FROM validation_jobs AS j WHERE j.dataset_id = d.id AND j.state IN ('QUEUED', 'RUNNING'))
	`, now)
	if err != nil {
		return 0, fmt.Errorf("release stuck datasets: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *DatasetRepository) JobStats(ctx context.Context, now time.Time) (interfaces.DatasetJobStats, error) {
	stats := interfaces.DatasetJobStats{ImportsByState: map[string]int64{}, ValidationsByState: map[string]int64{}}
	for _, q := range []struct {
		table  string
		target map[string]int64
	}{{"dataset_manifest_imports", stats.ImportsByState}, {"validation_jobs", stats.ValidationsByState}} {
		rows, err := r.db.Pool.Query(ctx, `SELECT state, count(*) FROM `+q.table+` GROUP BY state`)
		if err != nil {
			return stats, fmt.Errorf("count %s: %w", q.table, err)
		}
		for rows.Next() {
			var state string
			var n int64
			if err := rows.Scan(&state, &n); err != nil {
				rows.Close()
				return stats, err
			}
			q.target[state] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return stats, err
		}
	}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM $1::timestamptz - min(due)), 0), COALESCE(sum(stuck), 0)
		FROM (
		    SELECT next_attempt_at AS due, (state = 'RUNNING' AND lease_expires_at < $1)::int AS stuck
		    FROM dataset_manifest_imports WHERE state IN ('QUEUED', 'RUNNING')
		  UNION ALL
		    SELECT next_attempt_at, (state = 'RUNNING' AND lease_expires_at < $1)::int
		    FROM validation_jobs WHERE state IN ('QUEUED', 'RUNNING')
		) AS live WHERE due <= $1 OR stuck = 1
	`, now).Scan(&stats.OldestQueuedSeconds, &stats.Stuck)
	if err != nil {
		return stats, fmt.Errorf("job ages: %w", err)
	}
	return stats, nil
}
