package storageconn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// DriftStore keeps what the drift scan reads and finds
// (postgres.StorageConnectionRepository).
type DriftStore interface {
	StoredObjects(ctx context.Context, tenantID, connectionID, after string, limit int) ([]postgres.StoredObject, error)
	KnownKeys(ctx context.Context, tenantID, connectionID string, keys []string) (map[string]bool, error)
	AddDrift(ctx context.Context, f model.DriftFinding) (bool, error)
	Drift(ctx context.Context, tenantID, connectionID string, open bool, limit int) ([]model.DriftFinding, error)
	ResolveDrift(ctx context.Context, tenantID, id, actor string, at time.Time) (model.DriftFinding, error)
	VersionDrift(ctx context.Context, tenantID, versionID string, limit int) ([]model.DriftFinding, []string, error)
	DriftDue(ctx context.Context, before time.Time, limit int) ([]model.StorageConnection, error)
	DriftCursor(ctx context.Context, connectionID string) (string, string, error)
	SaveDriftCursor(ctx context.Context, tenantID, connectionID, objects, listing string, checked int, at time.Time) error
}

// DriftStats counts one scheduled pass.
type DriftStats struct {
	Connections, Checked, Found, Errors int
}

// ScanDrift checks, for each connection due, the next objects the
// platform keeps there against what it recorded when it wrote them, and
// the next page of its prefix for objects it never wrote. budget bounds
// the objects one connection is checked for per pass; cursors carry the
// scan on next time, so every object is reached in turn.
func (s *Service) ScanDrift(ctx context.Context, interval time.Duration, connections, budget int) (DriftStats, error) {
	var stats DriftStats
	if s.deps.Drift == nil || s.enabled() != nil {
		return stats, nil
	}
	due, err := s.deps.Drift.DriftDue(ctx, s.now().Add(-interval), connections)
	if err != nil {
		return stats, err
	}
	for _, c := range due {
		checked, found, err := s.scanConnection(ctx, c, budget)
		stats.Connections++
		stats.Checked += checked
		stats.Found += found
		if err != nil {
			stats.Errors++
			s.deps.Log.Warn("drift scan stopped", "connection_id", c.ID, "category", Categorize(err))
		}
	}
	return stats, nil
}

func (s *Service) scanConnection(ctx context.Context, c model.StorageConnection, budget int) (int, int, error) {
	client, closeClient, err := s.deps.Clients.BuildStored(ctx, c)
	if err != nil {
		return 0, 0, err
	}
	defer closeClient()
	objectCursor, listingCursor, err := s.deps.Drift.DriftCursor(ctx, c.ID)
	if err != nil {
		return 0, 0, err
	}
	checked, found := 0, 0
	record := func(f model.DriftFinding) error {
		f.ID, f.TenantID, f.ConnectionID, f.DetectedAt = s.newID(), c.TenantID, c.ID, s.now()
		created, err := s.deps.Drift.AddDrift(ctx, f)
		if created {
			found++
		}
		return err
	}

	// What the platform wrote, against what it recorded.
	objects, err := s.deps.Drift.StoredObjects(ctx, c.TenantID, c.ID, objectCursor, budget)
	if err != nil {
		return checked, found, err
	}
	var scanErr error
	for _, o := range objects {
		kind, detail, err := examine(ctx, client, c.Bucket, o)
		if err != nil {
			scanErr = err
			break
		}
		checked++
		objectCursor = o.Key
		if kind != "" {
			if err := record(model.DriftFinding{AssetID: o.AssetID, ObjectKey: o.Key, Kind: kind, Detail: detail}); err != nil {
				return checked, found, err
			}
		}
	}
	if scanErr == nil && len(objects) < budget {
		objectCursor = "" // the pass is over; the next starts again
	}

	// What is under the platform's prefix that it never wrote.
	if scanErr == nil {
		prefix := c.ReadyPrefix + "/" + c.TenantID + "/"
		page, err := client.ListObjectsPage(ctx, c.Bucket, prefix, listingCursor, min(budget, 1000))
		if err != nil {
			scanErr = err
		} else {
			listingCursor = page.Cursor
			var keys []string
			sizes := map[string]interfaces.StoredObject{}
			for _, o := range page.Objects {
				name := strings.TrimPrefix(o.ObjectKey, prefix)
				if strings.HasPrefix(name, ".") {
					continue
				}
				keys = append(keys, o.ObjectKey)
				sizes[o.ObjectKey] = o
			}
			if len(keys) > 0 {
				known, err := s.deps.Drift.KnownKeys(ctx, c.TenantID, c.ID, keys)
				if err != nil {
					return checked, found, err
				}
				for _, k := range keys {
					if known[k] {
						continue
					}
					o := sizes[k]
					detail := fmt.Sprintf("an object the platform did not write appeared under its prefix (%d bytes, modified %s)",
						o.SizeBytes, o.LastModified.UTC().Format(time.RFC3339))
					if err := record(model.DriftFinding{ObjectKey: k, Kind: model.DriftUnknown, Detail: detail}); err != nil {
						return checked, found, err
					}
				}
			}
		}
	}

	if err := s.deps.Drift.SaveDriftCursor(ctx, c.TenantID, c.ID, objectCursor, listingCursor, checked, s.now()); err != nil {
		return checked, found, err
	}
	if found > 0 && s.deps.Alerts != nil {
		_, err := s.deps.Alerts.AddAlert(ctx, model.SecurityAlert{
			ID: s.newID(), TenantID: c.TenantID, PrincipalID: "connection:" + c.ID, Kind: model.AlertOutOfBand,
			Detail:  fmt.Sprintf("%d objects on connection %s (%s) changed or appeared outside the platform", found, c.Name, c.Bucket),
			Metrics: map[string]any{"connection_id": c.ID, "findings": found}, State: "OPEN",
			DedupKey: "drift:" + c.ID + ":" + s.now().Format("2006-01-02T15"), CreatedAt: s.now(),
		})
		if err != nil {
			s.deps.Log.Warn("drift alert not recorded", "connection_id", c.ID, "error", err)
		}
	}
	return checked, found, scanErr
}

