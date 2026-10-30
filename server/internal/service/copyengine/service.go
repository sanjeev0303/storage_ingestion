// Package copyengine copies objects between storage connections, always
// verified by SHA-256: replicas at a second provider, migrations, restores,
// export bundles, restore drills and scrubs that repair from a replica
// (docs/copy-engine.md).
package copyengine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Store is the copy repository (postgres.CopyRepository).
type Store interface {
	CreateJob(ctx context.Context, j model.CopyJob) (model.CopyJob, bool, error)
	GetJob(ctx context.Context, tenantID, id string) (model.CopyJob, error)
	ListJobs(ctx context.Context, tenantID, datasetID string, limit int) ([]model.CopyJob, error)
	ClaimJob(ctx context.Context, owner string, now time.Time, lease time.Duration) (model.CopyJob, bool, error)
	Release(ctx context.Context, id, owner string) error
	Finish(ctx context.Context, id, owner, state, message string, report any, now time.Time) error
	Cancel(ctx context.Context, tenantID, id string, now time.Time) error
	AddItems(ctx context.Context, jobID string, items []model.CopyItem) error
	PendingItems(ctx context.Context, jobID string, limit int) ([]model.CopyItem, error)
	Items(ctx context.Context, tenantID, jobID, state string, after, limit int) ([]model.CopyItem, error)
	FinishItem(ctx context.Context, it model.CopyItem, now time.Time) error

	DatasetAssets(ctx context.Context, tenantID, datasetID string) ([]postgres.StoredAsset, error)
	VersionAssets(ctx context.Context, tenantID, versionID string) ([]postgres.StoredAsset, error)
	GetStoredAsset(ctx context.Context, tenantID, assetID string) (postgres.StoredAsset, error)
	SwitchConnection(ctx context.Context, tenantID, datasetID, targetID string, moved []postgres.SwitchedAsset, now time.Time) (int64, error)

	PendingReplicas(ctx context.Context, now time.Time, limit int) ([]postgres.PendingReplica, error)
	PutReplica(ctx context.Context, rep model.AssetReplica) error
	ReplicaFailed(ctx context.Context, tenantID, assetID, connectionID, message string, now time.Time) error
	VerifiedReplica(ctx context.Context, tenantID, assetID string) (model.AssetReplica, bool, error)
	ReplicationStatus(ctx context.Context, tenantID, datasetID, connectionID string) (postgres.ReplicationStatus, error)

	ScrubDue(ctx context.Context, now time.Time, limit int) ([]postgres.StoredAsset, error)
	PutScrub(ctx context.Context, tenantID string, s model.ScrubResult) error
	ScrubFailures(ctx context.Context, tenantID, datasetID string, limit int) ([]model.ScrubResult, error)
	Repaired(ctx context.Context, tenantID, assetID, versionID, etag string, now time.Time) error

	Protection(ctx context.Context, tenantID, datasetID string) (model.Protection, string, error)
	SetCleanVersion(ctx context.Context, tenantID, datasetID, versionID string) error
	DiffSinceClean(ctx context.Context, tenantID, datasetID, cleanVersionID string, limit int) ([]postgres.CleanDiff, error)

	Rows(ctx context.Context, query string, args []any, fn func(map[string]any) error) error
	Schedule(ctx context.Context, tenantID string) (postgres.ExportSchedule, bool, error)
	PutSchedule(ctx context.Context, s postgres.ExportSchedule) error
	DeleteSchedule(ctx context.Context, tenantID string) error
	DueSchedules(ctx context.Context, now time.Time) ([]postgres.ExportSchedule, error)
	MarkScheduleRun(ctx context.Context, tenantID, jobID string, now time.Time) error
	RecentDeletions(ctx context.Context, since time.Time, atLeast int) ([]postgres.DeletionCount, error)
}

