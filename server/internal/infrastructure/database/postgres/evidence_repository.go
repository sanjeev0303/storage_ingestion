package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// EvidenceRepository keeps evidence settings, chain anchors and audit
// reviews, and reads what evidence packs, provenance checks, model
// summaries and control evidence need (docs/evidence.md).
type EvidenceRepository struct {
	db *DB
}

func NewEvidenceRepository(db *DB) *EvidenceRepository {
	return &EvidenceRepository{db: db}
}

// ─── Settings ─────────────────────────────────────────────────────────

const evidenceSettingsColumns = `tenant_id, COALESCE(anchor_connection_id, ''), anchor_every_minutes, anchor_lock_days, review_every_days,
	work_start_hour, work_end_hour, timezone, bulk_read_threshold, require_lawful_basis, updated_by, updated_at`

func scanEvidenceSettings(row pgx.Row) (model.EvidenceSettings, error) {
	var s model.EvidenceSettings
	err := row.Scan(&s.TenantID, &s.AnchorConnectionID, &s.AnchorEveryMinutes, &s.AnchorLockDays, &s.ReviewEveryDays,
		&s.WorkStartHour, &s.WorkEndHour, &s.Timezone, &s.BulkReadThreshold, &s.RequireLawfulBasis, &s.UpdatedBy, &s.UpdatedAt)
	return s, err
}

