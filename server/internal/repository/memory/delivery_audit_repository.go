package memory

import (
	"context"
	"sync"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DeliveryAuditRepository keeps audit events in memory. For tests only.
type DeliveryAuditRepository struct {
	mu     sync.Mutex
	events []model.DeliveryAuditEvent
	// Err, when set, fails every Record call.
	Err error
}

var _ interfaces.DeliveryAuditRepository = (*DeliveryAuditRepository)(nil)

func NewDeliveryAuditRepository() *DeliveryAuditRepository {
	return &DeliveryAuditRepository{}
}

func (r *DeliveryAuditRepository) Record(ctx context.Context, event model.DeliveryAuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return r.Err
	}
	r.events = append(r.events, event)
	return nil
}

// Events returns a snapshot, for assertions.
func (r *DeliveryAuditRepository) Events() []model.DeliveryAuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.DeliveryAuditEvent(nil), r.events...)
}
