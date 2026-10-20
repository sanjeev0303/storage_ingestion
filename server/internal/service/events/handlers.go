package events

import (
	"context"
	"errors"
	"fmt"

	"github.com/sanjeev0303/object-storage-platform/internal/dto/event"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/platform/retry"
)

// work is a service call that takes a tenant and an identifier from the
// event's payload.
type work func(ctx context.Context, tenantID, id string) (string, error)

// ProcessingHandlers are the processing queue's handlers. intake is the
// processing service's Intake.
func ProcessingHandlers(intake work) map[string]Handler {
	return map[string]Handler{
		event.TypeUploadCompleted: func(ctx context.Context, env event.Envelope) (string, error) {
			var p event.UploadCompleted
			if err := event.DecodePayload(env, &p); err != nil {
				return "", retry.Permanent(err)
			}
			return intake(ctx, env.TenantID, p.UploadID)
		},
	}
}

// LifecycleHandlers are the lifecycle queue's handlers: the asset
// service's RunDeletion and ResumeAfterHold.
func LifecycleHandlers(runDeletion, resumeAfterHold work) map[string]Handler {
	return map[string]Handler{
		event.TypeDeletionScheduled: func(ctx context.Context, env event.Envelope) (string, error) {
			var p event.DeletionScheduled
			if err := event.DecodePayload(env, &p); err != nil {
				return "", retry.Permanent(err)
			}
			return runDeletion(ctx, env.TenantID, p.OperationID)
		},
		event.TypeHoldReleased: func(ctx context.Context, env event.Envelope) (string, error) {
			var p event.HoldReleased
			if err := event.DecodePayload(env, &p); err != nil {
				return "", retry.Permanent(err)
			}
			return resumeAfterHold(ctx, env.TenantID, p.HoldID)
		},
	}
}

// CatalogHandlers are the catalog's handlers on the lifecycle queue:
// retireReplaced deletes a version a newer one replaced on an object
// whose versioning is disabled (catalog.Service.RetireReplaced).
func CatalogHandlers(retireReplaced func(ctx context.Context, tenantID, objectID, assetID string) (string, error)) map[string]Handler {
	return map[string]Handler{
		event.TypeVersionReplaced: func(ctx context.Context, env event.Envelope) (string, error) {
			var p event.VersionReplaced
			if err := event.DecodePayload(env, &p); err != nil {
				return "", retry.Permanent(err)
			}
			return retireReplaced(ctx, env.TenantID, p.ObjectID, p.AssetID)
		},
	}
}

// IngestionHandlers are the ingestion queue's handlers: reading a staged
// manifest, and recording the processing outcome of an entry's upload.
func IngestionHandlers(readManifest, uploadProcessed work) map[string]Handler {
	return map[string]Handler{
		event.TypeManifestStaged: func(ctx context.Context, env event.Envelope) (string, error) {
			var p event.ManifestStaged
			if err := event.DecodePayload(env, &p); err != nil {
				return "", retry.Permanent(err)
			}
			return readManifest(ctx, env.TenantID, p.JobID)
		},
		event.TypeUploadProcessed: func(ctx context.Context, env event.Envelope) (string, error) {
			var p event.UploadProcessed
			if err := event.DecodePayload(env, &p); err != nil {
				return "", retry.Permanent(err)
			}
			return uploadProcessed(ctx, env.TenantID, p.UploadID)
		},
	}
}

// DatasetHandlers are the datasets queue's handlers: reading a queued
// manifest import and running a validation job (the dataset service's
// RunImport and RunValidation).
func DatasetHandlers(runImport, runValidation work) map[string]Handler {
	return map[string]Handler{
		event.TypeImportQueued: func(ctx context.Context, env event.Envelope) (string, error) {
			var p event.ImportQueued
			if err := event.DecodePayload(env, &p); err != nil {
				return "", retry.Permanent(err)
			}
			return runImport(ctx, env.TenantID, p.ImportID)
		},
		event.TypeValidationQueued: func(ctx context.Context, env event.Envelope) (string, error) {
			var p event.ValidationQueued
			if err := event.DecodePayload(env, &p); err != nil {
				return "", retry.Permanent(err)
			}
			return runValidation(ctx, env.TenantID, p.JobID)
		},
	}
}

// Checks tell whether the business state still calls for an event, before
// it is sent again by an operator or found in the dead-letter table. They
// only read; each handler checks again when it runs.
type Checks struct {
	Uploads   interfaces.UploadRepository
	Assets    interfaces.AssetRepository
	Jobs      interfaces.ProcessingJobRepository
	Deletions interfaces.DeletionRepository
	Holds     interfaces.LegalHoldRepository
	// Ingestion, when set, judges ingestion events; without it they are
	// taken as still needed (their handlers check again anyway).
	Ingestion interfaces.IngestionRepository
	// Datasets, when set, judges dataset events, the same way.
	Datasets interfaces.DatasetRepository
}

