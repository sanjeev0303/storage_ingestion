import type { Dataset } from "./types";

export function makeDataset(over: Partial<Dataset> = {}): Dataset {
  return {
    id: "ds-1",
    name: "pets",
    completion_policy: "ALL",
    status: "OPEN",
    lifecycle_status: "DRAFT",
    max_bytes: 0,
    reserved_bytes: 0,
    stored_bytes: 0,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-02T00:00:00Z",
    description: "",
    tags: [],
    metadata: {},
    revision: 3,
    member_count: 4,
    created_by: "user-1",
    ...over,
  };
}