func (r *EvidenceRepository) Settings(ctx context.Context, tenantID string) (model.EvidenceSettings, error) {
	s, err := scanEvidenceSettings(r.db.Pool.QueryRow(ctx, `SELECT `+evidenceSettingsColumns+` FROM evidence_settings WHERE tenant_id = $1`, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.DefaultEvidenceSettings(tenantID), nil
	}
	if err != nil {
		return s, fmt.Errorf("read evidence settings: %w", err)
	}
	return s, nil
}

func (r *EvidenceRepository) PutSettings(ctx context.Context, s model.EvidenceSettings) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO evidence_settings (tenant_id, anchor_connection_id, anchor_every_minutes, anchor_lock_days, review_every_days,
		    work_start_hour, work_end_hour, timezone, bulk_read_threshold, require_lawful_basis, updated_by, updated_at)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id) DO UPDATE SET anchor_connection_id = NULLIF($2, ''), anchor_every_minutes = $3, anchor_lock_days = $4,
		    review_every_days = $5, work_start_hour = $6, work_end_hour = $7, timezone = $8, bulk_read_threshold = $9,
		    require_lawful_basis = $10, updated_by = $11, updated_at = $12
	`, s.TenantID, s.AnchorConnectionID, s.AnchorEveryMinutes, s.AnchorLockDays, s.ReviewEveryDays, s.WorkStartHour, s.WorkEndHour,
		s.Timezone, s.BulkReadThreshold, s.RequireLawfulBasis, s.UpdatedBy, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store evidence settings: %w", err)
	}
	return nil
}

// Tenants lists every organization with audit events, with its settings
// (the defaults where it saved none).
func (r *EvidenceRepository) Tenants(ctx context.Context) ([]model.EvidenceSettings, error) {
	rows, err := r.db.Pool.Query(ctx, `SELECT chain FROM audit_chain_heads WHERE chain <> $1 ORDER BY chain`, model.PlatformChain)
	if err != nil {
		return nil, fmt.Errorf("list chains: %w", err)
	}
	chains, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]model.EvidenceSettings, 0, len(chains))
	for _, c := range chains {
		s, err := r.Settings(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ─── The chain ────────────────────────────────────────────────────────

// Head is a chain's newest entry.
func (r *EvidenceRepository) Head(ctx context.Context, chain string) (int64, []byte, error) {
	var seq int64
	var hash []byte
	err := r.db.Pool.QueryRow(ctx, `SELECT seq, hash FROM audit_chain_heads WHERE chain = $1`, chain).Scan(&seq, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil
	}
	return seq, hash, err
}

// HashAt is the hash of a chain's entry at seq; false when it was purged
// or never existed.
func (r *EvidenceRepository) HashAt(ctx context.Context, chain string, seq int64) ([]byte, bool, error) {
	var hash []byte
	err := r.db.Pool.QueryRow(ctx, `SELECT hash FROM audit_events WHERE chain = $1 AND seq = $2`, chain, seq).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		err = r.db.Pool.QueryRow(ctx, `SELECT hash FROM audit_checkpoints WHERE chain = $1 AND seq = $2`, chain, seq).Scan(&hash)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return hash, err == nil, err
}

// ExportedEvent is a chain entry with exactly what its hash covers, so a
// reader can recompute the chain without this platform (docs/evidence.md).
type ExportedEvent struct {
	Seq           int64           `json:"seq"`
	ID            string          `json:"id"`
	Chain         string          `json:"chain"`
	TenantID      string          `json:"tenant_id"`
	ActorType     string          `json:"actor_type"`
	ActorID       string          `json:"actor_id"`
	Action        string          `json:"action"`
	ResourceType  string          `json:"resource_type"`
	ResourceID    string          `json:"resource_id"`
	Outcome       string          `json:"outcome"`
	Reason        string          `json:"reason"`
	CorrelationID string          `json:"correlation_id"`
	SourceIP      string          `json:"source_ip"`
	CreatedAt     string          `json:"created_at"`
	Details       json.RawMessage `json:"details"`
	// DetailsJSON is the details exactly as hashed, as a string, so a
	// verifier need not re-encode them.
	DetailsJSON string `json:"details_json"`
	PrevHash    string `json:"prev_hash"`
	Hash        string `json:"hash"`
}

// Export pages a chain oldest first after afterSeq, from start (zero
// time: from the beginning) to end (zero: to now).
func (r *EvidenceRepository) Export(ctx context.Context, chain string, afterSeq int64, start, end time.Time, limit int) ([]ExportedEvent, error) {
	if end.IsZero() {
		end = time.Now().Add(time.Hour)
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT seq, id, chain, COALESCE(tenant_id, ''), actor_type, actor_id, action, resource_type, resource_id, outcome, reason,
		       correlation_id, source_ip, created_at, details, COALESCE(prev_hash, ''::bytea), hash
		FROM audit_events WHERE chain = $1 AND seq > $2 AND created_at >= $3 AND created_at < $4
		ORDER BY seq LIMIT $5`, chain, afterSeq, start, end, limit)
	if err != nil {
		return nil, fmt.Errorf("export audit events: %w", err)
	}
	defer rows.Close()
	var out []ExportedEvent
	for rows.Next() {
		var e ExportedEvent
		var at time.Time
		var details, prev, hash []byte
		if err := rows.Scan(&e.Seq, &e.ID, &e.Chain, &e.TenantID, &e.ActorType, &e.ActorID, &e.Action, &e.ResourceType, &e.ResourceID,
			&e.Outcome, &e.Reason, &e.CorrelationID, &e.SourceIP, &at, &details, &prev, &hash); err != nil {
			return nil, err
		}
		decoded, err := decodeDetails(details)
		if err != nil {
			return nil, err
		}
		canonical, err := canonicalDetails(decoded)
		if err != nil {
			return nil, err
		}
		e.CreatedAt, e.Details, e.DetailsJSON = at.UTC().Format(time.RFC3339Nano), canonical, string(canonical)
		e.PrevHash, e.Hash = hex.EncodeToString(prev), hex.EncodeToString(hash)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─── Anchors ──────────────────────────────────────────────────────────

const anchorColumns = `id, chain, seq, hash, body, signature, key_id, connection_id, object_key, object_version, locked_until, tsa_token, created_at`

func collectAnchors(rows pgx.Rows, err error) ([]model.AuditAnchor, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AuditAnchor{}
	for rows.Next() {
		var a model.AuditAnchor
		var hash []byte
		if err := rows.Scan(&a.ID, &a.Chain, &a.Seq, &hash, &a.Body, &a.Signature, &a.KeyID, &a.ConnectionID, &a.ObjectKey,
			&a.ObjectVersion, &a.LockedUntil, &a.TSAToken, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Hash = hex.EncodeToString(hash)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *EvidenceRepository) PutAnchor(ctx context.Context, a model.AuditAnchor) error {
	hash, err := hex.DecodeString(a.Hash)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `
		INSERT INTO audit_anchors (`+anchorColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (chain, seq) DO NOTHING
	`, a.ID, a.Chain, a.Seq, hash, a.Body, a.Signature, a.KeyID, a.ConnectionID, a.ObjectKey, a.ObjectVersion, a.LockedUntil, a.TSAToken, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("record anchor: %w", err)
	}
	return nil
}

func (r *EvidenceRepository) Anchors(ctx context.Context, chain string, limit int) ([]model.AuditAnchor, error) {
	return collectAnchors(r.db.Pool.Query(ctx, `SELECT `+anchorColumns+` FROM audit_anchors WHERE chain = $1 ORDER BY seq DESC LIMIT $2`, chain, limit))
}

// ─── Reviews ──────────────────────────────────────────────────────────

const reviewColumns = `id, tenant_id, period_start, period_end, findings, counts, report_sha256, state, COALESCE(signed_by, ''), signed_at,
	note, created_by, created_at`

func collectReviews(rows pgx.Rows, err error) ([]model.AuditReview, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AuditReview{}
	for rows.Next() {
		var v model.AuditReview
		var findings, counts []byte
		if err := rows.Scan(&v.ID, &v.TenantID, &v.PeriodStart, &v.PeriodEnd, &findings, &counts, &v.ReportSHA256, &v.State, &v.SignedBy,
			&v.SignedAt, &v.Note, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(findings, &v.Findings); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(counts, &v.Counts); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *EvidenceRepository) PutReview(ctx context.Context, v model.AuditReview) error {
	findings, err := json.Marshal(v.Findings)
	if err != nil {
		return err
	}
	counts, err := json.Marshal(v.Counts)
	if err != nil {
		return err
	}
	_, err = r.db.Pool.Exec(ctx, `
		INSERT INTO audit_reviews (id, tenant_id, period_start, period_end, findings, counts, report_sha256, state, signed_by, signed_at,
		    note, created_by, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11, $12, $13)
	`, v.ID, v.TenantID, v.PeriodStart, v.PeriodEnd, findings, counts, v.ReportSHA256, v.State, v.SignedBy, v.SignedAt, v.Note, v.CreatedBy, v.CreatedAt)
	if err != nil {
		return fmt.Errorf("store review: %w", err)
	}
	return nil
}

func (r *EvidenceRepository) Reviews(ctx context.Context, tenantID string, limit int) ([]model.AuditReview, error) {
	return collectReviews(r.db.Pool.Query(ctx, `SELECT `+reviewColumns+` FROM audit_reviews WHERE tenant_id = $1 ORDER BY period_end DESC LIMIT $2`,
		tenantID, limit))
}

func (r *EvidenceRepository) Review(ctx context.Context, tenantID, id string) (model.AuditReview, error) {
	out, err := collectReviews(r.db.Pool.Query(ctx, `SELECT `+reviewColumns+` FROM audit_reviews WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if err != nil {
		return model.AuditReview{}, err
	}
	if len(out) == 0 {
		return model.AuditReview{}, model.ErrReviewNotFound
	}
	return out[0], nil
}

// LastReviewEnd is when the organization's newest review period ended.
func (r *EvidenceRepository) LastReviewEnd(ctx context.Context, tenantID string) (*time.Time, error) {
	var end *time.Time
	err := r.db.Pool.QueryRow(ctx, `SELECT max(period_end) FROM audit_reviews WHERE tenant_id = $1`, tenantID).Scan(&end)
	return end, err
}

func (r *EvidenceRepository) SignReview(ctx context.Context, tenantID, id, by, note string, at time.Time) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE audit_reviews SET state = 'SIGNED', signed_by = $3, signed_at = $5, note = $4
		WHERE tenant_id = $1 AND id = $2 AND state = 'OPEN'`, tenantID, id, by, note, at)
	if err != nil {
		return fmt.Errorf("sign review: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrReviewSigned
	}
	return nil
}

// Actions lists a chain's successful entries in a period whose action is
// one of actions.
func (r *EvidenceRepository) Actions(ctx context.Context, chain string, actions []string, from, to time.Time, limit int) ([]model.AuditEvent, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT seq, actor_id, action, resource_type, resource_id, created_at, details
		FROM audit_events WHERE chain = $1 AND action = ANY($2) AND outcome = 'SUCCESS' AND created_at >= $3 AND created_at < $4
		ORDER BY seq LIMIT $5`, chain, actions, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("list review actions: %w", err)
	}
	defer rows.Close()
	var out []model.AuditEvent
	for rows.Next() {
		var e model.AuditEvent
		var details []byte
		if err := rows.Scan(&e.Seq, &e.ActorID, &e.Action, &e.ResourceType, &e.ResourceID, &e.CreatedAt, &details); err != nil {
			return nil, err
		}
		if e.Details, err = decodeDetails(details); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FailedChecks lists, for a period, every place bytes did not check out:
// scrubs and drift findings, runs' verification reports, checkpoints whose
// re-read failed, and uploads rejected for a checksum.
func (r *EvidenceRepository) FailedChecks(ctx context.Context, tenantID string, from, to time.Time, limit int) ([]model.ReviewFinding, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT at, subject, detail FROM (
			SELECT checked_at AS at, asset_id AS subject, 'a scrub found the object ' || COALESCE(NULLIF(detail, ''), 'damaged') AS detail
			FROM scrub_results WHERE tenant_id = $1 AND NOT ok AND checked_at >= $2 AND checked_at < $3
			UNION ALL
			SELECT created_at, run_id, failed || ' of ' || checked || ' files read by ' || host || ' failed their SHA-256'
			FROM run_verifications WHERE tenant_id = $1 AND failed > 0 AND created_at >= $2 AND created_at < $3
			UNION ALL
			SELECT verified_at, run_id, 'checkpoint step ' || step || ': ' || verify_error
			FROM run_checkpoints WHERE tenant_id = $1 AND verify_error IS NOT NULL AND verified_at IS NULL
			  AND created_at >= $2 AND created_at < $3
			UNION ALL
			SELECT updated_at, id, 'upload rejected: ' || rejection_reason
			FROM assets WHERE tenant_id = $1 AND status = 'REJECTED' AND updated_at >= $2 AND updated_at < $3
			  AND (rejection_reason ILIKE '%checksum%' OR rejection_reason ILIKE '%sha%' OR rejection_reason ILIKE '%hash%')
		) x WHERE at IS NOT NULL ORDER BY at LIMIT $4`, tenantID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("list failed checks: %w", err)
	}
	defer rows.Close()
	var out []model.ReviewFinding
	for rows.Next() {
		f := model.ReviewFinding{Kind: "FAILED_HASH"}
		if err := rows.Scan(&f.At, &f.Subject, &f.Detail); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// BulkReads lists members who read at least threshold files in one hour
// outside working hours (in tz, start to end), in a period.
func (r *EvidenceRepository) BulkReads(ctx context.Context, tenantID, tz string, start, end, threshold int, from, to time.Time) ([]model.ReviewFinding, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT hour, principal_id, n FROM (
			SELECT date_trunc('hour', created_at AT TIME ZONE $2) AS hour, principal_id, count(*) AS n
			FROM delivery_audit_events
			WHERE tenant_id = $1 AND outcome = 'ALLOWED' AND created_at >= $5 AND created_at < $6
			GROUP BY 1, 2
		) x
		WHERE (extract(hour FROM hour) < $3 OR extract(hour FROM hour) >= $4) AND n >= $7
		ORDER BY hour LIMIT 500`, tenantID, tz, start, end, from, to, threshold)
	if err != nil {
		return nil, fmt.Errorf("list after-hours reads: %w", err)
	}
	defer rows.Close()
	var out []model.ReviewFinding
	for rows.Next() {
		var hour time.Time
		var who string
		var n int64
		if err := rows.Scan(&hour, &who, &n); err != nil {
			return nil, err
		}
		out = append(out, model.ReviewFinding{Kind: "AFTER_HOURS_READS", At: hour, Actor: who,
			Detail: fmt.Sprintf("%d files read in the hour from %s (%s), outside working hours", n, hour.Format("2006-01-02 15:04"), tz)})
	}
	return out, rows.Err()
}

// ─── Provenance ───────────────────────────────────────────────────────

func (r *EvidenceRepository) Provenance(ctx context.Context, tenantID, datasetID string) (model.Provenance, error) {
	var p model.Provenance
	err := r.db.Pool.QueryRow(ctx, `
		SELECT COALESCE(lawful_basis, ''), COALESCE(license, ''), COALESCE(source, ''), COALESCE(source_kind, ''),
		       provenance_recorded_at, COALESCE(provenance_recorded_by, '')
		FROM datasets WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID).
		Scan(&p.LawfulBasis, &p.License, &p.Source, &p.SourceKind, &p.RecordedAt, &p.RecordedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, model.ErrDatasetNotFound
	}
	return p, err
}

func (r *EvidenceRepository) PutProvenance(ctx context.Context, tenantID, datasetID string, p model.Provenance) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE datasets SET lawful_basis = NULLIF($3, ''), license = NULLIF($4, ''), source = NULLIF($5, ''),
		       source_kind = NULLIF($6, ''), provenance_recorded_at = $7, provenance_recorded_by = $8
		WHERE tenant_id = $1 AND id = $2`, tenantID, datasetID, p.LawfulBasis, p.License, p.Source, p.SourceKind, p.RecordedAt, p.RecordedBy)
	if err != nil {
		return fmt.Errorf("record provenance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrDatasetNotFound
	}
	return nil
}

// VersionOrder is a snapshot, its dataset's provenance, and when each was
// recorded, for a run's evidence of order.
type VersionOrder struct {
	VersionID, DatasetID, DatasetName string
	Version                           int
	CreatedAt                         time.Time
	Provenance                        model.Provenance
}

func (r *EvidenceRepository) VersionOrders(ctx context.Context, tenantID string, versionIDs []string) ([]VersionOrder, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT v.id, v.dataset_id, d.name, v.version, v.created_at, COALESCE(d.lawful_basis, ''), COALESCE(d.license, ''),
		       COALESCE(d.source, ''), COALESCE(d.source_kind, ''), d.provenance_recorded_at, COALESCE(d.provenance_recorded_by, '')
		FROM dataset_versions v JOIN datasets d ON d.id = v.dataset_id
		WHERE v.tenant_id = $1 AND v.id = ANY($2) ORDER BY v.created_at`, tenantID, versionIDs)
	if err != nil {
		return nil, fmt.Errorf("read version provenance: %w", err)
	}
	defer rows.Close()
	var out []VersionOrder
	for rows.Next() {
		var o VersionOrder
		p := &o.Provenance
		if err := rows.Scan(&o.VersionID, &o.DatasetID, &o.DatasetName, &o.Version, &o.CreatedAt, &p.LawfulBasis, &p.License,
			&p.Source, &p.SourceKind, &p.RecordedAt, &p.RecordedBy); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RunVersions lists the snapshots a training run read, and when it was
// recorded.
func (r *EvidenceRepository) RunVersions(ctx context.Context, tenantID, runID string) ([]string, time.Time, error) {
	var created time.Time
	if err := r.db.Pool.QueryRow(ctx, `SELECT created_at FROM training_runs WHERE tenant_id = $1 AND id = $2`, tenantID, runID).Scan(&created); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, created, model.ErrTrainingRunNotFound
		}
		return nil, created, err
	}
	rows, err := r.db.Pool.Query(ctx, `SELECT version_id FROM training_run_inputs WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID)
	if err != nil {
		return nil, created, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return ids, created, err
}

// AuditSeqOf is the chain position of the first successful entry of
// action on resourceID, 0 when there is none.
func (r *EvidenceRepository) AuditSeqOf(ctx context.Context, chain, action, resourceID string) (int64, error) {
	var seq *int64
	err := r.db.Pool.QueryRow(ctx, `SELECT min(seq) FROM audit_events WHERE chain = $1 AND action = $2 AND resource_id = $3 AND outcome = 'SUCCESS'`,
		chain, action, resourceID).Scan(&seq)
	if err != nil || seq == nil {
		return 0, err
	}
	return *seq, nil
}
