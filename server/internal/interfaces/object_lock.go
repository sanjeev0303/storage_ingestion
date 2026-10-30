package interfaces

import (
	"context"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// ObjectLocker is a provider that can protect one object version itself,
// so not even the bucket's own credentials can delete it early: S3 Object
// Lock. Providers without it answer false from ObjectLockEnabled.
type ObjectLocker interface {
	ObjectLockEnabled(ctx context.Context, bucket string) (bool, error)
	// LockObjectVersion sets the version's retention, in lock.Mode, when
	// lock.Until is not zero, and turns its legal hold on or off.
	LockObjectVersion(ctx context.Context, bucket, key, versionID string, lock VersionLock) error
	// ObjectLockStatus reads back what the provider holds for the version.
	ObjectLockStatus(ctx context.Context, bucket, key, versionID string) (VersionLock, error)
}

// VersionLock is one stored version's Object Lock: its retention mode and
// date (zero when none) and its legal hold.
type VersionLock struct {
	Mode      string
	Until     time.Time
	LegalHold bool
}

// ObjectLockRepository records what was last applied to each asset.
type ObjectLockRepository interface {
	GetLock(ctx context.Context, tenantID, assetID string) (model.ObjectLock, bool, error)
	PutLock(ctx context.Context, lock model.ObjectLock) error
	DueVerification(ctx context.Context, now time.Time, limit int) ([]model.ObjectLock, error)
	RecordVerification(ctx context.Context, assetID, result string, at time.Time) error
}
