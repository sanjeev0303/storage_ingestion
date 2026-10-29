package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// WebhookRepository stores webhook endpoints and their deliveries.
// Deliveries of platform events are written by the triggers of migration
// 000021, inside the transactions that make the changes.
type WebhookRepository struct {
	db *DB
}

func NewWebhookRepository(db *DB) *WebhookRepository {
	return &WebhookRepository{db: db}
}

const webhookColumns = `id, tenant_id, url, description, event_types, enabled,
	secret_key_version, secret_nonce, secret_ciphertext, created_by, created_at, updated_at`

func scanWebhook(row pgx.Row) (model.WebhookEndpoint, error) {
	var e model.WebhookEndpoint
	err := row.Scan(&e.ID, &e.TenantID, &e.URL, &e.Description, &e.EventTypes, &e.Enabled,
		&e.SecretKeyVersion, &e.SecretNonce, &e.SecretCipher, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, model.ErrWebhookNotFound
	}
	return e, err
}

func (r *WebhookRepository) Create(ctx context.Context, e model.WebhookEndpoint) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO webhook_endpoints (`+webhookColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		e.ID, e.TenantID, e.URL, e.Description, e.EventTypes, e.Enabled,
		e.SecretKeyVersion, e.SecretNonce, e.SecretCipher, e.CreatedBy, e.CreatedAt, e.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create webhook endpoint: %w", err)
	}
	return nil
}

func (r *WebhookRepository) Get(ctx context.Context, tenantID, id string) (model.WebhookEndpoint, error) {
	return scanWebhook(r.db.Pool.QueryRow(ctx,
		`SELECT `+webhookColumns+` FROM webhook_endpoints WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

func (r *WebhookRepository) List(ctx context.Context, tenantID string) ([]model.WebhookEndpoint, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT `+webhookColumns+` FROM webhook_endpoints WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list webhook endpoints: %w", err)
	}
	return collect(rows, scanWebhook)
}

// Update replaces the endpoint's settings and sealed secret.
func (r *WebhookRepository) Update(ctx context.Context, e model.WebhookEndpoint) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE webhook_endpoints
		SET url = $3, description = $4, event_types = $5, enabled = $6,
		    secret_key_version = $7, secret_nonce = $8, secret_ciphertext = $9, updated_at = $10
		WHERE tenant_id = $1 AND id = $2`,
		e.TenantID, e.ID, e.URL, e.Description, e.EventTypes, e.Enabled,
		e.SecretKeyVersion, e.SecretNonce, e.SecretCipher, e.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update webhook endpoint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrWebhookNotFound
	}
	return nil
}

// Delete removes the endpoint and, by cascade, its deliveries.
func (r *WebhookRepository) Delete(ctx context.Context, tenantID, id string) error {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM webhook_endpoints WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete webhook endpoint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrWebhookNotFound
	}
	return nil
}

// Enqueue writes one delivery to one endpoint (the test event).
func (r *WebhookRepository) Enqueue(ctx context.Context, d model.WebhookDelivery) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (id, event_id, endpoint_id, tenant_id, event_type, payload, status, next_attempt_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'PENDING', $7, $7)`,
		d.ID, d.EventID, d.EndpointID, d.TenantID, d.EventType, d.Payload, d.CreatedAt)
	if err != nil {
		return fmt.Errorf("queue webhook delivery: %w", err)
	}
	_, _ = r.db.Pool.Exec(ctx, `SELECT pg_notify('webhook_deliveries', '')`)
	return nil
}

const deliveryColumns = `id, event_id, endpoint_id, tenant_id, event_type, payload, status, attempts, next_attempt_at,
	COALESCE(last_status_code, 0), COALESCE(last_error, ''), last_attempt_at, delivered_at, created_at`

func scanDelivery(row pgx.Row) (model.WebhookDelivery, error) {
	var d model.WebhookDelivery
	err := row.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.TenantID, &d.EventType, &d.Payload, &d.Status, &d.Attempts,
		&d.NextAttemptAt, &d.LastStatusCode, &d.LastError, &d.LastAttemptAt, &d.DeliveredAt, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, model.ErrDeliveryNotFound
	}
	return d, err
}

