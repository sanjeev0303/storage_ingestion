package event

import (
	"errors"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// QueueIngestion carries bulk-ingestion work to the upload worker.
const QueueIngestion = "ingestion"

// AggregateIngestionJob: a bulk ingestion job.
const AggregateIngestionJob = "ingestion_job"

// TypeManifestStaged is written when a client submits a manifest it
// uploaded as an object. The ingestion consumer reads the manifest in
// bounded batches from its checkpoint; the worker's poll resumes a read
// that stops.
const TypeManifestStaged = "ingestion.manifest_staged"

// TypeUploadProcessed is written in the transaction that finishes the
// processing job of an upload that belongs to a bulk ingestion entry.
// The ingestion consumer records the entry's outcome from the asset's
// state, not from the event.
const TypeUploadProcessed = "upload.processed"

// ManifestStaged is the payload of ingestion.manifest_staged, version 1.
type ManifestStaged struct {
	JobID string `json:"job_id"`
}

func (p *ManifestStaged) validate() error {
	if p.JobID == "" {
		return errors.New("job_id is required")
	}
	return nil
}

// UploadProcessed is the payload of upload.processed, version 1.
type UploadProcessed struct {
	UploadID string `json:"upload_id"`
}

func (p *UploadProcessed) validate() error {
	if p.UploadID == "" {
		return errors.New("upload_id is required")
	}
	return nil
}

// NewManifestStaged builds the event for a submitted staged manifest.
func NewManifestStaged(tenantID, jobID string, at time.Time, correlationID string) model.OutboxEvent {
	return newEvent(TypeManifestStaged, tenantID, AggregateIngestionJob, jobID,
		&ManifestStaged{JobID: jobID}, at, correlationID)
}

// NewUploadProcessed builds the event for an upload whose processing ended.
func NewUploadProcessed(tenantID, uploadID string, at time.Time, correlationID string) model.OutboxEvent {
	return newEvent(TypeUploadProcessed, tenantID, AggregateUpload, uploadID,
		&UploadProcessed{UploadID: uploadID}, at, correlationID)
}