// Stale returns why the event no longer applies, or "" when it still does.
func (c Checks) Stale(ctx context.Context, env event.Envelope) (string, error) {
	switch env.Type {
	case event.TypeUploadCompleted:
		var p event.UploadCompleted
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		return c.uploadCompleted(ctx, env.TenantID, p.UploadID)

	case event.TypeDeletionScheduled:
		var p event.DeletionScheduled
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		op, err := c.Deletions.Get(ctx, env.TenantID, p.OperationID)
		if errors.Is(err, model.ErrDeletionNotFound) {
			return "the deletion no longer exists", nil
		}
		if err != nil {
			return "", err
		}
		if !op.State.IsLive() {
			return "the deletion is " + string(op.State), nil
		}
		return "", nil

	case event.TypeHoldReleased:
		var p event.HoldReleased
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		holds, err := c.Holds.List(ctx, env.TenantID, true)
		if err != nil {
			return "", err
		}
		for _, h := range holds {
			if h.ID == p.HoldID && h.ReleasedAt != nil {
				return "", nil
			}
		}
		return "the hold is not released", nil

	case event.TypeVersionReplaced:
		var p event.VersionReplaced
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		asset, err := c.Assets.GetByID(ctx, env.TenantID, p.AssetID)
		if errors.Is(err, model.ErrAssetNotFound) {
			return "the version no longer exists", nil
		}
		if err != nil {
			return "", err
		}
		if asset.Status != model.AssetStatusAvailable {
			return "the version is " + string(asset.Status), nil
		}
		return "", nil

	case event.TypeManifestStaged:
		var p event.ManifestStaged
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		if c.Ingestion == nil {
			return "", nil
		}
		job, err := c.Ingestion.GetJob(ctx, env.TenantID, p.JobID)
		if errors.Is(err, model.ErrIngestionNotFound) {
			return "the ingestion job no longer exists", nil
		}
		if err != nil {
			return "", err
		}
		if job.State != model.JobValidating || job.ManifestComplete {
			return "the manifest is no longer being read (job " + string(job.State) + ")", nil
		}
		return "", nil

	case event.TypeUploadProcessed:
		var p event.UploadProcessed
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		if c.Ingestion == nil {
			return "", nil
		}
		entry, err := c.Ingestion.EntryByUpload(ctx, p.UploadID)
		if errors.Is(err, model.ErrEntryNotFound) {
			return "no ingestion entry uses this upload any more", nil
		}
		if err != nil {
			return "", err
		}
		if !entry.State.AwaitingProcessing() && entry.State != model.EntryRetryable {
			return "the entry's outcome is recorded (" + string(entry.State) + ")", nil
		}
		return "", nil

	case event.TypeImportQueued:
		var p event.ImportQueued
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		if c.Datasets == nil {
			return "", nil
		}
		imp, err := c.Datasets.GetImport(ctx, env.TenantID, p.ImportID)
		if errors.Is(err, model.ErrImportNotFound) {
			return "the import no longer exists", nil
		}
		if err != nil {
			return "", err
		}
		if imp.State.Terminal() {
			return "the import is " + string(imp.State), nil
		}
		return "", nil

	case event.TypeValidationQueued:
		var p event.ValidationQueued
		if err := event.DecodePayload(env, &p); err != nil {
			return "", err
		}
		if c.Datasets == nil {
			return "", nil
		}
		job, err := c.Datasets.GetValidation(ctx, env.TenantID, p.JobID)
		if errors.Is(err, model.ErrValidationNotFound) {
			return "the validation no longer exists", nil
		}
		if err != nil {
			return "", err
		}
		if job.State.Terminal() {
			return "the validation is " + string(job.State), nil
		}
		return "", nil
	}
	return "", fmt.Errorf("%w: %q", event.ErrUnknownType, env.Type)
}

// uploadCompleted: the event is still needed while the upload is complete
// and its processing has not ended.
func (c Checks) uploadCompleted(ctx context.Context, tenantID, uploadID string) (string, error) {
	session, err := c.Uploads.GetByID(ctx, tenantID, uploadID)
	if errors.Is(err, model.ErrUploadNotFound) {
		return "the upload no longer exists", nil
	}
	if err != nil {
		return "", err
	}
	if session.Status != model.UploadStatusCompleted && session.Status != model.UploadStatusVerified {
		return "the upload is " + string(session.Status), nil
	}

	asset, err := c.Assets.GetByUpload(ctx, tenantID, uploadID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	job, err := c.Jobs.GetByAsset(ctx, tenantID, asset.ID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if job.State != model.ProcessingPending {
		return "processing already ended: " + string(job.State), nil
	}
	return "", nil
}
