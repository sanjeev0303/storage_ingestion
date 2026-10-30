package copyengine

import (
	"context"
	"fmt"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ReplicateStats counts one replication pass.
type ReplicateStats struct {
	Copied, Failed int
	Bytes          int64
}

// Replicate copies, verified, the next assets of protected datasets that
// have no copy at their dataset's replica connection. A locked vault's
// copies are locked in COMPLIANCE mode for its retention; other copies
// keep whatever lock their source holds.
func (s *Service) Replicate(ctx context.Context) (ReplicateStats, error) {
	var stats ReplicateStats
	pending, err := s.deps.Store.PendingReplicas(ctx, s.now(), s.cfg.Batch)
	if err != nil {
		return stats, err
	}
	for _, p := range pending {
		rep, err := s.replicate(ctx, p)
		if err != nil {
			stats.Failed++
			s.log.Warn("replica not made", "tenant_id", p.TenantID, "asset_id", p.AssetID, "connection_id", p.ReplicaConnectionID, "error", err)
			if err := s.deps.Store.ReplicaFailed(ctx, p.TenantID, p.AssetID, p.ReplicaConnectionID, truncate(err.Error(), 500), s.now()); err != nil {
				return stats, err
			}
			continue
		}
		stats.Copied++
		stats.Bytes += rep.SizeBytes
	}
	return stats, nil
}

func (s *Service) replicate(ctx context.Context, p postgres.PendingReplica) (model.AssetReplica, error) {
	dst, err := s.deps.Router.Locate(ctx, p.TenantID, p.ReplicaConnectionID)
	if err != nil {
		return model.AssetReplica{}, err
	}
	key := replicaKey(dst, p.TenantID, p.AssetID)
	meta, err := s.copyTo(ctx, p.StoredAsset, dst, key)
	if err != nil {
		return model.AssetReplica{}, err
	}
	rep := model.AssetReplica{
		TenantID: p.TenantID, AssetID: p.AssetID, ConnectionID: p.ReplicaConnectionID, ObjectKey: key,
		VersionID: meta.VersionID, ETag: meta.ETag, SizeBytes: p.SizeBytes, ChecksumSHA256: p.SHA256,
		ReplicatedAt: s.now(), VerifiedAt: s.now(),
	}
	if p.VaultRetentionDays > 0 {
		until := s.now().Add(time.Duration(p.VaultRetentionDays) * 24 * time.Hour)
		if err := s.lock(ctx, dst, key, meta.VersionID, interfaces.VersionLock{Mode: "COMPLIANCE", Until: until}); err != nil {
			_ = dst.Storage.DeleteObject(context.WithoutCancel(ctx), dst.ReadyBucket, key)
			return model.AssetReplica{}, fmt.Errorf("lock the vault copy: %w", err)
		}
		rep.LockMode, rep.LockedUntil = "COMPLIANCE", &until
	} else {
		lock, err := s.carryLock(ctx, p.StoredAsset, dst, key, meta.VersionID)
		if err != nil {
			_ = dst.Storage.DeleteObject(context.WithoutCancel(ctx), dst.ReadyBucket, key)
			return model.AssetReplica{}, err
		}
		if !lock.Until.IsZero() {
			rep.LockMode, rep.LockedUntil = lock.Mode, &lock.Until
		}
	}
	return rep, s.deps.Store.PutReplica(ctx, rep)
}

// VerifiedReplica finds an asset's verified copy at another connection,
// for reads that fail over when the primary does not answer.
func (s *Service) VerifiedReplica(ctx context.Context, tenantID, assetID string) (model.AssetReplica, bool, error) {
	return s.deps.Store.VerifiedReplica(ctx, tenantID, assetID)
}

// Replication is a dataset's replica: how much is copied, and how old its
// recovery point is.
type Replication struct {
	ConnectionID       string     `json:"connection_id"`
	VaultRetentionDays int        `json:"vault_retention_days"`
	Escrow             bool       `json:"escrow"`
	ScrubDays          int        `json:"scrub_days"`
	Objects            int64      `json:"objects"`
	Replicated         int64      `json:"replicated"`
	Pending            int64      `json:"pending"`
	Failing            int64      `json:"failing"`
	ReplicatedBytes    int64      `json:"replicated_bytes"`
	NewestReplicaAt    *time.Time `json:"newest_replica_at,omitempty"`
	OldestPendingSince *time.Time `json:"oldest_pending_since,omitempty"`
	// RecoveryPointSeconds is how much recent data a restore from the
	// replica would lose now: the age of the oldest object not yet copied.
	RecoveryPointSeconds int64  `json:"recovery_point_seconds"`
	LastError            string `json:"last_error,omitempty"`
}

func (s *Service) ReplicationOf(ctx context.Context, p model.Principal, datasetID string) (Replication, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead, "dataset.replication.read", "dataset", datasetID); err != nil {
		return Replication{}, err
	}
	prot, _, err := s.deps.Store.Protection(ctx, p.TenantID, datasetID)
	if err != nil {
		return Replication{}, err
	}
	out := Replication{ConnectionID: prot.ReplicaConnectionID, VaultRetentionDays: prot.VaultRetentionDays, Escrow: prot.Escrow,
		ScrubDays: prot.ScrubDays}
	if prot.ReplicaConnectionID == "" {
		return out, nil
	}
	st, err := s.deps.Store.ReplicationStatus(ctx, p.TenantID, datasetID, prot.ReplicaConnectionID)
	if err != nil {
		return out, err
	}
	out.Objects, out.Replicated, out.Failing, out.ReplicatedBytes = st.Wanted, st.Replicated, st.Failing, st.ReplicatedBytes
	out.Pending = st.Wanted - st.Replicated
	out.NewestReplicaAt, out.OldestPendingSince, out.LastError = st.NewestReplicaAt, st.OldestPendingSince, st.LastError
	if st.OldestPendingSince != nil {
		out.RecoveryPointSeconds = int64(s.now().Sub(*st.OldestPendingSince).Seconds())
	}
	return out, nil
}

