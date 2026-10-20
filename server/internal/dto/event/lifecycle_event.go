package event

import (
	"errors"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// TypeDeletionScheduled is written with the soft delete that schedules an
// asset's deletion. The lifecycle consumer runs the deletion at once when
// it is already due (a zero grace period); otherwise the deletion schedule
// runs it at its purge time.
const TypeDeletionScheduled = "asset.deletion_scheduled"

// TypeHoldReleased is written when a legal hold is released. The lifecycle
// consumer makes the deletions the hold was protecting due again now
// instead of at their next recheck.
const TypeHoldReleased = "legal_hold.released"

// DeletionScheduled is the payload of asset.deletion_scheduled, version 1.
type DeletionScheduled struct {
	OperationID string `json:"operation_id"`
}

func (p *DeletionScheduled) validate() error {
	if p.OperationID == "" {
		return errors.New("operation_id is required")
	}
	return nil
}

// HoldReleased is the payload of legal_hold.released, version 1.
type HoldReleased struct {
	HoldID string `json:"hold_id"`
}

func (p *HoldReleased) validate() error {
	if p.HoldID == "" {
		return errors.New("hold_id is required")
	}
	return nil
}

// NewDeletionScheduled builds the event for a newly scheduled deletion.
// Events are ordered per asset, so a late event about an older deletion is
// recognised as stale.
func NewDeletionScheduled(tenantID, assetID, operationID string, at time.Time, correlationID string) model.OutboxEvent {
	return newEvent(TypeDeletionScheduled, tenantID, AggregateAsset, assetID,
		&DeletionScheduled{OperationID: operationID}, at, correlationID)
}

// NewHoldReleased builds the event for a released legal hold.
func NewHoldReleased(tenantID, holdID string, at time.Time, correlationID string) model.OutboxEvent {
	return newEvent(TypeHoldReleased, tenantID, AggregateLegalHold, holdID,
		&HoldReleased{HoldID: holdID}, at, correlationID)
}
