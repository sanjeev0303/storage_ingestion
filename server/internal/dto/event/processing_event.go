package event

import (
	"errors"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// TypeUploadCompleted is written in the same transaction that moves an
// upload to COMPLETED. The processing consumer creates the upload's asset
// and processing job and starts the job.
const TypeUploadCompleted = "upload.completed"

// UploadCompleted is the payload of upload.completed, version 1.
type UploadCompleted struct {
	UploadID string `json:"upload_id"`
}

func (p *UploadCompleted) validate() error {
	if p.UploadID == "" {
		return errors.New("upload_id is required")
	}
	return nil
}

// NewUploadCompleted builds the event for an upload that just completed.
func NewUploadCompleted(tenantID, uploadID string, at time.Time, correlationID string) model.OutboxEvent {
	return newEvent(TypeUploadCompleted, tenantID, AggregateUpload, uploadID,
		&UploadCompleted{UploadID: uploadID}, at, correlationID)
}
