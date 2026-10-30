"use client";

import Link from "next/link";
import { ButtonLink } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { DefList, LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { validationLabels, validationTone } from "../helpers";
import { useVersion, useVersionMembers } from "../hooks";
import { IntegrityNotice } from "./integrity-notice";
import { ResidencyPanel } from "./residency-panel";
import type { VersionMember } from "../types";
import { ImmutableBadge } from "./draft-notice";
import { SqlPanel } from "./sql-panel";
import { StorageAuditButton } from "./storage-audit-button";
import { ReadForTraining } from "@/features/streaming/components/read-for-training";
import { VersionCosts } from "@/features/tiering/components/version-costs";
import { VersionEvidence } from "@/features/evidence/components/version-evidence";
import { VersionQuality } from "@/features/quality/components/version-quality";
import { TrainingRuns } from "./training-runs";
import { VersionDiff } from "./version-diff";

export function VersionDetail({ id, versionId }: { id: string; versionId: string }) {
  const version = useVersion(id, versionId);
  const members = useVersionMembers(id, versionId);
  const base = `/app/datasets/${id}`;

  if (version.isPending) return <SkeletonRows rows={5} />;
  if (version.isError) return <ErrorState error={version.error} onRetry={() => version.refetch()} />;

  const v = version.data;
  const config = Object.keys(v.config ?? {}).length > 0;

  return (
    <div className="grid gap-6">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h2 className="flex flex-wrap items-center gap-3 text-xl font-semibold">
            Version {v.version}
            <ImmutableBadge />
            <StateBadge tone={validationTone(v.validation_status)}>{validationLabels[v.validation_status]}</StateBadge>
          </h2>
          <p className="mt-1 max-w-prose text-base text-muted">
            A snapshot of the draft. Its members, their exact object versions and its manifest hash never change.{" "}
            <Link href={`${base}/versions`} className="underline underline-offset-2">
              All versions
            </Link>
          </p>
          {v.superseded_by && (
            <p role="status" className="neo-in-sm mt-3 max-w-prose p-3 text-base">
              Superseded by an erasure: training reads its{" "}
              <Link href={`${base}/versions/${v.superseded_by}`} className="underline underline-offset-2">
                successor
              </Link>{" "}
              instead. Erased members show as erased below.
            </p>
          )}
        </div>
        <div className="flex flex-wrap gap-2">
          <ButtonLink href={`${base}/validation?version=${v.id}`} variant="primary">
            Validate this version
          </ButtonLink>
          <StorageAuditButton datasetId={id} versionId={v.id} />
          <ButtonLink href={`${base}/lineage?version=${v.id}`}>Lineage</ButtonLink>
        </div>
      </header>

      <IntegrityNotice id={id} versionId={v.id} />
      <ResidencyPanel id={id} versionId={v.id} />

      <Panel title="Version facts">
        <DefList
          items={[
            { label: "Status", value: v.status },
            {
              label: "Validation",
              value: (
                <span>
                  {validationLabels[v.validation_status]}
                  {v.validation_status === "NOT_VALIDATED" && (
                    <span className="ml-2 text-sm font-normal text-muted">No validation has finished for this version.</span>
                  )}
                </span>
              ),
            },
            { label: "Members", value: formatNumber(v.member_count) },
            { label: "Total size", value: formatBytes(v.total_bytes) },
            {
              label: "Manifest hash (SHA-256)",
              value: (
                <span className="flex flex-wrap items-center gap-2">
                  <span className="mono-data">{v.manifest_sha256}</span>
                  <CopyButton value={v.manifest_sha256} />
                </span>
              ),
            },
            { label: "Manifest format", value: <span className="mono-data">{v.manifest_format}</span> },
            {
              label: "Profile",
              value: v.profile_id ? (
                <span className="mono-data">
                  {v.profile_id}
                  {v.profile_version ? `@${v.profile_version}` : ""}
                </span>
              ) : (
                <span className="text-muted">None recorded</span>
              ),
            },
            {
              label: "Parent version",
              value: v.parent_version_id ? (
                <Link href={`${base}/versions/${v.parent_version_id}`} className="mono-data underline underline-offset-2">
                  {v.parent_version_id}
                </Link>
              ) : (
                <span className="text-muted">None</span>
              ),
            },
            {
              label: "Latest report",
              value: v.latest_report_id ? (
                <Link href={`/app/validation/reports/${v.latest_report_id}`} className="mono-data underline underline-offset-2">
                  {v.latest_report_id}
                </Link>
              ) : (
                <span className="text-muted">None</span>
              ),
            },
            { label: "Created", value: <Time value={v.created_at} /> },
            { label: "Id", value: <span className="mono-data">{v.id}</span> },
          ]}
        />
        {v.derived_from && (
          <div className="mt-5">
            <h3 className="text-sm text-muted">Derived from</h3>
            <ul className="mt-1 grid gap-1">
              {v.derived_from.version_ids.map((source) => (
                <li key={source} className="mono-data">
                  {source}
                </li>
              ))}
            </ul>
            {v.derived_from.transformation && (
              <p className="mt-1 text-sm text-muted">
                Transformation {v.derived_from.transformation.name} {v.derived_from.transformation.version}
              </p>
            )}
          </div>
        )}
        {config && (
          <div className="mt-5">
            <h3 className="text-sm text-muted">Configuration</h3>
            <pre className="mono-data neo-in-sm mt-1 overflow-x-auto p-3" data-lenis-prevent-horizontal>
              {JSON.stringify(v.config, null, 2)}
            </pre>
          </div>
        )}
      </Panel>

      <VersionDiff id={id} versionId={v.id} version={v.version} />

      <SqlPanel datasetId={id} versionId={v.id} />

      <ReadForTraining id={id} versionId={v.id} />
      <VersionCosts id={id} versionId={v.id} />
      <VersionEvidence id={id} versionId={v.id} version={v.version} />
      <VersionQuality id={id} versionId={v.id} />
      <TrainingRuns datasetId={id} versionId={v.id} />

      <Panel
        title="Members of this version"
        description="In canonical order. Each is pinned to one exact object version. An erased member keeps its record; its bytes are gone."
      >
        {members.isPending ? (
          <SkeletonRows rows={6} />
        ) : members.isError && members.items.length === 0 ? (
          <ErrorState error={members.error} onRetry={() => members.refetch()} />
        ) : members.items.length === 0 ? (
          <EmptyState title="This version has no members">A version is only created from a draft with members.</EmptyState>
        ) : (
          <>
            <Table>
              <thead>
                <tr>
                  <Th className="text-right">Ordinal</Th>
                  <Th>Path</Th>
                  <Th>Bytes</Th>
                  <Th className="text-right">Object version</Th>
                  <Th>Version id</Th>
                  <Th className="text-right">Size</Th>
                  <Th>Content type</Th>
                  <Th>SHA-256</Th>
                  <Th>Label</Th>
                  <Th>Split</Th>
                </tr>
              </thead>
              <tbody>
                {members.items.map((member) => (
                  <tr key={member.ordinal}>
                    <Td className="text-right tabular-nums">{member.ordinal}</Td>
                    <Td className="mono-data">{member.path}</Td>
                    <Td>
                      <Availability member={member} />
                    </Td>
                    <Td className="text-right tabular-nums">{member.object_version}</Td>
                    <Td className="mono-data">{member.version_id}</Td>
                    <Td className="text-right tabular-nums">{formatBytes(member.size_bytes)}</Td>
                    <Td>{member.content_type}</Td>
                    <Td className="mono-data" title={member.checksum_sha256}>
                      {member.checksum_sha256 ? member.checksum_sha256.slice(0, 12) : <span className="text-muted">None</span>}
                    </Td>
                    <Td>{member.label || <span className="text-muted">None</span>}</Td>
                    <Td>{member.split || <span className="text-muted">None</span>}</Td>
                  </tr>
                ))}
              </tbody>
            </Table>
            <LoadMore
              hasNext={Boolean(members.hasNextPage)}
              loading={members.isFetchingNextPage}
              onLoad={() => members.fetchNextPage()}
              shown={members.items.length}
            />
          </>
        )}
      </Panel>
    </div>
  );
}

function Availability({ member }: { member: VersionMember }) {
  if (member.availability === "ERASED") {
    return (
      <div>
        <StateBadge tone="failed">Erased</StateBadge>
        {member.erased_at && (
          <div className="mt-0.5 text-sm text-muted">
            <Time value={member.erased_at} />
          </div>
        )}
      </div>
    );
  }
  if (member.availability === "DELETION_PENDING") return <StateBadge tone="warning">Being deleted</StateBadge>;
  return <span className="text-sm">Stored</span>;
}
