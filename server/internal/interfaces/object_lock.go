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
	// LockObjectVersion sets the version's retention (GOVERNANCE mode)
	// when until is not zero, and turns its legal hold on or off.
	LockObjectVersion(ctx context.Context, bucket, key, versionID string, until time.Time, legalHold bool) error
}

// ObjectLockRepository records what was last applied to each asset.
type ObjectLockRepository interface {
	GetLock(ctx context.Context, tenantID, assetID string) (model.ObjectLock, bool, error)
	PutLock(ctx context.Context, lock model.ObjectLock) error
}
