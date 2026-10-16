package postgres

import (
	"context"
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DeliveryAuditRepository appends delivery decisions to Neon PostgreSQL.
type DeliveryAuditRepository struct {
	db *DB
}

var _ interfaces.DeliveryAuditRepository = (*DeliveryAuditRepository)(nil)

func NewDeliveryAuditRepository(db *DB) *DeliveryAuditRepository {
	return &DeliveryAuditRepository{db: db}
}

func (r *DeliveryAuditRepository) Record(ctx context.Context, event model.DeliveryAuditEvent) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO delivery_audit_events
			(id, tenant_id, principal_id, asset_id, operation, outcome, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)
	`, event.ID, event.TenantID, event.PrincipalID, event.AssetID,
		event.Operation, event.Outcome, event.Reason, event.CreatedAt)
	if err != nil {
		return fmt.Errorf("record delivery audit event: %w", err)
	}
	return nil
}
