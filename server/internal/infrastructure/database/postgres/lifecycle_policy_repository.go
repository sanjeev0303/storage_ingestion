package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// RetentionPolicyRepository stores append-only retention policy versions.
type RetentionPolicyRepository struct {
	db *DB
}

var _ interfaces.RetentionPolicyRepository = (*RetentionPolicyRepository)(nil)

func NewRetentionPolicyRepository(db *DB) *RetentionPolicyRepository {
	return &RetentionPolicyRepository{db: db}
}

const policyColumns = `
	id, tenant_id, scope, scope_key, version,
	min_retention_seconds, max_age_seconds, deletion_grace_seconds, quarantine_retention_seconds,
	effective_from, created_by, reason, created_at,
	COALESCE(lock_mode, ''), COALESCE(retain_from_key, ''), COALESCE(template, '')
`

func scanPolicy(row pgx.Row) (model.RetentionPolicy, error) {
	var p model.RetentionPolicy
	var minRetention, maxAge, grace, quarantine *int64
	err := row.Scan(&p.ID, &p.TenantID, &p.Scope, &p.ScopeKey, &p.Version,
		&minRetention, &maxAge, &grace, &quarantine,
		&p.EffectiveFrom, &p.CreatedBy, &p.Reason, &p.CreatedAt,
		&p.LockMode, &p.RetainFromKey, &p.Template)
	p.MinRetention = fromSeconds(minRetention)
	p.MaxAge = fromSeconds(maxAge)
	p.DeletionGrace = fromSeconds(grace)
	p.QuarantineRetention = fromSeconds(quarantine)
	return p, err
}

func fromSeconds(seconds *int64) *time.Duration {
	if seconds == nil {
		return nil
	}
	d := time.Duration(*seconds) * time.Second
	return &d
}

func toSeconds(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	seconds := int64(*d / time.Second)
	return &seconds
}

// AddVersion numbers the new version one past the scope's latest. Two
// writers racing for the same number collide on the unique index; the
// loser simply takes the next number.
func (r *RetentionPolicyRepository) AddVersion(ctx context.Context, policy model.RetentionPolicy) (model.RetentionPolicy, error) {
	for attempt := 0; ; attempt++ {
		stored, err := scanPolicy(r.db.Pool.QueryRow(ctx, `
			INSERT INTO retention_policies (
				id, tenant_id, scope, scope_key, version,
				min_retention_seconds, max_age_seconds, deletion_grace_seconds, quarantine_retention_seconds,
				effective_from, created_by, reason, created_at, lock_mode, retain_from_key, template
			)
			SELECT $1, $2, $3, $4, COALESCE(MAX(version), 0) + 1, $5, $6, $7, $8, $9, $10, $11, $12,
			       NULLIF($13, ''), NULLIF($14, ''), NULLIF($15, '')
			FROM retention_policies
			WHERE tenant_id = $2 AND scope = $3 AND scope_key = $4
			RETURNING `+policyColumns,
			policy.ID, policy.TenantID, policy.Scope, policy.ScopeKey,
			toSeconds(policy.MinRetention), toSeconds(policy.MaxAge), toSeconds(policy.DeletionGrace), toSeconds(policy.QuarantineRetention),
			policy.EffectiveFrom, policy.CreatedBy, policy.Reason, policy.CreatedAt,
			policy.LockMode, policy.RetainFromKey, policy.Template))

		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && attempt < 3 {
			continue
		}
		if err != nil {
			return model.RetentionPolicy{}, fmt.Errorf("add retention policy version: %w", err)
		}
		return stored, nil
	}
}

func (r *RetentionPolicyRepository) Effective(ctx context.Context, tenantID, scope, scopeKey string, asOf time.Time) (model.RetentionPolicy, bool, error) {
	policy, err := scanPolicy(r.db.Pool.QueryRow(ctx, `
		SELECT `+policyColumns+`
		FROM retention_policies
		WHERE tenant_id = $1 AND scope = $2 AND scope_key = $3 AND effective_from <= $4
		ORDER BY effective_from DESC, version DESC
		LIMIT 1
	`, tenantID, scope, scopeKey, asOf))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.RetentionPolicy{}, false, nil
	}
	if err != nil {
		return model.RetentionPolicy{}, false, fmt.Errorf("get effective retention policy: %w", err)
	}
	return policy, true, nil
}

