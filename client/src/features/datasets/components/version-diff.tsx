"use client";

import { useState } from "react";
import { Field, Select } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { formatNumber } from "@/lib/utils/format";
import { useVersionDiff, useVersions } from "../hooks";
import type { DiffKind, DiffSide, VersionDifference } from "../types";

const KIND_TEXT: Record<DiffKind, string> = {
  ADDED: "Added",
  REMOVED: "Removed",
  CHANGED: "New bytes",
  RELABELED: "Relabeled",
};

// What changed between this version and an earlier one, object by object:
// the answer to "why does a model trained on it behave differently?".
export function VersionDiff({ id, versionId, version }: { id: string; versionId: string; version: number }) {
  const versions = useVersions(id);
  const others = versions.items.filter((v) => v.id !== versionId);
  // The version just before this one is the usual question.
  const previous = others.find((v) => v.version < version) ?? others[0];
  const [chosen, setChosen] = useState("");
  const against = chosen || previous?.id || "";
  const diff = useVersionDiff(id, versionId, against);
  const base = others.find((v) => v.id === against);

  return (
    <Panel
      title="What changed"
      description="Compared object by object. New bytes means the pinned SHA-256 differs; relabeled means only the path, label or split moved."
    >
      {versions.isPending ? (
        <SkeletonRows rows={2} />
      ) : others.length === 0 ? (
        <EmptyState title="Nothing to compare with">This is the dataset&apos;s only version. Take another snapshot to compare them.</EmptyState>
      ) : (
        <div className="flex flex-col gap-4">
          <Field label="Compared with" className="max-w-xs">
            {(p) => (
              <Select {...p} value={against} onChange={(e) => setChosen(e.target.value)}>
                {others.map((v) => (
                  <option key={v.id} value={v.id}>
                    Version {v.version}
                  </option>
                ))}
              </Select>
            )}
          </Field>

          {diff.isPending ? (
            <SkeletonRows rows={4} />
          ) : diff.isError ? (
            <ErrorState error={diff.error} onRetry={() => diff.refetch()} />
          ) : (
            <>
              {diff.summary && (
                <p className="text-base">
                  From version {base?.version} to version {version}:{" "}
                  <strong className="tabular-nums">{formatNumber(diff.summary.added)}</strong> added,{" "}
                  <strong className="tabular-nums">{formatNumber(diff.summary.removed)}</strong> removed,{" "}
                  <strong className="tabular-nums">{formatNumber(diff.summary.changed)}</strong> with new bytes,{" "}
                  <strong className="tabular-nums">{formatNumber(diff.summary.relabeled)}</strong> relabeled and{" "}
                  <span className="tabular-nums">{formatNumber(diff.summary.unchanged)}</span> the same.
                </p>
              )}
              {diff.items.length === 0 ? (
                <EmptyState title="The same members">Every object is pinned to the same bytes with the same path, label and split.</EmptyState>
              ) : (
                <>
                  <Table>
                    <caption className="sr-only">Differences between the two versions</caption>
                    <thead>
                      <tr>
                        <Th>Change</Th>
                        <Th>Version {base?.version}</Th>
                        <Th>Version {version}</Th>
                      </tr>
                    </thead>
                    <tbody>
                      {diff.items.map((d) => (
                        <DiffRow key={d.object_id} diff={d} />
                      ))}
                    </tbody>
                  </Table>
                  <LoadMore
                    hasNext={Boolean(diff.hasNextPage)}
                    loading={diff.isFetchingNextPage}
                    onLoad={() => diff.fetchNextPage()}
                    shown={diff.items.length}
                  />
                </>
              )}
            </>
          )}
        </div>
      )}
    </Panel>
  );
}

function DiffRow({ diff }: { diff: VersionDifference }) {
  const tone = diff.kind === "REMOVED" ? "failed" : diff.kind === "CHANGED" ? "warning" : diff.kind === "ADDED" ? "done" : "neutral";
  return (
    <tr>
      <Td>
        <StateBadge tone={tone}>{KIND_TEXT[diff.kind]}</StateBadge>
      </Td>
      <Td>{diff.base ? <Side side={diff.base} other={diff.target} /> : <span className="text-muted">Not in this version</span>}</Td>
      <Td>{diff.target ? <Side side={diff.target} other={diff.base} /> : <span className="text-muted">Not in this version</span>}</Td>
    </tr>
  );
}

// One side of a difference. Fields that differ from the other side are in
// full ink; the rest are muted, so the eye lands on what moved.
function Side({ side, other }: { side: DiffSide; other?: DiffSide }) {
  const differs = (a?: string, b?: string) => Boolean(other) && (a ?? "") !== (b ?? "");
  return (
    <div className="flex flex-col gap-0.5">
      <span className={differs(side.path, other?.path) ? "mono-data" : "mono-data text-muted"}>{side.path}</span>
      <span className={differs(side.checksum_sha256, other?.checksum_sha256) ? "mono-data text-sm" : "mono-data text-sm text-muted"}>
        {side.checksum_sha256 ? side.checksum_sha256.slice(0, 16) : "no checksum"}
      </span>
      {(side.label || side.split) && (
        <span className={differs(side.label, other?.label) || differs(side.split, other?.split) ? "text-sm" : "text-sm text-muted"}>
          {[side.label, side.split].filter(Boolean).join(", ")}
        </span>
      )}
    </div>
  );
}
