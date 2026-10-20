package event

import (
	"errors"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// QueueDatasets carries managed-dataset work to the dataset worker.
const QueueDatasets = "datasets"

// Aggregates of dataset work.
const (
	AggregateDatasetImport = "dataset_import"
	AggregateValidationJob = "validation_job"
)

// TypeImportQueued is written when a manifest import is ready to read:
// with an inline manifest, or once a staged one is uploaded. The dataset
// worker reads it from its checkpoint; the worker's poll resumes an
// import that stops.
const TypeImportQueued = "dataset.import_queued"

// TypeValidationQueued is written with a new validation job. The dataset
// worker runs it from its checkpoint; the poll recovers one that stops.
const TypeValidationQueued = "dataset.validation_queued"

// ImportQueued is the payload of dataset.import_queued, version 1.
type ImportQueued struct {
	ImportID string `json:"import_id"`
}

func (p *ImportQueued) validate() error {
	if p.ImportID == "" {
		return errors.New("import_id is required")
	}
	return nil
}

// ValidationQueued is the payload of dataset.validation_queued, version 1.
type ValidationQueued struct {
	JobID string `json:"job_id"`
}

func (p *ValidationQueued) validate() error {
	if p.JobID == "" {
		return errors.New("job_id is required")
	}
	return nil
}

// NewImportQueued builds the event that starts reading an import.
func NewImportQueued(tenantID, importID string, at time.Time, correlationID string) model.OutboxEvent {
	return newEvent(TypeImportQueued, tenantID, AggregateDatasetImport, importID, &ImportQueued{ImportID: importID}, at, correlationID)
}

// NewValidationQueued builds the event that starts a validation job.
func NewValidationQueued(tenantID, jobID string, at time.Time, correlationID string) model.OutboxEvent {
	return newEvent(TypeValidationQueued, tenantID, AggregateValidationJob, jobID, &ValidationQueued{JobID: jobID}, at, correlationID)
}
