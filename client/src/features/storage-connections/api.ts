import { api } from "@/lib/api/client";
import { isApiError } from "@/lib/api/errors";
import type {
  CapabilityMatrix,
  ConnectionList,
  CreateConnectionRequest,
  ProviderCatalog,
  StorageConnection,
  StorageCredentials,
  TestResponse,
  UpdateConnectionRequest,
} from "./types";

const base = "/storage-connections";

export const listConnections = () => api<ConnectionList>(base);
export const getConnection = (id: string) => api<StorageConnection>(`${base}/${id}`);
export const listProviders = () => api<ProviderCatalog>("/storage-providers");
export const getCapabilities = () => api<CapabilityMatrix>("/storage-providers/capabilities");

export const createConnection = (body: CreateConnectionRequest) =>
  api<StorageConnection>(base, { method: "POST", body });

// The revision goes in the body only. Sending it in the body and in If-Match
// makes the backend answer 500 when the two differ.
export const updateConnection = (id: string, body: UpdateConnectionRequest) =>
  api<StorageConnection>(`${base}/${id}`, { method: "PATCH", body });

export const testConnection = (id: string, writeProbe: boolean) =>
  api<TestResponse>(`${base}/${id}/test`, { method: "POST", body: writeProbe ? { write_probe: true } : {} });

export const rotateCredentials = (id: string, revision: number, credentials: StorageCredentials) =>
  api<TestResponse>(`${base}/${id}/rotate-credentials`, { method: "POST", body: { revision, credentials } });

export const setDefaultConnection = (id: string) =>
  api<StorageConnection>(`${base}/${id}/set-default`, { method: "POST" });

export const disableConnection = (id: string) =>
  api<StorageConnection>(`${base}/${id}/disable`, { method: "POST" });

export const enableConnection = (id: string) => api<TestResponse>(`${base}/${id}/enable`, { method: "POST" });

export const deleteConnection = (id: string) => api<void>(`${base}/${id}`, { method: "DELETE" });

// A refused edit or rotation answers 422 with the test result as the body.
export function testResultFromError(error: unknown): TestResponse | null {
  if (!isApiError(error) || error.status !== 422) return null;
  const body = error.body as Partial<TestResponse> | undefined;
  if (!body || typeof body !== "object" || !Array.isArray(body.checks)) return null;
  return body as TestResponse;
}
