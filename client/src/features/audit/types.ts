// Mirrors the audit DTOs in docs/api/identity-admin-audit.md section 5.

export type AuditOutcome = "SUCCESS" | "DENIED" | "FAILED";

export type AuditEvent = {
  id: string;
  seq: number;
  tenant_id?: string;
  actor_type: "USER" | "SERVICE" | "ANONYMOUS";
  actor_id: string;
  action: string;
  resource_type: string;
  resource_id?: string;
  outcome: AuditOutcome;
  reason?: string;
  correlation_id?: string;
  source_ip?: string;
  details?: Record<string, unknown>;
  created_at: string;
  hash: string;
};

export type AuditPage = { events: AuditEvent[]; next_before?: number };

export type AuditVerification = {
  chain: string;
  checked: number;
  first_seq: number;
  last_seq: number;
  intact: boolean;
  broken_at?: number;
  problem?: string;
};

export type AuditFilters = { action: string; outcome: "" | AuditOutcome; actor: string };

// "org" is the active organization's chain; "_platform" is the platform chain.
export type AuditChain = "org" | "_platform";
