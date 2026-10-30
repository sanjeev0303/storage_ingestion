package asset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	storagememory "github.com/sanjeev0303/object-storage-platform/internal/infrastructure/storage/memory"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
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
	mode    map[string]string
}

func (l *lockingStorage) ObjectLockEnabled(context.Context, string) (bool, error) { return l.enabled, nil }

func (l *lockingStorage) LockObjectVersion(_ context.Context, _, _, versionID string, lock interfaces.VersionLock) error {
	l.calls++
	if !lock.Until.IsZero() {
		// Like S3: a lock in force cannot be shortened or change its mode
		// without the governance bypass.
		if l.clock.Before(l.until[versionID]) && (lock.Mode != l.mode[versionID] || lock.Until.Before(l.until[versionID])) {
			return errors.New("InvalidRequest: retention in force cannot be reduced or change mode")
		}
		l.until[versionID] = lock.Until
		if l.mode == nil {
			l.mode = map[string]string{}
		}
		l.mode[versionID] = lock.Mode
	}
	l.hold[versionID] = lock.LegalHold
	return nil
}

func (l *lockingStorage) ObjectLockStatus(_ context.Context, _, _, versionID string) (interfaces.VersionLock, error) {
	return interfaces.VersionLock{Mode: l.mode[versionID], Until: l.until[versionID], LegalHold: l.hold[versionID]}, nil
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

func (m memLocks) DueVerification(_ context.Context, now time.Time, limit int) ([]model.ObjectLock, error) {
	var out []model.ObjectLock
	for _, l := range m {
		if len(out) < limit && (l.LegalHold || (l.RetainUntil != nil && l.RetainUntil.After(now))) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (m memLocks) RecordVerification(_ context.Context, assetID, result string, at time.Time) error {
	l := m[assetID]
	l.VerifiedAt, l.VerifyResult = &at, result
	m[assetID] = l
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

// memLockSync is the lock sync queue over the env's assets.
type memLockSync struct {
	e        *env
	requests []model.LockSyncRequest
	done     map[string]bool
}

func (m *memLockSync) Request(_ context.Context, r model.LockSyncRequest) error {
	m.requests = append(m.requests, r)
	return nil
}

func (m *memLockSync) Next(context.Context, string) (model.LockSyncRequest, bool, error) {
	for _, r := range m.requests {
		if !m.done[r.ID] {
			return r, true, nil
		}
	}
	return model.LockSyncRequest{}, false, nil
}

func (m *memLockSync) Assets(ctx context.Context, r model.LockSyncRequest, limit int) ([]model.Asset, error) {
	all, err := m.e.assets.ListAfter(ctx, r.Position, r.TenantID, []model.AssetStatus{model.AssetStatusAvailable}, 1000)
	if err != nil {
		return nil, err
	}
	var out []model.Asset
	for _, a := range all {
		if (r.Scope == model.ScopeAsset && a.ID != r.ScopeKey) || (r.Scope == model.ScopeDataset && a.Dataset != r.ScopeKey) {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, a)
	}
	return out, nil
}

func (m *memLockSync) Advance(_ context.Context, id, position string, done bool, _ time.Time) error {
	for i := range m.requests {
		if m.requests[i].ID == id {
			m.requests[i].Position = position
		}
	}
	if done {
		m.done[id] = true
	}
	return nil
}

// A hold placed on one asset reaches its stored version on the next
// cycle, however far the sweep is from that asset; its release does too.
func TestPolicyAndHoldChangesAreLockedAheadOfTheSweep(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	locker := &lockingStorage{ObjectStorage: e.storage, enabled: true, clock: &e.clock, until: map[string]time.Time{}, hold: map[string]bool{}}
	e.storageAPI = locker
	for i := 0; i < 30; i++ {
		e.versioned(fmt.Sprintf("a%02d", i), fmt.Sprintf("v%02d", i))
	}
	svc := e.service("worker-a")
	svc.deps.Locks = memLocks{}
	queue := &memLockSync{e: e, done: map[string]bool{}}
	svc.deps.LockSync = queue

	// The sweep evaluates 10 assets a cycle, a00 to a09 first; a29 is
	// three cycles away.
	hold, err := svc.PlaceHold(ctx, model.LegalHold{TenantID: "tenant-1", Scope: model.ScopeAsset, ScopeKey: "a29", Reason: "case", PlacedBy: "legal"})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !locker.hold["v29"] || stats.LocksSynced != 1 {
		t.Fatalf("after one cycle: hold on a29 %v, synced %d", locker.hold["v29"], stats.LocksSynced)
	}
	if err := svc.ReleaseHold(ctx, "tenant-1", hold.ID, "legal"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if locker.hold["v29"] {
		t.Fatal("the released hold stayed on the stored version for another cycle")
	}
	if _, found, _ := queue.Next(ctx, ""); found {
		t.Fatal("a finished request is still queued")
	}
}
