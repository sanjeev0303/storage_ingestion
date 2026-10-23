import type { UploadRequest } from "./types";
import { parseMetadata, validateDataset, validatePath } from "./validation";

// What the person typed for one file, before it becomes an upload request.
export type FileDraft = {
  dataset: string;
  path: string;
  metadataText: string;
  createOnly: boolean;
  connectionId: string;
};

export type DraftErrors = { dataset?: string; path?: string; metadata?: string; size?: string };

export function newDraft(connectionId: string): FileDraft {
  return { dataset: "", path: "", metadataText: "", createOnly: false, connectionId };
}

export function draftFromRequest(request: UploadRequest, connectionId: string): FileDraft {
  return {
    dataset: request.dataset ?? "",
    path: request.path ?? "",
    metadataText: Object.entries(request.metadata ?? {})
      .map(([key, value]) => `${key}=${value}`)
      .join("\n"),
    createOnly: Boolean(request.create_only),
    connectionId: request.storage_connection_id ?? connectionId,
  };
}

export function draftErrors(draft: FileDraft, file: { name: string; size: number }, maxObjectBytes?: number): DraftErrors {
  const errors: DraftErrors = {};
  const dataset = validateDataset(draft.dataset.trim());
  if (dataset) errors.dataset = dataset;

  const path = validatePath(draft.path.trim() || file.name);
  if (path) errors.path = draft.path.trim() ? path : `${path} The file name is used when no path is set, so enter a path.`;

  const metadata = parseMetadata(draft.metadataText).error;
  if (metadata) errors.metadata = metadata;

  if (file.size === 0) errors.size = "This file is empty. The platform does not accept empty files.";
  if (maxObjectBytes && maxObjectBytes > 0 && file.size > maxObjectBytes) {
    errors.size = "This file is larger than your organization's maximum object size.";
  }
  return errors;
}

export function hasErrors(errors: DraftErrors) {
  return Object.keys(errors).length > 0;
}

// The default connection is left out of the request so the backend resolves it.
export function toRequest(file: File, draft: FileDraft, defaultConnectionId: string): UploadRequest {
  const dataset = draft.dataset.trim();
  const path = draft.path.trim();
  const metadata = parseMetadata(draft.metadataText).value;

  return {
    file_name: file.name,
    content_type: file.type || "application/octet-stream",
    dataset: dataset || undefined,
    path: path || undefined,
    metadata: Object.keys(metadata).length > 0 ? metadata : undefined,
    create_only: draft.createOnly || undefined,
    storage_connection_id: draft.connectionId && draft.connectionId !== defaultConnectionId ? draft.connectionId : undefined,
  };
}
