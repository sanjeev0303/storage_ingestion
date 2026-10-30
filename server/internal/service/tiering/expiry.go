package tiering

import (
	"context"
	"errors"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// VersionEstimate is what reading a snapshot would cost and wait for:
// downloading it, or one training epoch in the provider's region, and
// restoring what is archived first.
type VersionEstimate struct {
	Files       int64    `json:"files"`
	Bytes       int64    `json:"bytes"`
	PackedFiles int64    `json:"packed_files"`
	Packs       int64    `json:"packs"`
	Download    Estimate `json:"download"`
	Epoch       Estimate `json:"epoch"`
	// To restore first: archived files that cannot be read now.
	SealedFiles int64    `json:"archived_files"`
	SealedBytes int64    `json:"archived_bytes"`
	Restore     Estimate `json:"restore"`
}

func add(a, b Estimate) Estimate {
	a.Requests += b.Requests
	a.Bytes += b.Bytes
	a.StorageUSD, a.RequestUSD = cents(a.StorageUSD+b.StorageUSD), cents(a.RequestUSD+b.RequestUSD)
	a.EgressUSD, a.RetrievalUSD = cents(a.EgressUSD+b.EgressUSD), cents(a.RetrievalUSD+b.RetrievalUSD)
	a.TotalUSD = cents(a.TotalUSD + b.TotalUSD)
	a.Hours = max(a.Hours, b.Hours)
	if a.Note == "" {
		a.Note = b.Note
	}
	return a
}

func (s *Service) VersionEstimate(ctx context.Context, p model.Principal, datasetID, versionID string, speed model.RestoreSpeed) (VersionEstimate, error) {
	if _, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID); err != nil {
		return VersionEstimate{}, err
	}
	if speed == "" {
		speed = model.RestoreStandard
	}
	if !speed.Valid() {
		return VersionEstimate{}, invalid("speed is FAST, STANDARD or BULK")
	}
	profile, err := s.deps.Store.VersionProfile(ctx, p.TenantID, versionID, s.now())
	if err != nil {
		return VersionEstimate{}, err
	}
	out := VersionEstimate{Download: Estimate{Operation: "download"}, Epoch: Estimate{Operation: "epoch"}, Restore: Estimate{Operation: "restore"}}
	for _, v := range profile {
		book := model.PricesFor(v.Provider)
		out.Files += v.Files
		out.Bytes += v.Bytes
		out.PackedFiles += v.PackedFiles
		out.Packs += v.Packs
		out.Download = add(out.Download, estimate(EstimateInput{Operation: "download", Files: v.Files, Bytes: v.Bytes, Tier: v.Tier}, book))
		// A shard stream reads a pack per request, and each other file on its own.
		reads := v.Files - v.PackedFiles + v.Packs
		out.Epoch = add(out.Epoch, estimate(EstimateInput{Operation: "epoch", Files: v.Files, Requests: reads, Bytes: v.Bytes, Tier: v.Tier}, book))
		if model.ArchiveNeedsRestore(v.Provider, v.Tier) && v.SealedFiles > 0 {
			out.SealedFiles += v.SealedFiles
			out.SealedBytes += v.SealedBytes
			out.Restore = add(out.Restore, estimate(EstimateInput{Operation: "restore", Files: v.SealedFiles, Bytes: v.SealedBytes, Speed: speed}, book))
		}
	}
	return out, nil
}

// ExpiryInput chooses snapshots to expire: all but the newest KeepLast,
// and only those older than OlderThanDays.
type ExpiryInput struct {
	KeepLast      int  `json:"keep_last"`
	OlderThanDays int  `json:"older_than_days"`
	DryRun        bool `json:"dry_run"`
}

