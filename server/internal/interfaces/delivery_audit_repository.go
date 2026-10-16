package interfaces

import (
	"context"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// DeliveryAuditRepository appends delivery decisions. It is append-only.
type DeliveryAuditRepository interface {
	Record(ctx context.Context, event model.DeliveryAuditEvent) error
}