// Deliveries lists an endpoint's deliveries, newest first. before is the
// cursor of the last one of the previous page ("" for the first page).
func (r *WebhookRepository) Deliveries(ctx context.Context, tenantID, endpointID, status, before string, limit int) ([]model.WebhookDelivery, string, error) {
	at, id := time.Now().Add(time.Hour), ""
	if before != "" {
		var err error
		if at, id, err = decodeTimeCursor(before); err != nil {
			return nil, "", err
		}
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT `+deliveryColumns+` FROM webhook_deliveries
		WHERE tenant_id = $1 AND endpoint_id = $2 AND ($3 = '' OR status = $3)
		  AND (created_at, id) < ($4, $5)
		ORDER BY created_at DESC, id DESC
		LIMIT $6`, tenantID, endpointID, status, at, id, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("list webhook deliveries: %w", err)
	}
	page, err := collect(rows, scanDelivery)
	if err != nil || len(page) <= limit {
		return page, "", err
	}
	page = page[:limit]
	last := page[limit-1]
	return page, encodeTimeCursor(last.CreatedAt, last.ID), nil
}

// Redeliver sends a delivery again from a fresh attempt count.
func (r *WebhookRepository) Redeliver(ctx context.Context, tenantID, endpointID, deliveryID string, now time.Time) (model.WebhookDelivery, error) {
	d, err := scanDelivery(r.db.Pool.QueryRow(ctx, `
		UPDATE webhook_deliveries
		SET status = 'PENDING', attempts = 0, next_attempt_at = $4, lease_owner = NULL, lease_expires_at = NULL
		WHERE tenant_id = $1 AND endpoint_id = $2 AND id = $3
		RETURNING `+deliveryColumns, tenantID, endpointID, deliveryID, now))
	if err == nil {
		_, _ = r.db.Pool.Exec(ctx, `SELECT pg_notify('webhook_deliveries', '')`)
	}
	return d, err
}

// Claim leases up to limit due deliveries of enabled endpoints. A lease
// that ran out (a worker that stopped mid-send) makes its delivery due
// again.
func (r *WebhookRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]model.DueDelivery, error) {
	rows, err := r.db.Pool.Query(ctx, `
		WITH due AS (
		    SELECT d.id FROM webhook_deliveries d JOIN webhook_endpoints e ON e.id = d.endpoint_id
		    WHERE d.status = 'PENDING' AND d.next_attempt_at <= $2 AND e.enabled
		      AND (d.lease_expires_at IS NULL OR d.lease_expires_at < $2)
		    ORDER BY d.next_attempt_at
		    LIMIT $4
		    FOR UPDATE OF d SKIP LOCKED
		)
		UPDATE webhook_deliveries d SET lease_owner = $1, lease_expires_at = $3
		FROM due WHERE d.id = due.id
		RETURNING d.id`, owner, now, now.Add(lease), limit)
	if err != nil {
		return nil, fmt.Errorf("claim webhook deliveries: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	rows, err = r.db.Pool.Query(ctx, `
		SELECT d.id, d.event_id, d.endpoint_id, d.tenant_id, d.event_type, d.payload, d.status, d.attempts, d.next_attempt_at,
		       COALESCE(d.last_status_code, 0), COALESCE(d.last_error, ''), d.last_attempt_at, d.delivered_at, d.created_at,
		       e.id, e.tenant_id, e.url, e.description, e.event_types, e.enabled,
		       e.secret_key_version, e.secret_nonce, e.secret_ciphertext, e.created_by, e.created_at, e.updated_at
		FROM webhook_deliveries d JOIN webhook_endpoints e ON e.id = d.endpoint_id
		WHERE d.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read claimed webhook deliveries: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.DueDelivery, error) {
		var x model.DueDelivery
		d, e := &x.WebhookDelivery, &x.Endpoint
		err := row.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.TenantID, &d.EventType, &d.Payload, &d.Status, &d.Attempts,
			&d.NextAttemptAt, &d.LastStatusCode, &d.LastError, &d.LastAttemptAt, &d.DeliveredAt, &d.CreatedAt,
			&e.ID, &e.TenantID, &e.URL, &e.Description, &e.EventTypes, &e.Enabled,
			&e.SecretKeyVersion, &e.SecretNonce, &e.SecretCipher, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt)
		return x, err
	})
}

// Record stores one attempt's result under the caller's lease. next is
// when to try again; nil means delivered (ok) or given up.
func (r *WebhookRepository) Record(ctx context.Context, id, owner string, now time.Time, ok bool, statusCode int, lastError string, next *time.Time) error {
	status := model.HookFailed
	switch {
	case ok:
		status = model.HookDelivered
	case next != nil:
		status = model.HookPending
	}
	nextAt := now
	if next != nil {
		nextAt = *next
	}
	var delivered *time.Time
	if ok {
		delivered = &now
	}
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = $3, attempts = attempts + 1, next_attempt_at = $4, last_status_code = NULLIF($5, 0),
		    last_error = NULLIF($6, ''), last_attempt_at = $7, delivered_at = COALESCE($8, delivered_at),
		    lease_owner = NULL, lease_expires_at = NULL
		WHERE id = $1 AND lease_owner = $2`,
		id, owner, status, nextAt, statusCode, lastError, now, delivered)
	if err != nil {
		return fmt.Errorf("record webhook delivery: %w", err)
	}
	return nil
}
