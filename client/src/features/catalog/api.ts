import { api, apiRaw } from "@/lib/api/client";
import type {
  AssetLifecycle,
  CatalogCapabilities,
  CatalogObject,
  CatalogPage,
  ConnectionName,
  DeleteObjectResponse,
  DownloadMode,
  DownloadUrl,
  ObjectVersion,
  VersionsPage,
} from "./types";

export const PAGE_SIZE = 50;
export const VERSION_PAGE_SIZE = 25;

const object = (id: string) => `/catalog/objects/${encodeURIComponent(id)}`;

// The query is built here, not through the client's query option, because
// that option drops empty values and dataset= (empty) means "root only".
export function listObjects(params: URLSearchParams, cursor: string | undefined, signal?: AbortSignal) {
  const search = new URLSearchParams(params);
  search.set("limit", String(PAGE_SIZE));
  if (cursor) search.set("cursor", cursor);
  return api<CatalogPage>(`/catalog/objects?${search.toString()}`, { signal });
}

export const getObject = (id: string) => api<CatalogObject>(object(id));

export function lookupObject(dataset: string, path: string) {
  return api<CatalogObject>(`/catalog/lookup?${new URLSearchParams({ dataset, path }).toString()}`);
}

export const getCapabilities = () => api<CatalogCapabilities>("/catalog/capabilities");

export function listVersions(id: string, before: number | undefined, signal?: AbortSignal) {
  return api<VersionsPage>(`${object(id)}/versions`, { query: { limit: VERSION_PAGE_SIZE, before }, signal });
}

export const getVersion = (id: string, version: number | "current") =>
  api<ObjectVersion>(`${object(id)}/versions/${version}`);

export const patchMetadata = (id: string, revision: number, metadata: Record<string, string | null>) =>
  api<CatalogObject>(object(id), { method: "PATCH", body: { revision, metadata } });

export const renameObject = (id: string, revision: number, path: string) =>
  api<CatalogObject>(`${object(id)}/rename`, { method: "POST", body: { revision, path } });

export const setVersioning = (id: string, revision: number, versioning: "ENABLED" | "DISABLED") =>
  api<CatalogObject>(`${object(id)}/versioning`, { method: "PUT", body: { revision, versioning } });

export const promoteVersion = (id: string, revision: number, version: number) =>
  api<CatalogObject>(`${object(id)}/versions/${version}/promote`, { method: "POST", body: { revision } });

export const deleteVersion = (id: string, version: number) =>
  api<ObjectVersion>(`${object(id)}/versions/${version}`, { method: "DELETE" });

export const deleteObject = (id: string) => api<DeleteObjectResponse>(object(id), { method: "DELETE" });

export const getLifecycle = (assetId: string) =>
  api<AssetLifecycle>(`/assets/${encodeURIComponent(assetId)}/lifecycle`);

export const createDownloadUrl = (assetId: string, mode: DownloadMode, filename?: string) =>
  api<DownloadUrl>(`/assets/${encodeURIComponent(assetId)}/download-url`, {
    method: "POST",
    body: { mode, filename: filename || undefined },
  });

export const revokeDownloadLinks = (assetId: string) =>
  api<{ revoked: number }>(`/assets/${encodeURIComponent(assetId)}/download-links`, { method: "DELETE" });

export const fetchContent = (assetId: string, signal?: AbortSignal) =>
  apiRaw(`/assets/${encodeURIComponent(assetId)}/content`, { signal, timeoutMs: 120_000 });

export async function listConnectionNames(): Promise<ConnectionName[]> {
  const body = await api<ConnectionName[] | { connections?: ConnectionName[] }>("/storage-connections");
  return Array.isArray(body) ? body : (body.connections ?? []);
}