// CheckProtection is what a dataset's protection settings require: a
// replica connection unlike its own; and for a locked vault, a replica
// bucket with Object Lock in another account than the primary's, as their
// last posture checks found them.
func (s *Service) CheckProtection(ctx context.Context, p model.Principal, prot model.Protection, primaryID string) error {
	if prot.ReplicaConnectionID == "" {
		if prot.VaultRetentionDays > 0 || prot.Escrow {
			return fmt.Errorf("%w: a locked vault and escrow need a replica connection", model.ErrVaultRequirements)
		}
		return nil
	}
	if primaryID == "" {
		primaryID = model.PlatformConnectionID
	}
	if prot.ReplicaConnectionID == primaryID {
		return fmt.Errorf("%w: the replica must be at another connection than the dataset's own", model.ErrVaultRequirements)
	}
	if prot.VaultRetentionDays == 0 || s.deps.Posture == nil {
		return nil
	}
	replica, checked, err := s.deps.Posture.Posture(ctx, p.TenantID, prot.ReplicaConnectionID)
	if err != nil {
		return err
	}
	if lock, ok := replica.Check(model.CheckObjectLock); !checked || !ok || lock.Status != model.PosturePass {
		return fmt.Errorf("%w: test the replica connection; its bucket must have Object Lock on", model.ErrVaultRequirements)
	}
	if !model.IsPlatformConnection(primaryID) {
		primary, checked, err := s.deps.Posture.Posture(ctx, p.TenantID, primaryID)
		if err != nil {
			return err
		}
		if checked && primary.OwnerAccount != "" && primary.OwnerAccount == replica.OwnerAccount {
			return fmt.Errorf("%w: the replica's bucket is in the same account (%s) as the dataset's own", model.ErrVaultRequirements,
				replica.OwnerAccount)
		}
	}
	return nil
}
