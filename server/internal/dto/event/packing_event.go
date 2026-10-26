package event

import (
	"errors"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// QueuePacking carries packing work to the pack worker.
const QueuePacking = "packing"

// AggregatePack is a pack and the job that builds it.
const AggregatePack = "pack"

// TypePackPlanned is written with a new pack job. The pack worker builds
// the pack; its poll picks up a job whose event was lost.
const TypePackPlanned = "pack.planned"

// PackPlanned is the payload of pack.planned, version 1.
type PackPlanned struct {
	PackID string `json:"pack_id"`
}

func (p *PackPlanned) validate() error {
	if p.PackID == "" {
		return errors.New("pack_id is required")
	}
	return nil
}

// NewPackPlanned builds the event that starts building a pack.
func NewPackPlanned(tenantID, packID string, at time.Time) model.OutboxEvent {
	return newEvent(TypePackPlanned, tenantID, AggregatePack, packID, &PackPlanned{PackID: packID}, at, "")
}
