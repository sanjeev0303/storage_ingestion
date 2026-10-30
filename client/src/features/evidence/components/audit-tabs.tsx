"use client";

import { TabNav } from "@/components/ui/page";

const tabs = [
  { href: "/app/audit", label: "Log" },
  { href: "/app/audit/evidence", label: "Anchors and exports" },
  { href: "/app/audit/reviews", label: "Reviews" },
  { href: "/app/audit/compliance", label: "Compliance" },
];

export function AuditTabs() {
  return <TabNav tabs={tabs} label="Audit" />;
}
