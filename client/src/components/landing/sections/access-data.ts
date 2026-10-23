import { PERMISSIONS, type Permission } from "@/lib/permissions";

export const BANDS: { name: string; permissions: { id: Permission; label: string }[] }[] = [
  {
    name: "Storage",
    permissions: [
      { id: "storage.read", label: "Read storage connections" },
      { id: "storage.use", label: "Use storage connections" },
      { id: "storage.manage", label: "Manage storage connections" },
    ],
  },
  {
    name: "Ingestion",
    permissions: [
      { id: "ingestion.read", label: "Read ingests" },
      { id: "ingestion.write", label: "Start ingests" },
      { id: "ingestion.manage", label: "Manage ingests" },
      { id: "uploads.write", label: "Upload files" },
    ],
  },
  {
    name: "Catalog",
    permissions: [
      { id: "assets.read", label: "Read catalog objects" },
      { id: "assets.download", label: "Download objects" },
      { id: "assets.delete", label: "Delete objects" },
      { id: "catalog.write", label: "Write catalog entries" },
      { id: "quarantine.read", label: "Read quarantine" },
      { id: "quarantine.release", label: "Release from quarantine" },
      { id: "retention.manage", label: "Manage retention" },
    ],
  },
  {
    name: "Datasets",
    permissions: [
      { id: "datasets.read", label: "Read datasets" },
      { id: "datasets.write", label: "Edit datasets" },
      { id: "datasets.manage", label: "Manage datasets" },
    ],
  },
  { name: "Audit", permissions: [{ id: "audit.read", label: "Read the audit log" }] },
  {
    name: "Organization",
    permissions: [
      { id: "org.read", label: "Read organization settings" },
      { id: "org.manage", label: "Manage organization settings" },
      { id: "members.read", label: "Read members" },
      { id: "members.manage", label: "Manage members" },
      { id: "roles.manage", label: "Manage roles" },
      { id: "quota.read", label: "Read usage and quota" },
      { id: "org.ownership", label: "Grant ownership" },
    ],
  },
];

// The built-in role sets, as the backend defines them.
export const ROLE_PERMISSIONS: Record<string, readonly Permission[]> = {
  Owner: PERMISSIONS,
  Administrator: PERMISSIONS.filter((p) => p !== "org.ownership"),
  Developer: [
    "org.read",
    "members.read",
    "datasets.read",
    "datasets.write",
    "ingestion.read",
    "ingestion.write",
    "ingestion.manage",
    "uploads.write",
    "assets.read",
    "assets.download",
    "assets.delete",
    "quarantine.read",
    "quota.read",
    "catalog.write",
    "storage.read",
    "storage.use",
  ],
  Auditor: [
    "org.read",
    "members.read",
    "datasets.read",
    "ingestion.read",
    "assets.read",
    "quarantine.read",
    "quota.read",
    "audit.read",
    "storage.read",
  ],
};
