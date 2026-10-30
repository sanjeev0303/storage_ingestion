"use client";

import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { useSession } from "@/lib/auth/session";
import { formatBytes } from "@/lib/utils/format";
import { useBlastRadius, useFindings } from "../hooks";
import type { ContentFinding } from "../types";

// What each family of findings means, for someone deciding what to do.
export const FAMILY_TEXT: Record<string, { title: string; detail: string }> = {
  active_content: {
    title: "Active content in documents",
    detail: "Macros, ActiveX and embedded objects in Office files; JavaScript, launch actions, embedded files and XFA forms in PDFs.",
  },
  model: {
    title: "Code in model files",
    detail:
      "Pickle and PyTorch files whose loading would run code, imports that need a model's own code, object arrays in NumPy files and broken safetensors headers.",
  },
  secret: {
    title: "Credentials in text",
    detail:
      "AWS, GitHub, Slack, Stripe, Google, Azure and AI provider keys, private keys and signed tokens. A finding names the kind and where it is, never the value.",
  },
  format: {
    title: "Broken formats",
    detail: "Damaged gzip, cut-short BGZF and BAM files, malformed FASTQ records, MCAP recordings without a footer and tar entries that escape their folder.",
  },
  external: {
    title: "External engine",
    detail: "The verdict of an engine your operator runs on every upload, such as a PII, de-identification or YARA scanner.",
  },
  malware: {
    title: "Malware",
    detail: "Rejects at upload, and blocks a published file that a re-scan with newer signatures finds infected. It cannot be changed.",
  },
};

export function familyOf(code: string) {
  return code.split(".")[0] ?? code;
}

export function ContentFindingsPanel({ assetId, blockedReason }: { assetId: string; blockedReason?: string }) {
  const { can } = useSession();
  const allowed = can("assets.read");
  const findings = useFindings(assetId, allowed);

  return (
    <Panel
      title="Content checks"
      description="What the checks inside the quarantine gate found in this file's bytes, beyond its type and the malware scan."
    >
      {blockedReason && (
        <p role="alert" className="mb-4 neo-in-sm p-3">
          <span className="font-medium">Blocked.</span> A re-scan with newer signatures found this file unsafe after it was
          published (<span className="mono-data">{blockedReason}</span>). No download is served. Delete the asset, and check
          where its bytes went below.
        </p>
      )}
      {!allowed ? (
        <PermissionDenied detail="Reading findings needs the assets.read permission." />
      ) : findings.isPending ? (
        <SkeletonRows rows={2} />
      ) : findings.isError ? (
        <ErrorState error={findings.error} onRetry={() => findings.refetch()} />
      ) : findings.data.findings.length === 0 ? (
        <p className="text-base text-muted">
          Nothing found. A file uploaded before the checks existed, or larger than their limit, shows the same; the
          processing history says which checks ran.
        </p>
      ) : (
        <ul className="flex flex-col gap-3">
          {findings.data.findings.map((f) => (
            <FindingRow key={f.id} finding={f} />
          ))}
        </ul>
      )}
    </Panel>
  );
}

function FindingRow({ finding: f }: { finding: ContentFinding }) {
  const family = FAMILY_TEXT[familyOf(f.code)];
  return (
    <li className="neo-in-sm p-3">
      <div className="flex flex-wrap items-center gap-2">
        <StateBadge tone={f.action === "reject" ? "failed" : "warning"}>{f.action === "reject" ? "Rejected" : "Flagged"}</StateBadge>
        <span className="font-medium">{family?.title ?? "Finding"}</span>
        <span translate="no" className="mono-data text-sm text-muted">{f.code}</span>
      </div>
      <p className="mt-1 [overflow-wrap:anywhere]">{f.detail}</p>
      <p className="mt-1 text-sm text-muted">
        Found by the <span className="mono-data">{f.source}</span> check, <Time value={f.created_at} />
      </p>
    </li>
  );
}

// Where the bytes went: loaded only when asked for, as it reads the audit log.
export function BlastRadiusPanel({ assetId }: { assetId: string }) {
  const { can } = useSession();
  const allowed = can("audit.read");
  const [asked, setAsked] = useState(false);
  const radius = useBlastRadius(assetId, allowed && asked);

  return (
    <Panel
      title="Where these bytes went"
      description="Every asset with the same SHA-256, the dataset snapshots that pin them and who was given download links."
      actions={
        allowed && !asked && <Button onClick={() => setAsked(true)}>Trace these bytes</Button>
      }
    >
      {!allowed ? (
        <PermissionDenied detail="Tracing downloads needs the audit.read permission." />
      ) : !asked ? (
        <p className="text-base text-muted">
          Use it when a file turns out to be unsafe or leaked: it answers which snapshots and which people it reached.
        </p>
      ) : radius.isPending ? (
        <SkeletonRows rows={3} />
      ) : radius.isError ? (
        <ErrorState error={radius.error} onRetry={() => radius.refetch()} />
      ) : (
        <div className="flex flex-col gap-5">
          <p className="text-sm text-muted">
            <span className="mono-data [overflow-wrap:anywhere]">{radius.data.checksum_sha256 || "No SHA-256 recorded"}</span>,{" "}
            {formatBytes(radius.data.size_bytes)}
          </p>
          <section aria-labelledby="radius-assets">
            <h3 id="radius-assets" className="font-medium">
              Assets with these bytes ({radius.data.assets.length})
            </h3>
            <ul className="mt-2 flex flex-col gap-1">
              {radius.data.assets.map((a) => (
                <li key={a.asset_id} className="flex flex-wrap items-center gap-2 text-sm">
                  <StateBadge tone={a.blocked_reason ? "failed" : "neutral"}>{a.blocked_reason ? "Blocked" : a.status}</StateBadge>
                  <span className="[overflow-wrap:anywhere]">{a.file_name}</span>
                  <span translate="no" className="mono-data text-muted [overflow-wrap:anywhere]">{a.asset_id}</span>
                </li>
              ))}
            </ul>
          </section>
          <section aria-labelledby="radius-snapshots">
            <h3 id="radius-snapshots" className="font-medium">
              Snapshots that pin them ({radius.data.snapshots.length})
            </h3>
            {radius.data.snapshots.length === 0 ? (
              <p className="mt-1 text-sm text-muted">No dataset snapshot holds these bytes.</p>
            ) : (
              <ul className="mt-2 flex flex-col gap-1 text-sm">
                {radius.data.snapshots.map((s) => (
                  <li key={`${s.dataset_id}-${s.version}`}>
                    {s.dataset_name} version {s.version}
                    <span className="text-muted">
                      , {s.members} {s.members === 1 ? "member" : "members"}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </section>
          <section aria-labelledby="radius-downloads">
            <h3 id="radius-downloads" className="font-medium">
              Given download links ({radius.data.downloads.length})
            </h3>
            {radius.data.downloads.length === 0 ? (
              <EmptyState title="Nobody was given a link">The audit log has no download link issued for these bytes.</EmptyState>
            ) : (
              <ul className="mt-2 flex flex-col gap-1 text-sm">
                {radius.data.downloads.map((d) => (
                  <li key={`${d.actor_type}-${d.actor_id}`}>
                    <span translate="no" className="mono-data [overflow-wrap:anywhere]">{d.actor_id}</span>{" "}
                    <span className="text-muted">
                      ({d.actor_type.toLowerCase()}), {d.links} {d.links === 1 ? "link" : "links"}, last <Time value={d.last} />
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </section>
        </div>
      )}
    </Panel>
  );
}
