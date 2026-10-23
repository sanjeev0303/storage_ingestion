export const MAX_METADATA_FILTERS = 4;

export type MetadataFilter = { key: string; value: string };

// Mirrors GET /catalog/objects. dataset undefined means every namespace,
// an empty string means the root namespace only.
export type CatalogFilters = {
  dataset?: string;
  prefix: string;
  content_type: string;
  min_size: string;
  max_size: string;
  created_after: string;
  created_before: string;
  updated_after: string;
  updated_before: string;
  metadata: MetadataFilter[];
  include_unpublished: boolean;
  order: "path" | "updated";
  object_id: string;
  asset_id: string;
};

const TEXT_FIELDS = [
  "prefix",
  "content_type",
  "min_size",
  "max_size",
  "created_after",
  "created_before",
  "updated_after",
  "updated_before",
  "object_id",
  "asset_id",
] as const;

const DATE_FIELDS = ["created_after", "created_before", "updated_after", "updated_before"] as const;

export const emptyFilters: CatalogFilters = {
  dataset: undefined,
  prefix: "",
  content_type: "",
  min_size: "",
  max_size: "",
  created_after: "",
  created_before: "",
  updated_after: "",
  updated_before: "",
  metadata: [],
  include_unpublished: false,
  order: "path",
  object_id: "",
  asset_id: "",
};

export function parseFilters(params: URLSearchParams): CatalogFilters {
  const filters: CatalogFilters = { ...emptyFilters, metadata: [] };
  if (params.has("dataset")) filters.dataset = params.get("dataset") ?? "";
  for (const field of TEXT_FIELDS) filters[field] = params.get(field) ?? "";
  filters.include_unpublished = params.get("include") === "unpublished";
  filters.order = params.get("order") === "updated" ? "updated" : "path";
  for (const [name, value] of params.entries()) {
    if (name.startsWith("metadata.") && filters.metadata.length < MAX_METADATA_FILTERS) {
      filters.metadata.push({ key: name.slice("metadata.".length), value });
    }
  }
  return filters;
}

// The query string used both in the page URL and for the API call.
export function filtersToParams(filters: CatalogFilters): URLSearchParams {
  const params = new URLSearchParams();
  if (filters.dataset !== undefined) params.set("dataset", filters.dataset);
  for (const field of TEXT_FIELDS) {
    const value = filters[field].trim();
    if (value) params.set(field, value);
  }
  for (const item of filters.metadata) {
    if (item.key.trim()) params.append(`metadata.${item.key.trim()}`, item.value);
  }
  if (filters.include_unpublished) params.set("include", "unpublished");
  if (filters.order === "updated") params.set("order", "updated");
  return params;
}

// The page keeps plain dates (from <input type="date">); the API wants RFC 3339.
export function toApiParams(params: URLSearchParams): URLSearchParams {
  const out = new URLSearchParams(params);
  for (const field of DATE_FIELDS) {
    const value = out.get(field);
    if (value && /^\d{4}-\d{2}-\d{2}$/.test(value)) out.set(field, `${value}T00:00:00Z`);
  }
  return out;
}

export function countActiveFilters(filters: CatalogFilters) {
  const params = filtersToParams(filters);
  params.delete("order");
  return Array.from(params.keys()).length;
}
