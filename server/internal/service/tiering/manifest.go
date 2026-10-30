package tiering

import (
	"context"

	"github.com/sanjeev0303/object-storage-platform/internal/infrastructure/database/postgres"
	"github.com/sanjeev0303/object-storage-platform/internal/model"
	"github.com/sanjeev0303/object-storage-platform/internal/service/audit"
)

// ManifestLine is one member of a storage manifest: where its bytes are
// on the organization's own storage, for tools that read storage directly
// (an Iceberg table registering the snapshot's Parquet files, Spark,
// DuckDB). URI is empty for bytes the platform keeps on its own storage
// or in a pack: those are read through the API.
type ManifestLine struct {
	Path      string            `json:"path"`
	URI       string            `json:"uri,omitempty"`
	VersionID string            `json:"storage_version_id,omitempty"`
	Size      int64             `json:"size_bytes"`
	SHA256    string            `json:"sha256"`
	Tier      model.StorageTier `json:"tier"`
	AssetID   string            `json:"asset_id"`
}

type ManifestPage struct {
	Lines []ManifestLine `json:"lines"`
	After string         `json:"next_after,omitempty"`
}

var schemes = map[model.StorageProviderType]string{
	model.ProviderAWSS3: "s3", model.ProviderS3Compatible: "s3", model.ProviderR2: "s3",
	model.ProviderGCS: "gs", model.ProviderAzureBlob: "az",
}

// StorageManifest pages a snapshot's members with their storage URIs
// (docs/tiering.md). It needs storage.read as well as datasets.read: it
// names buckets and keys.
func (s *Service) StorageManifest(ctx context.Context, p model.Principal, datasetID, versionID, after string, limit int) (ManifestPage, error) {
	if err := audit.Require(ctx, s.deps.Audit, p, model.PermStorageRead, "dataset.storage_manifest", "dataset_version", versionID); err != nil {
		return ManifestPage{}, err
	}
	if _, err := s.deps.Datasets.Version(ctx, p, datasetID, versionID); err != nil {
		return ManifestPage{}, err
	}
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	objects, err := s.deps.Store.VersionObjects(ctx, p.TenantID, versionID, after, limit)
	if err != nil {
		return ManifestPage{}, err
	}
	buckets := map[string]string{}
	out := ManifestPage{Lines: make([]ManifestLine, 0, len(objects))}
	for _, o := range objects {
		line := ManifestLine{Path: o.Path, VersionID: o.StorageVersionID, Size: o.Size, SHA256: o.SHA256, Tier: o.Tier, AssetID: o.AssetID}
		if !o.Packed && o.ConnectionID != model.PlatformConnectionID {
			if uri, ok := s.uri(ctx, p.TenantID, o, buckets); ok {
				line.URI = uri
			}
		}
		out.Lines = append(out.Lines, line)
	}
	if len(objects) == limit {
		out.After = objects[len(objects)-1].Path
	}
	return out, nil
}

func (s *Service) uri(ctx context.Context, tenantID string, o postgres.VersionObject, buckets map[string]string) (string, bool) {
	provider, err := s.deps.Store.ProviderOf(ctx, tenantID, o.ConnectionID)
	scheme, ok := schemes[provider]
	if err != nil || !ok {
		return "", false
	}
	bucket, seen := buckets[o.ConnectionID]
	if !seen {
		loc, err := s.deps.Router.Locate(ctx, tenantID, o.ConnectionID)
		if err != nil {
			return "", false
		}
		bucket = loc.ReadyBucket
		buckets[o.ConnectionID] = bucket
	}
	return scheme + "://" + bucket + "/" + o.ObjectKey, true
}
