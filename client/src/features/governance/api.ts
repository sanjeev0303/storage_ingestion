import { api } from "@/lib/api/client";

// Mirrors /governance, /approvals, /assets/bulk-delete, /legal-holds/{id}/proof
// and the disposition certificate routes (docs/governance.md).

export type Profile = "STANDARD" | "GXP_VAULT" | "SEC_17A4_AUDIT_TRAIL";

export type Governance = {
  profile: Profile;
  two_person: boolean;
  approve_deletes: boolean;
  retention_floor: string;
  updated_by?: string;
  updated_at?: string;
};

export type ApprovalState = "PENDING" | "EXECUTED" | "REJECTED" | "EXPIRED" | "FAILED";

export type Approval = {
  id: string;
  kind: "RETENTION_CHANGE" | "HOLD_RELEASE" | "ASSET_DELETE" | "GOVERNANCE_CHANGE";
  summary: string;
  payload: unknown;
  state: ApprovalState;
  reason: string;
  requested_by: string;
  requested_at: string;
  expires_at: string;
  decided_by?: string;
  decided_at?: string;
  decision_note?: string;
  result?: string;
};

export type BulkDeletePlan = {
  matched: number;
  bytes: number;
  blocked: Record<string, number>;
  checked: number;
  scheduled: number;
  approval_id?: string;
  sample: string[];
};

export type BulkDeleteRequest = {
  asset_ids?: string[];
  dataset?: string;
  path_prefix?: string;
  metadata?: Record<string, string>;
  reason: string;
  dry_run: boolean;
};

export type HeldAsset = {
  asset_id: string;
  file_name: string;
  status: string;
  checksum_sha256: string;
  storage_version_id?: string;
  lock_applied_at?: string;
  lock_legal_hold: boolean;
  lock_mode?: string;
  lock_retain_until?: string;
  provider?: string;
};

export type HoldProof = { assets: HeldAsset[]; truncated: boolean; generated_at: string };

export type Certificate = {
  id: string;
  deletion_id: string;
  asset_id: string;
  body: Record<string, unknown>;
  signed_body: string;
  body_sha256: string;
  key_id: string;
  signature: string;
  created_at: string;
};

export const getGovernance = (signal?: AbortSignal) => api<Governance>("/governance", { signal });

export const putGovernance = (body: Omit<Governance, "updated_by" | "updated_at"> & { reason: string }) =>
  api<Governance | { status: "PENDING_APPROVAL"; approval_id: string }>("/governance", { method: "PUT", body });

export const listApprovals = (state: ApprovalState | "", signal?: AbortSignal) =>
  api<{ approvals: Approval[] }>("/approvals", { query: { state: state || undefined }, signal });

export const decideApproval = ({ id, approve, note }: { id: string; approve: boolean; note: string }) =>
  api<Approval>(`/approvals/${encodeURIComponent(id)}/${approve ? "approve" : "reject"}`, { method: "POST", body: { note } });

export const bulkDelete = (body: BulkDeleteRequest) => api<BulkDeletePlan>("/assets/bulk-delete", { method: "POST", body });

export const holdProof = (id: string, verify: boolean, signal?: AbortSignal) =>
  api<HoldProof>(`/legal-holds/${encodeURIComponent(id)}/proof`, { query: { verify: verify || undefined }, signal });

export const deletionCertificate = (deletionId: string) =>
  api<Certificate>(`/deletions/${encodeURIComponent(deletionId)}/certificate`);

export const assetCertificates = (assetId: string) =>
  api<{ certificates: Certificate[] }>(`/assets/${encodeURIComponent(assetId)}/certificates`);

export const publicKey = () => api<{ key_id: string; algorithm: string; public_key: string }>("/certificates/public-key");

const fromBase64 = (value: string) => Uint8Array.from(atob(value), (c) => c.charCodeAt(0));

// verifyCertificate checks a certificate in the browser: its SHA-256, and
// its Ed25519 signature with the platform's public key (WebCrypto).
export async function verifyCertificate(cert: Certificate, key: string | null) {
  const signed = new TextEncoder().encode(cert.signed_body);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", signed));
  const hex = Array.from(digest, (b) => b.toString(16).padStart(2, "0")).join("");
  const hashOk = hex === cert.body_sha256;
  if (!cert.signature || !key) return { hashOk, signature: "unsigned" as const };
  try {
    const pub = await crypto.subtle.importKey("raw", fromBase64(key), { name: "Ed25519" }, false, ["verify"]);
    const ok = await crypto.subtle.verify({ name: "Ed25519" }, pub, fromBase64(cert.signature), signed);
    return { hashOk, signature: ok ? ("valid" as const) : ("invalid" as const) };
  } catch {
    return { hashOk, signature: "unsupported" as const };
  }
}
