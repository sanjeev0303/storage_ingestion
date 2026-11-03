import { api } from "@/lib/api/client";

// Mirrors the chunk routes (docs/chunks.md).

export type ChunkSavings = {
  enabled: boolean;
  logical_bytes: number;
  unique_file_bytes: number;
  indexed_files: number;
  indexed_file_bytes: number;
  unique_chunk_bytes: number;
  unique_chunks: number;
  not_indexed_files: number;
  not_indexed_bytes: number;
  would_save_bytes: number;
};

export type ChunkStatus = {
  state: "NONE" | "PENDING" | "RUNNING" | "DONE" | "FAILED";
  files_indexed: number;
  bytes_read: number;
  skipped: { path: string; reason: string }[];
  error?: string;
};

export type ChunkStorageStats = {
  enabled: boolean;
  files: number;
  logical_bytes: number;
  retired_source_bytes: number;
  chunk_bytes: number;
  chunk_objects: number;
  rehydrating: number;
};

export type ChunkDelta = {
  total_bytes: number;
  unchanged_files: number;
  unchanged_bytes: number;
  changed_files: number;
  changed_bytes: number;
  reused_bytes: number;
  fetch_bytes: number;
  not_indexed_files: number;
  largest_changed: { path: string; size_bytes: number; reused_bytes: number; indexed: boolean }[];
};

const dataset = (id: string) => `/datasets/${encodeURIComponent(id)}`;
const version = (id: string, versionId: string) => `${dataset(id)}/versions/${encodeURIComponent(versionId)}`;

export const chunkSavings = (id: string, signal?: AbortSignal) => api<ChunkSavings>(`${dataset(id)}/chunk-savings`, { signal });

export const setChunking = (id: string, enabled: boolean) => api<{ enabled: boolean }>(`${dataset(id)}/chunking`, { method: "PUT", body: { enabled } });

export const chunkStatus = (id: string, versionId: string, signal?: AbortSignal) => api<ChunkStatus>(`${version(id, versionId)}/chunks/status`, { signal });

export const indexVersion = (id: string, versionId: string) => api<{ state: string }>(`${version(id, versionId)}/chunks/index`, { method: "POST", body: {} });

export const chunkStorage = (id: string, signal?: AbortSignal) => api<ChunkStorageStats>(`${dataset(id)}/chunk-storage`, { signal });

export const setChunkStorage = (id: string, enabled: boolean) => api<{ enabled: boolean }>(`${dataset(id)}/chunk-storage`, { method: "PUT", body: { enabled } });

export const rehydrate = (id: string) => api<{ files: number }>(`${dataset(id)}/chunk-storage/rehydrate`, { method: "POST", body: {} });

export const chunkDelta = (id: string, versionId: string, from: string, signal?: AbortSignal) =>
  api<ChunkDelta>(`${version(id, versionId)}/delta?from=${encodeURIComponent(from)}`, { signal });
