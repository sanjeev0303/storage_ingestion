import type { SessionValue } from "@/lib/auth/session";

const all = [
  { href: "/app/operations/assets", label: "Assets", allowed: (s: SessionValue) => s.can("assets.read") },
  { href: "/app/operations/retention", label: "Retention and holds", allowed: (s: SessionValue) => s.can("retention.manage") },
  { href: "/app/operations/governance", label: "Governance", allowed: (s: SessionValue) => s.can("retention.manage") },
  { href: "/app/operations/access", label: "Access", allowed: (s: SessionValue) => s.can("assets.read") },
  { href: "/app/operations/costs", label: "Costs and tiers", allowed: (s: SessionValue) => s.can("org.read") },
  { href: "/app/operations/erasure", label: "Erasure", allowed: (s: SessionValue) => s.can("retention.manage") },
  { href: "/app/operations/dead-letters", label: "Dead letters", allowed: (s: SessionValue) => s.isPlatformAdmin },
  { href: "/app/operations/status", label: "Status", allowed: () => true },
];

export function operationsTabs(session: SessionValue) {
  return all.filter((tab) => tab.allowed(session)).map(({ href, label }) => ({ href, label }));
}
