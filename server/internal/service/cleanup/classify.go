package cleanup

import (
	"context"
	"errors"
	"strings"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Where an object lives decides which record owns it.
const (
	kindQuarantine = "quarantine"
	kindDelivery   = "delivery"
)

// verdict is what the metadata says about one stored object. An object is
// an orphan only when an authoritative record says nothing owns it (or,
// for the unrecorded categories, when no record exists at all); anything
// in doubt is skipped, never deleted.
type verdict struct {
	orphan   bool
	category string
	// reason explains the decision; for a skip, skip names the counter.
	reason   string
	skip     string
	tenantID string
	assetID  string
	dataset  string
}

func skipped(counter, reason string) verdict {
	return verdict{skip: counter, reason: reason}
}

// classify reads the authoritative records for one object. Keys are only
// parsed to find those records; a key alone never decides anything.
func (s *Service) classify(ctx context.Context, a area, object interfaces.StoredObject, checkAge bool) (verdict, error) {
	// A young object may belong to a request or worker that has not
	// committed its record yet.
	if checkAge && s.now().Sub(object.LastModified) < s.cfg.MinAge {
		return skipped("young", "newer than the safety grace period"), nil
	}
	if a.kind == kindQuarantine {
		return s.classifyQuarantine(ctx, a, object.ObjectKey)
	}
	return s.classifyDelivery(ctx, a, object.ObjectKey)
}

// boundTo tells whether a record's connection binding is the area's. A
// record bound elsewhere does not own an object here, nor disown it.
func boundTo(a area, connectionID string) bool {
	if connectionID == "" {
		connectionID = model.PlatformConnectionID
	}
	return connectionID == a.connectionID
}

func (s *Service) classifyQuarantine(ctx context.Context, a area, key string) (verdict, error) {
	tenantID, uploadID, ok := splitKey(key, a.base)
	if !ok {
		return skipped("not_managed", "not an upload object key"), nil
	}

	session, err := s.deps.Uploads.GetByID(ctx, tenantID, uploadID)
	if errors.Is(err, model.ErrUploadNotFound) {
		return verdict{orphan: true, category: model.OrphanNoUploadSession, reason: "no upload session exists for this object", tenantID: tenantID}, nil
	}
	if err != nil {
		return verdict{}, err
	}
	if !boundTo(a, session.StagingConnectionID) {
		return skipped("other_connection", "the upload session stages on another storage connection"), nil
	}

	switch session.Status {
	case model.UploadStatusAborted, model.UploadStatusExpired:
		return skipped("reconciliation", "multipart reconciliation removes objects of aborted and expired uploads"), nil
	case model.UploadStatusPending, model.UploadStatusUploading, model.UploadStatusCompleting, model.UploadStatusAborting:
		return skipped("active_upload", "upload is "+string(session.Status)), nil
	}

	asset, err := s.deps.Assets.GetByUpload(ctx, tenantID, uploadID)
	if errors.Is(err, model.ErrAssetNotFound) {
		if session.Status == model.UploadStatusFailed {
			if s.now().Sub(session.UpdatedAt) >= s.cfg.FailedUploadRetention {
				return verdict{orphan: true, category: model.OrphanExpiredFailedUpload,
					reason:   "failed upload kept as evidence for " + s.cfg.FailedUploadRetention.String() + "; that has passed",
					tenantID: tenantID, dataset: session.Dataset}, nil
			}
			return skipped("evidence_retention", "failed upload kept as evidence"), nil
		}
		return skipped("awaiting_processing", "completed upload without an asset yet"), nil
	}
	if err != nil {
		return verdict{}, err
	}

	owned := verdict{tenantID: tenantID, assetID: asset.ID, dataset: asset.Dataset}
	switch asset.Status {
	case model.AssetStatusQuarantined, model.AssetStatusProcessing:
		return skipped("processing", "asset is "+string(asset.Status)), nil
	case model.AssetStatusRejected:
		return skipped("quarantine_retention", "the processing worker purges rejected objects after quarantine retention"), nil
	case model.AssetStatusSoftDeleted:
		return skipped("deletion_workflow", "the deletion workflow removes it"), nil
	case model.AssetStatusDeleted:
		owned.orphan, owned.category, owned.reason = true, model.OrphanDeletedAssetObject, "the asset is deleted"
		return owned, nil
	}

	// AVAILABLE: the source is removed once promotion's cleanup stage ran.
	job, err := s.deps.Jobs.GetByAsset(ctx, tenantID, asset.ID)
	if err != nil && !errors.Is(err, model.ErrAssetNotFound) {
		return verdict{}, err
	}
	if err == nil && job.State == model.ProcessingSucceeded {
		owned.orphan, owned.category, owned.reason = true, model.OrphanLeftoverSource, "promotion finished without removing its source"
		return owned, nil
	}
	return skipped("promotion_cleanup", "promotion has not finished its cleanup"), nil
}

func (s *Service) classifyDelivery(ctx context.Context, a area, key string) (verdict, error) {
	tenantID, assetID, ok := splitKey(key, a.base)
	if !ok {
		return skipped("not_managed", "not a delivery object key"), nil
	}

	asset, err := s.deps.Assets.GetByID(ctx, tenantID, assetID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return verdict{orphan: true, category: model.OrphanNoAsset, reason: "no asset exists for this object", tenantID: tenantID, assetID: assetID}, nil
	}
	if err != nil {
		return verdict{}, err
	}
	if !boundTo(a, asset.StorageConnectionID) {
		return skipped("other_connection", "the asset is stored on another storage connection"), nil
	}

	owned := verdict{tenantID: tenantID, assetID: asset.ID, dataset: asset.Dataset}
	switch asset.Status {
	case model.AssetStatusAvailable:
		if asset.ObjectKey == key {
			return skipped("owned", "delivered asset"), nil
		}
		return skipped("uncertain", "an available asset recorded at another key"), nil
	case model.AssetStatusSoftDeleted:
		return skipped("deletion_workflow", "the deletion workflow removes it"), nil
	case model.AssetStatusDeleted:
		owned.orphan, owned.category, owned.reason = true, model.OrphanDeletedAssetObject, "the asset is deleted"
		return owned, nil
	case model.AssetStatusRejected:
		owned.orphan, owned.category, owned.reason = true, model.OrphanDeadPromotionCopy, "the asset was rejected"
		return owned, nil
	}

	// QUARANTINED or PROCESSING: a copy here belongs to a promotion.
	job, err := s.deps.Jobs.GetByAsset(ctx, tenantID, asset.ID)
	if errors.Is(err, model.ErrAssetNotFound) {
		return skipped("uncertain", "processing asset without a job"), nil
	}
	if err != nil {
		return verdict{}, err
	}
	switch job.State {
	case model.ProcessingPending:
		return skipped("promotion_in_progress", "promotion in progress"), nil
	case model.ProcessingExhausted:
		return skipped("held_asset", "kept for the held asset's requeue"), nil
	case model.ProcessingFailed:
		owned.orphan, owned.category, owned.reason = true, model.OrphanDeadPromotionCopy, "the asset's processing failed"
		return owned, nil
	}
	return skipped("uncertain", "job is "+string(job.State)+" but the asset is "+string(asset.Status)), nil
}

// splitKey reads prefix{tenant}/{id}; anything else is not ours.
func splitKey(key, prefix string) (string, string, bool) {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return "", "", false
	}
	tenant, id, ok := strings.Cut(rest, "/")
	if !ok || tenant == "" || id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	return tenant, id, true
}
