// Permission strings the backend checks. The UI uses them to decide what to
// show; the backend stays the authority and may still answer 403.
export const PERMISSIONS = [
  "org.read",
  "org.manage",
  "members.read",
  "members.manage",
  "roles.manage",
  "datasets.read",
  "datasets.write",
  "datasets.manage",
  "ingestion.read",
  "ingestion.write",
  "ingestion.manage",
  "uploads.write",
  "assets.read",
  "assets.download",
  "assets.delete",
  "quarantine.read",
  "quarantine.release",
  "retention.manage",
  "quota.read",
  "audit.read",
  "catalog.write",
  "org.ownership",
  "storage.read",
  "storage.use",
  "storage.manage",
] as const;

export type Permission = (typeof PERMISSIONS)[number];

// A navigation entry is shown when the user holds any of its permissions.
// Unknown permissions (roles could not be read) show everything.
export const NAV_PERMISSIONS: Record<string, Permission[]> = {
  overview: [],
  storage: ["storage.read"],
  ingestion: ["ingestion.read", "uploads.write"],
  catalog: ["assets.read"],
  datasets: ["datasets.read"],
  validation: ["datasets.read"],
  operations: ["assets.read", "quarantine.read", "retention.manage"],
  audit: ["audit.read"],
  settings: ["org.read"],
};
