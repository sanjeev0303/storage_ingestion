package response

import "time"

// DeletionResponse is an asset's deletion as its owner sees it. Internal
// reasons (legal holds among them) are not part of it.
type DeletionResponse struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	PurgeAfter time.Time `json:"purge_after"`
	// Restorable is true while nothing has been removed yet.
	Restorable bool `json:"restorable"`
}

// DeleteAssetResponse answers DELETE /api/v1/assets/{id}.
type DeleteAssetResponse struct {
	AssetID  string           `json:"asset_id"`
	Status   string           `json:"status"`
	Deletion DeletionResponse `json:"deletion"`
	// Created is false when the asset already had this deletion.
	Created bool `json:"created"`
}

// RestoreAssetResponse answers POST /api/v1/assets/{id}/restore.
type RestoreAssetResponse struct {
	AssetID string `json:"asset_id"`
	Status  string `json:"status"`
}

// AssetLifecycleResponse answers GET /api/v1/assets/{id}/lifecycle.
type AssetLifecycleResponse struct {
	AssetID     string     `json:"asset_id"`
	Status      string     `json:"status"`
	Dataset     string     `json:"dataset,omitempty"`
	RetainUntil time.Time  `json:"retain_until"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	// DeletionGraceSeconds is how long a deletion stays restorable.
	DeletionGraceSeconds int64             `json:"deletion_grace_seconds"`
	Deletion             *DeletionResponse `json:"deletion,omitempty"`
	// LockedUntil: on a bucket with S3 Object Lock, the provider refuses
	// to delete the stored version before this.
	LockedUntil *time.Time `json:"locked_until,omitempty"`
}