func (r *RetentionPolicyRepository) History(ctx context.Context, tenantID, scope, scopeKey string) ([]model.RetentionPolicy, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+policyColumns+`
		FROM retention_policies
		WHERE tenant_id = $1 AND scope = $2 AND scope_key = $3
		ORDER BY version DESC
	`, tenantID, scope, scopeKey)
	if err != nil {
		return nil, fmt.Errorf("list retention policy versions: %w", err)
	}
	defer rows.Close()

	var policies []model.RetentionPolicy
	for rows.Next() {
		policy, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan retention policy: %w", err)
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func (r *RetentionPolicyRepository) Latest(ctx context.Context, tenantID string) ([]model.RetentionPolicy, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT ON (scope, scope_key) `+policyColumns+`
		FROM retention_policies
		WHERE tenant_id = $1
		ORDER BY scope, scope_key, version DESC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list retention policies: %w", err)
	}
	defer rows.Close()

	var policies []model.RetentionPolicy
	for rows.Next() {
		policy, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan retention policy: %w", err)
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

// LegalHoldRepository stores legal holds.
type LegalHoldRepository struct {
	db *DB
}

var _ interfaces.LegalHoldRepository = (*LegalHoldRepository)(nil)

func NewLegalHoldRepository(db *DB) *LegalHoldRepository {
	return &LegalHoldRepository{db: db}
}

const holdColumns = `
	id, tenant_id, scope, scope_key, reason, placed_by, placed_at,
	COALESCE(released_by, ''), released_at, query, matter, custodians
`

func scanHold(row pgx.Row) (model.LegalHold, error) {
	var h model.LegalHold
	err := row.Scan(&h.ID, &h.TenantID, &h.Scope, &h.ScopeKey, &h.Reason, &h.PlacedBy, &h.PlacedAt, &h.ReleasedBy, &h.ReleasedAt,
		&h.Query, &h.Matter, &h.Custodians)
	return h, err
}

// matchesQuery is true when the asset $assetParam matches the hold row's
// query. It is evaluated whenever the asset is checked, so a QUERY hold
// covers assets created after it too.
func matchesQuery(holds, assetParam string) string {
	return `EXISTS (
		SELECT 1 FROM assets AS qa
		LEFT JOIN catalog_objects AS qo ON qo.id = qa.object_id AND qo.tenant_id = qa.tenant_id
		WHERE qa.tenant_id = ` + holds + `.tenant_id AND qa.id = ` + assetParam + `
		  AND (NOT ` + holds + `.query ? 'dataset' OR qa.dataset = ` + holds + `.query->>'dataset')
		  AND (NOT ` + holds + `.query ? 'path_prefix' OR starts_with(COALESCE(qo.path, qa.file_name), ` + holds + `.query->>'path_prefix'))
		  AND (NOT ` + holds + `.query ? 'content_type_prefix' OR starts_with(qa.content_type, ` + holds + `.query->>'content_type_prefix'))
		  AND (NOT ` + holds + `.query ? 'metadata' OR COALESCE(qa.metadata, '{}'::jsonb) @> (` + holds + `.query->'metadata'))
		  AND (NOT ` + holds + `.query ? 'created_after' OR qa.created_at >= (` + holds + `.query->>'created_after')::timestamptz)
		  AND (NOT ` + holds + `.query ? 'created_before' OR qa.created_at < (` + holds + `.query->>'created_before')::timestamptz))`
}

func (r *LegalHoldRepository) Place(ctx context.Context, hold model.LegalHold) (model.LegalHold, error) {
	var query []byte
	if hold.Query != nil {
		query = hold.Query.JSON()
	}
	custodians := hold.Custodians
	if custodians == nil {
		custodians = []string{}
	}
	stored, err := scanHold(r.db.Pool.QueryRow(ctx, `
		INSERT INTO legal_holds (id, tenant_id, scope, scope_key, reason, placed_by, placed_at, query, matter, custodians)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+holdColumns,
		hold.ID, hold.TenantID, hold.Scope, hold.ScopeKey, hold.Reason, hold.PlacedBy, hold.PlacedAt, query, hold.Matter, custodians))
	if err != nil {
		return model.LegalHold{}, fmt.Errorf("place legal hold: %w", err)
	}
	return stored, nil
}

func (r *LegalHoldRepository) Release(ctx context.Context, tenantID, holdID, releasedBy string, at time.Time, outbox ...model.OutboxEvent) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin hold release: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, `
		UPDATE legal_holds
		SET released_by = $3, released_at = $4
		WHERE tenant_id = $1 AND id = $2 AND released_at IS NULL
	`, tenantID, holdID, releasedBy, at)
	if err != nil {
		return fmt.Errorf("release legal hold: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrHoldNotFound
	}
	if err := insertOutbox(ctx, tx, outbox...); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit hold release: %w", err)
	}
	return nil
}

func (r *LegalHoldRepository) Active(ctx context.Context, tenantID, dataset, assetID string) ([]model.LegalHold, error) {
	return r.query(ctx, `
		SELECT `+holdColumns+`
		FROM legal_holds
		WHERE tenant_id = $1
		  AND released_at IS NULL
		  AND (scope = 'TENANT'
		       OR (scope = 'DATASET' AND $2 <> '' AND scope_key = $2)
		       OR (scope = 'ASSET' AND $3 <> '' AND scope_key = $3)
		       OR (scope = 'QUERY' AND $3 <> '' AND `+matchesQuery("legal_holds", "$3")+`))
		ORDER BY placed_at, id
	`, tenantID, dataset, assetID)
}

func (r *LegalHoldRepository) List(ctx context.Context, tenantID string, includeReleased bool) ([]model.LegalHold, error) {
	return r.query(ctx, `
		SELECT `+holdColumns+`
		FROM legal_holds
		WHERE tenant_id = $1 AND ($2 OR released_at IS NULL)
		ORDER BY placed_at DESC, id
	`, tenantID, includeReleased)
}

func (r *LegalHoldRepository) query(ctx context.Context, sql string, args ...any) ([]model.LegalHold, error) {
	rows, err := r.db.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query legal holds: %w", err)
	}
	defer rows.Close()

	var holds []model.LegalHold
	for rows.Next() {
		hold, err := scanHold(rows)
		if err != nil {
			return nil, fmt.Errorf("scan legal hold: %w", err)
		}
		holds = append(holds, hold)
	}
	return holds, rows.Err()
}
