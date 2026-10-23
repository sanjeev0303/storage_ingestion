import type { CatalogObject, ObjectVersion } from "./types";

export const sha = "a".repeat(64);

export function makeObject(overrides: Partial<CatalogObject> = {}): CatalogObject {
  return {
    id: "obj-1",
    dataset: "images",
    path: "2026/cat.png",
    versioning: "ENABLED",
    revision: 3,
    metadata: { stage: "draft" },
    current_version: {
      version: 2,
      asset_id: "asset-2",
      size_bytes: 2048,
      content_type: "image/png",
      checksum_sha256: sha,
      published_at: "2026-02-01T10:00:00Z",
      storage_connection_id: "platform",
    },
    versions: 2,
    created_at: "2026-01-01T10:00:00Z",
    updated_at: "2026-02-01T10:00:00Z",
    ...overrides,
  };
}

export function makeVersion(overrides: Partial<ObjectVersion> = {}): ObjectVersion {
  return {
    version: 2,
    asset_id: "asset-2",
    status: "AVAILABLE",
    current: true,
    file_name: "cat.png",
    size_bytes: 2048,
    content_type: "image/png",
    checksum_algorithm: "sha256",
    checksum_sha256: sha,
    provider_etag: '"etag-1"',
    published_at: "2026-02-01T10:00:00Z",
    created_at: "2026-02-01T09:59:00Z",
    ...overrides,
  };
}
