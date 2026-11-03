import { api } from "@/lib/api/client";

// Mirrors the domain routes (docs/domain.md).

export type RecordingMeta = {
  asset_id: string;
  path: string;
  kind: "MCAP" | "ROSBAG2";
  start_ns: number;
  end_ns: number;
  message_count: number;
  topics: { topic: string; schema?: string; encoding?: string; message_count: number }[];
};

export type Segment = {
  id: string;
  path: string;
  start_ns: number;
  end_ns: number;
  label: string;
  note: string;
  created_at: string;
};

export type EmbeddingIndex = {
  id: string;
  name: string;
  model: string;
  dimensions: number;
  metric: "COSINE" | "DOT" | "EUCLIDEAN";
  vectors: number;
  created_at: string;
};

export type UpstreamSource = {
  url: string;
  path: string;
  sha256: string;
  state: "WATCHING" | "CHANGED" | "GONE";
  note?: string;
  checked_at?: string;
  changed_at?: string;
};

export type Creator = { name: string; affiliation?: string; orcid?: string };

export type Publication = {
  id: string;
  slug: string;
  title: string;
  license: string;
  doi?: string;
  doi_error?: string;
  withdrawn_at?: string;
  withdrawn_reason?: string;
  created_at: string;
};

export type PublishedResponse = { publication: Publication; landing_url: string };

export type RetentionEstimate = {
  total_bytes: number;
  years: number;
  tiers: { tier: string; price_per_gb_month: number; per_year: number; total: number }[];
  note: string;
};

// What anyone may read on a landing page.
export type PublicRecord = {
  slug: string;
  title: string;
  description: string;
  creators: Creator[];
  publisher: string;
  publication_year: number;
  license: string;
  version: number;
  manifest_sha256: string;
  member_count: number;
  total_bytes: number;
  doi?: string;
  published_at: string;
  withdrawn_at?: string;
  withdrawn_reason?: string;
  landing_url: string;
  citation: string;
  bibtex: string;
  ris: string;
  datacite: object;
  schema_org: object;
};

const versionPath = (id: string, versionId: string) => `/datasets/${encodeURIComponent(id)}/versions/${encodeURIComponent(versionId)}`;

export const listRecordings = (id: string, versionId: string, signal?: AbortSignal) =>
  api<{ recordings: RecordingMeta[] }>(`${versionPath(id, versionId)}/recordings`, { signal });

export const listSegments = (id: string, versionId: string, signal?: AbortSignal) =>
  api<{ segments: Segment[] }>(`${versionPath(id, versionId)}/segments`, { signal });

export const addSegment = (id: string, versionId: string, body: { path: string; start_ns: number; end_ns: number; label: string; note: string }) =>
  api<{ segments: Segment[] }>(`${versionPath(id, versionId)}/segments`, { method: "POST", body: { segments: [body] } });

export const deleteSegment = (id: string, versionId: string, segmentId: string) =>
  api<void>(`${versionPath(id, versionId)}/segments/${encodeURIComponent(segmentId)}`, { method: "DELETE" });

export const listIndexes = (id: string, versionId: string, signal?: AbortSignal) =>
  api<{ indexes: EmbeddingIndex[] }>(`${versionPath(id, versionId)}/embedding-indexes`, { signal });

export const createIndex = (id: string, versionId: string, body: { name: string; model: string; dimensions: number; metric: string }) =>
  api<EmbeddingIndex>(`${versionPath(id, versionId)}/embedding-indexes`, { method: "POST", body });

export const deleteIndex = (indexId: string) => api<void>(`/embedding-indexes/${encodeURIComponent(indexId)}`, { method: "DELETE" });

export const listUpstream = (datasetId: string, signal?: AbortSignal) =>
  api<{ sources: UpstreamSource[] }>(`/upstream-sources?dataset_id=${encodeURIComponent(datasetId)}`, { signal });

export const getPublication = (id: string, versionId: string, signal?: AbortSignal) =>
  api<PublishedResponse>(`${versionPath(id, versionId)}/publication`, { signal });

export const publish = (
  id: string,
  versionId: string,
  body: { title: string; description: string; creators: Creator[]; publisher: string; license: string; mint_doi: boolean },
) => api<PublishedResponse>(`${versionPath(id, versionId)}/publish`, { method: "POST", body });

export const mintDOI = (id: string, versionId: string) => api<PublishedResponse>(`${versionPath(id, versionId)}/publication/doi`, { method: "POST", body: {} });

export const withdraw = (id: string, versionId: string, reason: string) =>
  api<PublishedResponse>(`${versionPath(id, versionId)}/publication/withdraw`, { method: "POST", body: { reason } });

export const retentionCost = (id: string, versionId: string, years: number, signal?: AbortSignal) =>
  api<RetentionEstimate>(`${versionPath(id, versionId)}/retention-cost?years=${years}`, { signal });

export const stacCollection = (id: string, versionId: string) => api<object>(`${versionPath(id, versionId)}/stac/collection.json`);

export const stacItems = (id: string, versionId: string) => api<object>(`${versionPath(id, versionId)}/stac/items.json`);
