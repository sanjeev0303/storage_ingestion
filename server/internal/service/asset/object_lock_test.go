package asset

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// lockingStorage is memory storage with Object Lock: it keeps what was set
// on each version and, like S3, refuses to delete a held or retained one.
type lockingStorage struct {
	*storagememory.ObjectStorage
	enabled bool
	clock   *time.Time
	calls   int
	until   map[string]time.Time
	hold    map[string]bool
}

func (l *lockingStorage) ObjectLockEnabled(context.Context, string) (bool, error) { return l.enabled, nil }

func (l *lockingStorage) LockObjectVersion(_ context.Context, _, _, versionID string, until time.Time, hold bool) error {
	l.calls++
	if !until.IsZero() {
		l.until[versionID] = until
	}
	l.hold[versionID] = hold
	return nil
}

func (l *lockingStorage) DeleteObjectVersion(ctx context.Context, bucket, key, versionID string) error {
	if l.hold[versionID] || l.clock.Before(l.until[versionID]) {
		return errors.New("AccessDenied: the object is WORM protected")
	}
	return l.ObjectStorage.DeleteObjectVersion(ctx, bucket, key, versionID)
}

type memLocks map[string]model.ObjectLock

func (m memLocks) GetLock(_ context.Context, _, assetID string) (model.ObjectLock, bool, error) {
	l, ok := m[assetID]
	return l, ok, nil
}

func (m memLocks) PutLock(_ context.Context, l model.ObjectLock) error {
	m[l.AssetID] = l
	return nil
}

func (e *env) versioned(id, versionID string) {
	e.t.Helper()
	e.published(id, e.clock)
	e.storage.SetVersion("assets/tenant-1/"+id, versionID)
	e.assets.Update(id, func(a *model.Asset) { a.StorageVersionID = versionID })
}

func (e *env) evaluate(svc *Service) {
	e.t.Helper()
	if err := svc.EvaluateRetention(context.Background(), &CycleStats{}); err != nil {
		e.t.Fatal(err)
	}
}

func TestObjectLockFollowsRetentionAndHolds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	locker := &lockingStorage{ObjectStorage: e.storage, enabled: true, clock: &e.clock, until: map[string]time.Time{}, hold: map[string]bool{}}
	e.storageAPI = locker
	created := e.clock
	e.versioned("a1", "v1")
	locks := memLocks{}
	svc := e.service("worker-a")
	svc.deps.Locks = locks

	setRetention := func(d time.Duration) {
		t.Helper()
		if _, err := svc.SetPolicy(ctx, model.RetentionPolicy{
			TenantID: "tenant-1", Scope: model.ScopeTenant, MinRetention: duration(d), CreatedBy: "compliance",
			Reason: "test", EffectiveFrom: e.clock.Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The retention period goes onto the stored version, once.
	setRetention(30 * day)
	e.evaluate(svc)
	if !locker.until["v1"].Equal(created.Add(30*day)) || locker.hold["v1"] || locks["a1"].RetainUntil == nil {
		t.Fatalf("after evaluation: until %v hold %v record %+v", locker.until["v1"], locker.hold["v1"], locks["a1"])
	}
	calls := locker.calls
	e.evaluate(svc)
	if locker.calls != calls {
		t.Fatal("an unchanged lock was applied again")
	}

	// A legal hold goes on and comes off; a shorter policy never shortens
	// the lock.
	hold, err := svc.PlaceHold(ctx, model.LegalHold{TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "a1", Reason: "case", PlacedBy: "legal"})
	if err != nil {
		t.Fatal(err)
	}
	e.evaluate(svc)
	if !locker.hold["v1"] || !locks["a1"].LegalHold {
		t.Fatal("the legal hold was not set on the stored version")
	}
	if err := svc.ReleaseHold(ctx, "tenant-1", hold.ID, "legal"); err != nil {
		t.Fatal(err)
	}
	setRetention(day)
	e.evaluate(svc)
	if locker.hold["v1"] || !locker.until["v1"].Equal(created.Add(30*day)) {
		t.Fatalf("after release and a shorter policy: hold %v until %v", locker.hold["v1"], locker.until["v1"])
	}

	// A deletion waits for the lock, not only for the current policy.
	if _, err := svc.RequestDeletion(ctx, owner, "a1"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(2 * day)
	if stats := e.run(svc); stats.Protected != 1 || !strings.Contains(e.op("a1").ProtectedReason, "object lock until") {
		t.Fatalf("deletion under a lock: %+v %q", stats, e.op("a1").ProtectedReason)
	}
	e.clock = created.Add(31 * day)
	if stats := e.run(svc); stats.Completed != 1 || e.objects("a1") != 0 {
		t.Fatalf("deletion after the lock ended: %+v", stats)
	}

	// A hold the platform set is taken off the version before deleting,
	// even when the asset was soft-deleted since the last evaluation.
	e.versioned("a2", "v2")
	held, err := svc.PlaceHold(ctx, model.LegalHold{TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "a2", Reason: "case", PlacedBy: "legal"})
	if err != nil {
		t.Fatal(err)
	}
	e.evaluate(svc)
	if !locker.hold["v2"] {
		t.Fatal("the hold was not set on a2")
	}
	if _, err := svc.RequestDeletion(ctx, owner, "a2"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReleaseHold(ctx, "tenant-1", held.ID, "legal"); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(2 * day)
	if stats := e.run(svc); stats.Completed != 1 || locker.hold["v2"] {
		t.Fatalf("a2's deletion: %+v hold %v reason %q", stats, locker.hold["v2"], e.op("a2").ProtectedReason)
	}
}

func TestObjectLockLeavesOrdinaryBucketsAlone(t *testing.T) {
	e := newEnv(t)
	locker := &lockingStorage{ObjectStorage: e.storage, clock: &e.clock, until: map[string]time.Time{}, hold: map[string]bool{}}
	e.storageAPI = locker
	e.versioned("a1", "v1")
	locks := memLocks{}
	svc := e.service("worker-a")
	svc.deps.Locks = locks
	if _, err := svc.SetPolicy(context.Background(), model.RetentionPolicy{
		TenantID: "tenant-1", Scope: model.ScopeTenant, MinRetention: duration(30 * day), CreatedBy: "c", Reason: "r",
		EffectiveFrom: e.clock.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	e.evaluate(svc)
	if locker.calls != 0 || len(locks) != 0 {
		t.Fatalf("a bucket without Object Lock got %d lock calls", locker.calls)
	}
}

func TestOwnerInspectShowsTheLockDate(t *testing.T) {
	e := newEnv(t)
	e.versioned("a1", "v1")
	until := e.clock.Add(30 * day)
	locks := memLocks{"a1": {AssetID: "a1", TenantID: "tenant-1", RetainUntil: &until, LegalHold: true}}
	svc := e.service("worker-a")
	svc.deps.Locks = locks
	inspection, err := svc.OwnerInspect(context.Background(), owner, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.LockedUntil == nil || !inspection.LockedUntil.Equal(until) || inspection.Holds != nil {
		t.Fatalf("owner inspection: locked until %v, holds %v", inspection.LockedUntil, inspection.Holds)
	}
}
