import {
  Buildings,
  ClipboardText,
  Files,
  GearSix,
  HardDrives,
  SealCheck,
  SquaresFour,
  Stack,
  UploadSimple,
} from "@phosphor-icons/react";
import type { Icon } from "@phosphor-icons/react";

export type NavItem = { key: string; label: string; href: string; icon: Icon; group: string };

// Overview stands alone at the top. The rest are grouped by the question they answer.
export const NAV_GROUPS = ["", "Data", "Trust", "Organization"];

// One entry per real product area. Nothing is listed for a feature the API lacks.
export const NAV_ITEMS: NavItem[] = [
  { key: "overview", label: "Overview", href: "/app/overview", icon: SquaresFour, group: "" },
  { key: "storage", label: "Storage", href: "/app/storage", icon: HardDrives, group: "Data" },
  { key: "ingestion", label: "Ingestion", href: "/app/ingestion", icon: UploadSimple, group: "Data" },
  { key: "catalog", label: "Object Catalog", href: "/app/catalog", icon: Files, group: "Data" },
  { key: "datasets", label: "Datasets", href: "/app/datasets", icon: Stack, group: "Data" },
  { key: "validation", label: "Validation and Readiness", href: "/app/validation", icon: SealCheck, group: "Trust" },
  { key: "operations", label: "Lifecycle and Operations", href: "/app/operations", icon: GearSix, group: "Trust" },
  { key: "audit", label: "Audit", href: "/app/audit", icon: ClipboardText, group: "Trust" },
  { key: "settings", label: "Organization Settings", href: "/app/settings", icon: Buildings, group: "Organization" },
];