// PostureReader reads connections' posture (the connection repository).
type PostureReader interface {
	Posture(ctx context.Context, tenantID, connectionID string) (model.PostureReport, bool, error)
}

// Alerter raises deduplicated security alerts (the access store).
type Alerter interface {
	AddAlert(ctx context.Context, a model.SecurityAlert) (bool, error)
}

type Dependencies struct {
	Store   Store
	Router  interfaces.StorageRouter
	Packs   interfaces.PackLocator
	Locks   interfaces.ObjectLockRepository
	Posture PostureReader
	Alerts  Alerter
	Audit   interfaces.AuditRecorder
}

type Config struct {
	// Owner names this process's leases.
	Owner string
	Lease time.Duration
	// Batch is how many objects one pass copies per job, and how many
	// replicas and scrubs one pass makes.
	Batch int
	// MassDeletions is how many deletions by one member in an hour raise an
	// alert (0 turns the alert off).
	MassDeletions int
}

type Service struct {
	deps  Dependencies
	cfg   Config
	log   *slog.Logger
	now   func() time.Time
	newID func() string
}

func NewService(deps Dependencies, cfg Config, log *slog.Logger) *Service {
	if cfg.Lease <= 0 {
		cfg.Lease = 5 * time.Minute
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	return &Service{deps: deps, cfg: cfg, log: log, now: func() time.Time { return time.Now().UTC() }, newID: randomID}
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ─── Moving bytes ─────────────────────────────────────────────────────

// open streams an asset's stored bytes from where they are: its own
// object, pinned to the version it was published as, or its entry in a
// pack.
func (s *Service) open(ctx context.Context, a postgres.StoredAsset) (io.ReadCloser, error) {
	loc, err := s.deps.Router.Locate(ctx, a.TenantID, a.ConnectionID)
	if err != nil {
		return nil, err
	}
	if s.deps.Packs != nil {
		packed, ok, err := s.deps.Packs.PackedLocation(ctx, a.TenantID, a.AssetID)
		if err != nil {
			return nil, err
		}
		if ok {
			start, end, err := packed.Range(0, -1)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", model.ErrPackCorrupt, err)
			}
			return loc.Storage.OpenRange(ctx, packed.Bucket, packed.ObjectKey, start, end,
				interfaces.ReadCondition{IfMatch: packed.StorageETag, VersionID: packed.StorageVersionID})
		}
	}
	return loc.Storage.OpenRange(ctx, loc.ReadyBucket, a.ObjectKey, 0, -1, interfaces.ReadCondition{VersionID: a.VersionID})
}

// hashOf reads r to the end and returns its SHA-256 and length.
func hashOf(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// copyTo writes an asset's bytes to key at dst, hashing them on the way,
// then reads the copy back and hashes it again. The copy is verified, or
// it is deleted and an error returned.
func (s *Service) copyTo(ctx context.Context, a postgres.StoredAsset, dst interfaces.StorageLocation, key string) (interfaces.ObjectMetadata, error) {
	return s.copyFrom(ctx, func(ctx context.Context) (io.ReadCloser, error) { return s.open(ctx, a) }, a, dst, key)
}

// copyFrom is copyTo from any reader of a's bytes: a replica, for a repair.
func (s *Service) copyFrom(ctx context.Context, open func(context.Context) (io.ReadCloser, error), a postgres.StoredAsset,
	dst interfaces.StorageLocation, key string) (interfaces.ObjectMetadata, error) {
	body, err := open(ctx)
	if err != nil {
		return interfaces.ObjectMetadata{}, fmt.Errorf("read the source: %w", err)
	}
	defer body.Close()
	h := sha256.New()
	meta, err := dst.Storage.WriteObject(ctx, dst.ReadyBucket, key, io.TeeReader(body, h), a.SizeBytes, a.ContentType)
	if err != nil {
		return meta, fmt.Errorf("write the copy: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); a.SHA256 != "" && got != a.SHA256 {
		_ = dst.Storage.DeleteObject(context.WithoutCancel(ctx), dst.ReadyBucket, key)
		return meta, fmt.Errorf("%w: the source read as %s", model.ErrChecksumMismatch, got)
	}
	back, err := dst.Storage.OpenRange(ctx, dst.ReadyBucket, key, 0, -1, interfaces.ReadCondition{VersionID: meta.VersionID})
	if err != nil {
		return meta, fmt.Errorf("read the copy back: %w", err)
	}
	got, n, err := hashOf(back)
	back.Close()
	switch {
	case err != nil:
		return meta, fmt.Errorf("read the copy back: %w", err)
	case n != a.SizeBytes || (a.SHA256 != "" && got != a.SHA256):
		_ = dst.Storage.DeleteObject(context.WithoutCancel(ctx), dst.ReadyBucket, key)
		return meta, fmt.Errorf("%w: the copy read back as %d bytes, %s", model.ErrChecksumMismatch, n, got)
	}
	return meta, nil
}

// carryLock gives a copy the Object Lock its source holds: the same
// mode, date and legal hold. A copy whose bucket cannot lock reports
// false; with COMPLIANCE that is an error, since the copy would be
// weaker than what it protects.
func (s *Service) carryLock(ctx context.Context, a postgres.StoredAsset, dst interfaces.StorageLocation, key, versionID string) (interfaces.VersionLock, error) {
	if s.deps.Locks == nil {
		return interfaces.VersionLock{}, nil
	}
	lock, ok, err := s.deps.Locks.GetLock(ctx, a.TenantID, a.AssetID)
	if err != nil || !ok || (lock.RetainUntil == nil && !lock.LegalHold) {
		return interfaces.VersionLock{}, err
	}
	want := interfaces.VersionLock{Mode: lock.Mode, LegalHold: lock.LegalHold}
	if lock.RetainUntil != nil && lock.RetainUntil.After(s.now()) {
		want.Until = *lock.RetainUntil
		if want.Mode == "" {
			want.Mode = "GOVERNANCE"
		}
	}
	if err := s.lock(ctx, dst, key, versionID, want); err != nil {
		if want.Mode == "COMPLIANCE" {
			return want, fmt.Errorf("the source is under a COMPLIANCE lock the target cannot keep: %w", err)
		}
		return interfaces.VersionLock{}, nil
	}
	return want, nil
}

var errNoLock = errors.New("the target bucket does not have Object Lock")

func (s *Service) lock(ctx context.Context, dst interfaces.StorageLocation, key, versionID string, want interfaces.VersionLock) error {
	locker, ok := dst.Storage.(interfaces.ObjectLocker)
	if !ok || versionID == "" {
		return errNoLock
	}
	enabled, err := locker.ObjectLockEnabled(ctx, dst.ReadyBucket)
	if err != nil {
		return err
	}
	if !enabled {
		return errNoLock
	}
	return locker.LockObjectVersion(ctx, dst.ReadyBucket, key, versionID, want)
}

// promotedKey is where an asset lives under a connection's ready prefix,
// as delivery expects it: by its owner (the asset whose bytes it shares).
func promotedKey(readyPrefix string, a postgres.StoredAsset) string {
	owner := a.AssetID
	if a.SharedFrom != "" {
		owner = a.SharedFrom
	}
	return readyPrefix + "/" + a.TenantID + "/" + owner
}

// The platform's own folders under an organization's prefix. The drift
// scan skips names starting with a dot.
func replicaKey(loc interfaces.StorageLocation, tenantID, assetID string) string {
	return loc.ReadyPrefix + "/" + tenantID + "/.replicas/" + assetID
}

func cleanPath(p string) string {
	p = strings.TrimLeft(strings.ReplaceAll(p, "\\", "/"), "/")
	var out []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." && part != ".." {
			out = append(out, part)
		}
	}
	return strings.Join(out, "/")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