func sameETag(a, b string) bool { return strings.Trim(a, `"`) == strings.Trim(b, `"`) }

// examine compares one stored object with what the platform recorded. An
// error is the provider failing to answer, which says nothing about the
// object; a refused request (S3's answer to a HEAD of an object re-
// encrypted with a customer key) is a finding.
func examine(ctx context.Context, client interfaces.StorageProvider, bucket string, o postgres.StoredObject) (string, string, error) {
	what := "the object"
	if o.PackID != "" {
		what = "the pack " + o.PackID
	}
	unreadable := func(err error) bool { return errors.Is(err, interfaces.ErrStorageBadRequest) }
	if o.VersionID != "" {
		_, err := client.HeadObjectVersion(ctx, bucket, o.Key, o.VersionID)
		switch {
		case errors.Is(err, interfaces.ErrObjectNotFound):
			return model.DriftDeleted, "the version the platform pinned was deleted outside it: snapshots holding " + what + " cannot be read", nil
		case unreadable(err):
			return model.DriftUnreadable, what + " can no longer be read without a key the platform does not have (re-encrypted with SSE-C?)", nil
		case err != nil:
			return "", "", err
		}
		latest, err := client.HeadObject(ctx, bucket, o.Key)
		switch {
		case errors.Is(err, interfaces.ErrObjectNotFound):
			return model.DriftDeleted, what + " was deleted outside the platform (a delete marker); the pinned version is intact", nil
		case unreadable(err):
			return model.DriftNewVersion, "a version encrypted with a key the platform does not have was written over " + what +
				"; the pinned version is intact", nil
		case err != nil:
			return "", "", err
		case latest.VersionID != "" && latest.VersionID != o.VersionID:
			return model.DriftNewVersion, "a newer version was written over " + what + " outside the platform; the pinned version is intact", nil
		}
		return "", "", nil
	}
	meta, err := client.HeadObject(ctx, bucket, o.Key)
	switch {
	case errors.Is(err, interfaces.ErrObjectNotFound):
		return model.DriftDeleted, what + " was deleted outside the platform", nil
	case unreadable(err):
		return model.DriftUnreadable, what + " can no longer be read without a key the platform does not have (re-encrypted with SSE-C?)", nil
	case err != nil:
		return "", "", err
	case meta.SizeBytes != o.SizeBytes:
		return model.DriftOverwritten, fmt.Sprintf("%s was replaced outside the platform: %d bytes recorded, %d stored", what, o.SizeBytes, meta.SizeBytes), nil
	case o.ETag != "" && !sameETag(meta.ETag, o.ETag):
		return model.DriftOverwritten, what + " was replaced outside the platform: its ETag changed", nil
	}
	return "", "", nil
}

// DriftFor lists a connection's findings (open ones unless all).
func (s *Service) DriftFor(ctx context.Context, p model.Principal, connectionID string, all bool) ([]model.DriftFinding, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageRead, "storage_connection.drift.read", "storage_connection", connectionID); err != nil {
		return nil, err
	}
	if s.deps.Drift == nil {
		return nil, ErrPostureOff
	}
	if connectionID != "" {
		if _, err := s.ownConnection(ctx, p, connectionID); err != nil {
			return nil, err
		}
	}
	return s.deps.Drift.Drift(ctx, p.TenantID, connectionID, !all, 1000)
}