type ExpiringVersion struct {
	ID        string    `json:"id"`
	Version   int       `json:"version"`
	Members   int64     `json:"members"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
	// Kept says why a version stays; empty for one that expires.
	Kept string `json:"kept,omitempty"`
}

// Expiry is a snapshot expiry, previewed or done: the versions that go
// and those that stay (with why), and the space freed.
type Expiry struct {
	Expire []ExpiringVersion `json:"expire"`
	Keep   []ExpiringVersion `json:"keep"`
	// Freed: objects only the expiring versions hold, and their bytes.
	FreedObjects int64   `json:"freed_objects"`
	FreedBytes   int64   `json:"freed_bytes"`
	FreedUSD     float64 `json:"freed_usd_month"`
	DryRun       bool    `json:"dry_run"`
	// Done: deletions asked for, and objects whose deletion was refused
	// (a hold, retention or escrow), with why.
	DeletionsRequested int               `json:"deletions_requested"`
	Refused            map[string]string `json:"refused,omitempty"`
}

// ExpireVersions previews (DryRun) or carries out a snapshot expiry. The
// newest version, and any version still in use, always stays. Objects only
// expired versions held are deleted through the lifecycle, so legal
// holds, retention and escrow still apply, after its grace period.
func (s *Service) ExpireVersions(ctx context.Context, p model.Principal, datasetID string, in ExpiryInput) (Expiry, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsManage, "dataset.expire_versions", "dataset", datasetID); err != nil {
		return Expiry{}, err
	}
	if _, err := s.deps.Datasets.Get(ctx, p, datasetID); err != nil {
		return Expiry{}, err
	}
	if in.KeepLast < 1 || in.KeepLast > 10000 || in.OlderThanDays < 0 || in.OlderThanDays > 36500 {
		return Expiry{}, invalid("keep_last is at least 1, and older_than_days 0 to 36500")
	}
	uses, err := s.deps.Store.VersionUses(ctx, p.TenantID, datasetID)
	if err != nil {
		return Expiry{}, err
	}
	cutoff := s.now().AddDate(0, 0, -in.OlderThanDays)
	out := Expiry{Expire: []ExpiringVersion{}, Keep: []ExpiringVersion{}, DryRun: in.DryRun}
	var ids []string
	for i, u := range uses { // newest first
		v := ExpiringVersion{ID: u.ID, Version: u.Version, Members: u.Members, Bytes: u.Bytes, CreatedAt: u.CreatedAt, Kept: u.Reason}
		switch {
		case i < in.KeepLast:
			v.Kept = "one of the newest kept"
		case v.Kept == "" && u.CreatedAt.After(cutoff):
			v.Kept = "newer than the age asked for"
		}
		if v.Kept != "" {
			out.Keep = append(out.Keep, v)
			continue
		}
		out.Expire = append(out.Expire, v)
		ids = append(ids, u.ID)
	}
	if len(ids) == 0 {
		return out, nil
	}
	freed, err := s.deps.Store.Freed(ctx, p.TenantID, ids)
	if err != nil {
		return Expiry{}, err
	}
	stored := map[model.StorageProviderType]float64{}
	for _, a := range freed {
		out.FreedObjects++
		out.FreedBytes += a.SizeBytes
		provider, err := s.deps.Store.ProviderOf(ctx, p.TenantID, a.StorageConnectionID)
		if err == nil {
			stored[provider] += float64(a.SizeBytes) / gb * model.PricesFor(provider).StorageGBMonth[a.StorageTier]
		}
	}
	for _, usd := range stored {
		out.FreedUSD += usd
	}
	out.FreedUSD = cents(out.FreedUSD)
	if in.DryRun {
		return out, nil
	}

	if err := s.deps.Store.Expire(ctx, p.TenantID, ids, p.UserID, s.now()); err != nil {
		return Expiry{}, err
	}
	out.Refused = map[string]string{}
	for _, a := range freed {
		if s.deps.Delete == nil {
			break
		}
		if err := s.deps.Delete(ctx, p, a.ID); err != nil {
			if errors.Is(err, context.Canceled) {
				return out, err
			}
			out.Refused[a.ID] = err.Error()
			continue
		}
		out.DeletionsRequested++
	}
	s.record(ctx, p, "dataset.expire_versions", "dataset", datasetID, map[string]any{"versions": ids,
		"freed_objects": out.FreedObjects, "freed_bytes": out.FreedBytes, "deletions_requested": out.DeletionsRequested})
	return out, nil
}
