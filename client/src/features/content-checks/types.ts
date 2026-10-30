// Mirrors GET /assets/{id}/findings, /assets/{id}/blast-radius and /content-policy.

export type FindingAction = "flag" | "reject";

export type ContentFinding = {
  id: string;
  asset_id: string;
  job_id?: string;
  // The check that found it: content, pdf, office, pickle, secrets, gzip,
  // bam, fastq, mcap, tar, external or rescan.
  source: string;
  // "<family>.<what>", e.g. secret.aws_access_key.
  code: string;
  action: FindingAction;
  detail: string;
  created_at: string;
};

export type BlastRadius = {
  checksum_sha256: string;
  size_bytes: number;
  assets: {
    asset_id: string;
    object_id: string;
    version: number;
    file_name: string;
    status: string;
    blocked_reason?: string;
    created_at: string;
  }[];
  snapshots: { dataset_id: string; dataset_name: string; version: number; members: number }[];
  downloads: { actor_type: string; actor_id: string; links: number; first: string; last: string }[];
};

export type PolicyAction = "allow" | "flag" | "reject";

export type ContentPolicy = {
  allowed_types: string[];
  denied_types: string[];
  max_size_bytes: number;
  unknown_type_action: "" | "allow" | "reject";
  finding_actions: Record<string, PolicyAction>;
};

export type ContentPolicyResponse = { policy: ContentPolicy; finding_families: string[] };