// ResolveDriftFor closes a finding someone has looked into.
func (s *Service) ResolveDriftFor(ctx context.Context, p model.Principal, findingID string) (model.DriftFinding, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageManage, "storage_connection.drift.resolve", "drift_finding", findingID); err != nil {
		return model.DriftFinding{}, err
	}
	if s.deps.Drift == nil {
		return model.DriftFinding{}, ErrPostureOff
	}
	return s.deps.Drift.ResolveDrift(ctx, p.TenantID, findingID, p.UserID, s.now())
}

// VersionIntegrity is whether a snapshot's members changed outside the
// platform. Object keys stay inside the service.
type VersionIntegrity struct {
	Degraded bool             `json:"degraded"`
	Objects  []DegradedMember `json:"objects"`
}

type DegradedMember struct {
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Detail     string    `json:"detail"`
	DetectedAt time.Time `json:"detected_at"`
}

// LocationStore groups a snapshot's members by where they are stored.
type LocationStore interface {
	VersionLocations(ctx context.Context, tenantID, versionID string) ([]postgres.LocatedMembers, error)
	DatasetRulesByID(ctx context.Context, tenantID, datasetID string) (model.PlacementRules, error)
}

// Residency is where a snapshot's bytes are, against its dataset's rules.
type Residency struct {
	Allowed            []string        `json:"allowed"`
	RequireCustomerKey bool            `json:"require_customer_key"`
	Inside             bool            `json:"inside"`
	Locations          []LocationShare `json:"locations"`
}

type LocationShare struct {
	ConnectionID     string `json:"connection_id"`
	Location         string `json:"location"`
	LocationVerified bool   `json:"location_verified"`
	EncryptionKey    string `json:"encryption_key,omitempty"`
	Members          int64  `json:"members"`
	Bytes            int64  `json:"bytes"`
	Allowed          bool   `json:"allowed"`
}

// ResidencyOf reports where a snapshot's members are stored, each
// location as the provider reported it, and whether all of them are
// inside what the dataset allows.
func (s *Service) ResidencyOf(ctx context.Context, p model.Principal, datasetID, versionID string) (Residency, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead, "dataset_version.residency", "dataset_version", versionID); err != nil {
		return Residency{}, err
	}
	out := Residency{Allowed: []string{}, Locations: []LocationShare{}, Inside: true}
	store, ok := s.deps.Drift.(LocationStore)
	if !ok {
		return out, ErrPostureOff
	}
	rules, err := store.DatasetRulesByID(ctx, p.TenantID, datasetID)
	if err != nil {
		return out, err
	}
	out.Allowed, out.RequireCustomerKey = append(out.Allowed, rules.Residency...), rules.RequireCustomerKey
	located, err := store.VersionLocations(ctx, p.TenantID, versionID)
	if err != nil {
		return out, err
	}
	for _, l := range located {
		platform := model.IsPlatformConnection(l.ConnectionID)
		share := LocationShare{ConnectionID: l.ConnectionID, Location: l.Location, LocationVerified: l.LocationVerified || platform,
			EncryptionKey: l.EncryptionKey, Members: l.Members, Bytes: l.Bytes}
		share.Allowed = (len(rules.Residency) == 0 || (share.LocationVerified && model.LocationAllowed(l.Location, rules.Residency))) &&
			(!rules.RequireCustomerKey || l.EncryptionKey != "")
		out.Inside = out.Inside && share.Allowed
		out.Locations = append(out.Locations, share)
	}
	return out, nil
}

// IntegrityOf reports the open drift findings that touch a snapshot.
func (s *Service) IntegrityOf(ctx context.Context, p model.Principal, versionID string) (VersionIntegrity, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermDatasetsRead, "dataset_version.integrity", "dataset_version", versionID); err != nil {
		return VersionIntegrity{}, err
	}
	out := VersionIntegrity{Objects: []DegradedMember{}}
	if s.deps.Drift == nil {
		return out, nil
	}
	findings, paths, err := s.deps.Drift.VersionDrift(ctx, p.TenantID, versionID, 1000)
	if err != nil {
		return out, err
	}
	for i, f := range findings {
		out.Objects = append(out.Objects, DegradedMember{Path: paths[i], Kind: f.Kind, Detail: f.Detail, DetectedAt: f.DetectedAt})
	}
	out.Degraded = len(out.Objects) > 0
	return out, nil
}
