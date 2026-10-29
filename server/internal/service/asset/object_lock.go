package asset

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Object Lock (docs/lifecycle.md): on a bucket with S3 Object Lock, an
// asset's retention period and legal holds are also set on its stored
// version, so the provider refuses to delete it early, whoever holds the
// bucket's keys. Retention evaluation keeps them in step; a lock is only
// ever extended, never shortened.

// lockCheckTTL is how long a bucket's Object Lock setting is trusted.
const lockCheckTTL = 10 * time.Minute

type lockCheck struct {
	enabled bool
	at      time.Time
}

type lockBuckets struct {
	mu     sync.Mutex
	checks map[string]lockCheck
}

// lockable is the asset's delivery copy, when it sits at a recorded
// version on a bucket with Object Lock on.
func (s *Service) lockable(ctx context.Context, asset model.Asset) (interfaces.ObjectLocker, string, string, bool, error) {
	if s.deps.Locks == nil || asset.StorageVersionID == "" {
		return nil, "", "", false, nil
	}
	storage, place, err := s.place(ctx, asset.TenantID, asset.StorageConnectionID)
	if err != nil {
		return nil, "", "", false, err
	}
	locker, ok := storage.(interfaces.ObjectLocker)
	if !ok {
		return nil, "", "", false, nil
	}
	key := fmt.Sprintf("%s/%s/%s", place.ReadyPrefix, asset.TenantID, asset.ID)
	if asset.SharedFrom != "" {
		key = asset.ObjectKey // the object it shares; a lock only ever extends
	}
	if asset.ObjectKey != key {
		return nil, "", "", false, nil
	}
	cacheKey := place.ConnectionID + "/" + place.ReadyBucket
	s.locks.mu.Lock()
	check, seen := s.locks.checks[cacheKey]
	s.locks.mu.Unlock()
	if !seen || s.now().Sub(check.at) > lockCheckTTL {
		enabled, err := locker.ObjectLockEnabled(ctx, place.ReadyBucket)
		if err != nil {
			return nil, "", "", false, fmt.Errorf("check object lock: %w", err)
		}
		check = lockCheck{enabled: enabled, at: s.now()}
		s.locks.mu.Lock()
		if s.locks.checks == nil {
			s.locks.checks = map[string]lockCheck{}
		}
		s.locks.checks[cacheKey] = check
		s.locks.mu.Unlock()
	}
	return locker, place.ReadyBucket, key, check.enabled, nil
}

// syncLock brings an available asset's lock up to its retention period
// and legal holds.
func (s *Service) syncLock(ctx context.Context, asset model.Asset, policy EffectivePolicy) error {
	if asset.Status != model.AssetStatusAvailable {
		return nil
	}
	locker, bucket, key, enabled, err := s.lockable(ctx, asset)
	if err != nil || !enabled {
		return err
	}
	current, _, err := s.deps.Locks.GetLock(ctx, asset.TenantID, asset.ID)
	if err != nil {
		return err
	}
	holds, err := s.deps.Holds.Active(ctx, asset.TenantID, asset.Dataset, asset.ID)
	if err != nil {
		return fmt.Errorf("check legal holds: %w", err)
	}
	want := model.ObjectLock{AssetID: asset.ID, TenantID: asset.TenantID, RetainUntil: current.RetainUntil, LegalHold: len(holds) > 0}
	var extendTo time.Time
	if until := policy.RetainUntil(asset.CreatedAt); until.After(s.now()) &&
		(current.RetainUntil == nil || until.After(*current.RetainUntil)) {
		extendTo = until
		want.RetainUntil = &until
	}
	if extendTo.IsZero() && want.LegalHold == current.LegalHold {
		return nil
	}
	if err := locker.LockObjectVersion(ctx, bucket, key, asset.StorageVersionID, extendTo, want.LegalHold); err != nil {
		return fmt.Errorf("apply object lock: %w", err)
	}
	want.AppliedAt = s.now()
	s.deps.Metrics.Add("object_locks_applied", 1)
	return s.deps.Locks.PutLock(ctx, want)
}

// lockBlocker is the reason a locked version cannot be deleted yet, and
// until when.
func (s *Service) lockBlocker(ctx context.Context, asset model.Asset) (string, time.Time, error) {
	if s.deps.Locks == nil {
		return "", time.Time{}, nil
	}
	lock, found, err := s.deps.Locks.GetLock(ctx, asset.TenantID, asset.ID)
	if err != nil || !found || lock.RetainUntil == nil || !s.now().Before(*lock.RetainUntil) {
		return "", time.Time{}, err
	}
	return "object lock until " + lock.RetainUntil.Format(time.RFC3339), *lock.RetainUntil, nil
}

// releaseLockHold turns off a legal hold the platform set on the stored
// version, once no hold covers the asset any more, so its deletion can
// go ahead.
func (s *Service) releaseLockHold(ctx context.Context, asset model.Asset) error {
	if s.deps.Locks == nil {
		return nil
	}
	lock, found, err := s.deps.Locks.GetLock(ctx, asset.TenantID, asset.ID)
	if err != nil || !found || !lock.LegalHold {
		return err
	}
	locker, bucket, key, enabled, err := s.lockable(ctx, asset)
	if err != nil || !enabled {
		return err
	}
	if err := locker.LockObjectVersion(ctx, bucket, key, asset.StorageVersionID, time.Time{}, false); err != nil {
		return fmt.Errorf("release object legal hold: %w", err)
	}
	lock.LegalHold, lock.AppliedAt = false, s.now()
	return s.deps.Locks.PutLock(ctx, lock)
}
