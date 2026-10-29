"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useId, useRef, useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Modal } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { newIdempotencyKey } from "@/lib/api/client";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { formatNumber } from "@/lib/utils/format";
import { lifecycleLabels, lifecycleTone, shortHash, validateName } from "../helpers";
import { useBranches, useCreateBranch, useDataset, useMerge, useMerges, useVersion, useVersions } from "../hooks";
import type { Dataset, MergeConflict, MergeResult, MergeSide, MergeStrategy } from "../types";
import { SnapshotControl } from "./snapshot-control";

export function BranchesScreen({ id }: { id: string }) {
  const dataset = useDataset(id);
  if (dataset.isPending) return <SkeletonRows rows={4} />;
  if (dataset.isError) return <ErrorState error={dataset.error} onRetry={() => dataset.refetch()} />;
  return dataset.data.branch ? <BranchView branch={dataset.data} /> : <ParentView parent={dataset.data} />;
}

function ParentView({ parent }: { parent: Dataset }) {
  const branches = useBranches(parent.id);
  const [source, setSource] = useState("");
  const list = branches.data ?? [];
  const names = Object.fromEntries([[parent.id, parent.name], ...list.map((b) => [b.id, b.branch?.name ?? b.name])]);
  const chosen = source || list[0]?.id || "";

  return (
    <div className="flex flex-col gap-6">
      <Panel
        title="Branches"
        description="A branch starts as a copy of one version's members and changes on its own. Rows are copied, bytes are not."
        actions={<NewBranchButton parent={parent} />}
      >
        {branches.isPending ? (
          <SkeletonRows rows={3} />
        ) : branches.isError ? (
          <ErrorState error={branches.error} onRetry={() => branches.refetch()} />
        ) : list.length === 0 ? (
          <EmptyState title="No branches yet">
            Branch from a version to try a relabelling, a new split or extra data without touching this dataset. Merge it
            back when it is right.
          </EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Branch</Th>
                <Th>Status</Th>
                <Th className="text-right">Members</Th>
                <Th>Branched from</Th>
                <Th>Created</Th>
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              </tr>
            </thead>
            <tbody>
              {list.map((b) => (
                <tr key={b.id}>
                  <Td>
                    <Link href={`/app/datasets/${b.id}`} className="font-semibold underline-offset-2 hover:underline">
                      {b.branch?.name ?? b.name}
                    </Link>
                  </Td>
                  <Td>
                    <StateBadge tone={lifecycleTone(b.lifecycle_status)}>{lifecycleLabels[b.lifecycle_status]}</StateBadge>
                  </Td>
                  <Td className="text-right tabular-nums">{formatNumber(b.member_count)}</Td>
                  <Td>
                    {b.branch && <VersionLink datasetId={parent.id} versionId={b.branch.fork_version_id} />}
                  </Td>
                  <Td>
                    <Time value={b.created_at} />
                  </Td>
                  <Td className="text-right">
                    <Button variant="ghost" onClick={() => setSource(b.id)} aria-pressed={chosen === b.id}>
                      Merge from here
                    </Button>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Panel>

      {list.length > 0 && (
        <MergePanel
          key={chosen}
          target={parent}
          sources={list.map((b) => ({ id: b.id, name: b.branch?.name ?? b.name }))}
          sourceId={chosen}
          onSource={setSource}
        />
      )}
      <MergeHistory target={parent} names={names} />
    </div>
  );
}

function BranchView({ branch }: { branch: Dataset }) {
  const info = branch.branch!;
  const parent = useDataset(info.parent_id);
  const parentName = parent.data?.name ?? "the parent dataset";
  const names = { [branch.id]: info.name, [info.parent_id]: parentName };

  return (
    <div className="flex flex-col gap-6">
      <Panel
        title={`Branch ${info.name}`}
        description={
          <>
            To bring this branch&apos;s changes into{" "}
            <Link href={`/app/datasets/${info.parent_id}/branches`} className="underline underline-offset-2">
              {parentName}
            </Link>
            , merge from its Branches tab.
          </>
        }
      >
        <dl className="grid gap-4 sm:grid-cols-2">
          <div>
            <dt className="text-sm text-muted">Branched from</dt>
            <dd className="mt-1">
              <VersionLink datasetId={info.parent_id} versionId={info.fork_version_id} />
            </dd>
          </div>
          <div>
            <dt className="text-sm text-muted">Common ancestor for the next merge</dt>
            <dd className="mt-1">
              <VersionLink datasetId={info.parent_id} otherId={branch.id} versionId={info.merge_base_version_id} />
            </dd>
          </div>
        </dl>
      </Panel>
      <MergePanel
        target={branch}
        sources={[{ id: info.parent_id, name: parentName }]}
        sourceId={info.parent_id}
        onSource={() => {}}
      />
      <MergeHistory target={branch} names={names} />
    </div>
  );
}

// A merge base is a version of the parent or of the branch: it is looked
// up on the first dataset, then on the second.
function VersionLink({ datasetId, otherId, versionId }: { datasetId: string; otherId?: string; versionId: string }) {
  const first = useVersion(datasetId, versionId);
  const second = useVersion(otherId ?? datasetId, versionId, Boolean(otherId) && first.isError);
  const found = first.data ?? second.data;
  if (!found) return <span className="mono-data">{shortHash(versionId)}</span>;
  return (
    <Link href={`/app/datasets/${found.dataset_id}/versions/${versionId}`} className="underline-offset-2 hover:underline">
      {found.dataset_id === datasetId ? `Version ${found.version}` : `Version ${found.version} of this branch`}
    </Link>
  );
}

function NewBranchButton({ parent }: { parent: Dataset }) {
  const { can } = useSession();
  const router = useRouter();
  const versions = useVersions(parent.id);
  const create = useCreateBranch(parent.id);
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [from, setFrom] = useState("");
  const [touched, setTouched] = useState(false);

  const archived = parent.lifecycle_status === "ARCHIVED";
  const noVersion = !parent.current_version_id;
  const nameProblem = validateName(name.trim());

  async function submit(e: FormEvent) {
    e.preventDefault();
    setTouched(true);
    if (nameProblem) return;
    try {
      const branch = await create.mutateAsync({ name: name.trim(), fromVersionId: from });
      notify.success(`Branch ${name.trim()} created`);
      router.push(`/app/datasets/${branch.id}`);
    } catch {
      // shown below the form
    }
  }

  const blocked = archived ? "Archived datasets are read-only" : !can("datasets.write") ? "Needs datasets.write" : noVersion ? "Take a snapshot first: a branch starts from a version" : undefined;

  return (
    <>
      <Button variant="primary" disabled={Boolean(blocked)} title={blocked} onClick={() => setOpen(true)}>
        New branch
      </Button>
      <Modal
        open={open}
        onOpenChange={(next) => {
          if (create.isPending) return;
          setOpen(next);
          if (!next) create.reset();
        }}
        title="New branch"
        description={`It starts with the members of one version of ${parent.name}.`}
        footer={
          <>
            <Button onClick={() => setOpen(false)} disabled={create.isPending}>
              Cancel
            </Button>
            <Button variant="primary" type="submit" form="new-branch" loading={create.isPending}>
              Create branch
            </Button>
          </>
        }
      >
        <form id="new-branch" onSubmit={submit} noValidate className="flex flex-col gap-4">
          <Field label="Branch name" required error={touched ? (nameProblem ?? "") : ""} hint={`Its full name is ${parent.name}@${name.trim() || "name"}.`}>
            {(p) => (
              <Input
                {...p}
                value={name}
                onChange={(e) => setName(e.target.value)}
                onBlur={() => setTouched(true)}
                autoComplete="off"
                spellCheck={false}
                maxLength={64}
                placeholder="relabel-cats…"
              />
            )}
          </Field>
          <Field label="Start from">
            {(p) => (
              <Select {...p} value={from} onChange={(e) => setFrom(e.target.value)}>
                <option value="">The latest version</option>
                {versions.items.map((v) => (
                  <option key={v.id} value={v.id}>
                    Version {v.version} ({formatNumber(v.member_count)} members)
                  </option>
                ))}
              </Select>
            )}
          </Field>
          {create.isError && (
            <p role="alert" className="text-base font-medium">
              {describeError(create.error)}
            </p>
          )}
        </form>
      </Modal>
    </>
  );
}

const STRATEGIES: { id: MergeStrategy; label: string; help: string }[] = [
  { id: "FAIL", label: "Stop on any conflict", help: "Nothing changes while an object was changed differently on both sides. You see each one." },
  { id: "SOURCE", label: "Take the incoming side", help: "Where both sides changed an object, the version being merged in wins." },
  { id: "TARGET", label: "Keep this draft's side", help: "Where both sides changed an object, this draft keeps what it has." },
];

function MergePanel({
  target,
  sources,
  sourceId,
  onSource,
}: {
  target: Dataset;
  sources: { id: string; name: string }[];
  sourceId: string;
  onSource: (id: string) => void;
}) {
  const { can } = useSession();
  const radioName = useId();
  const versions = useVersions(sourceId);
  const merge = useMerge(target.id);
  const [version, setVersion] = useState("");
  const [strategy, setStrategy] = useState<MergeStrategy>("FAIL");
  // One key per attempt, so a retried merge is not applied twice.
  const key = useRef(newIdempotencyKey());

  const sourceName = sources.find((s) => s.id === sourceId)?.name ?? "";
  const archived = target.lifecycle_status === "ARCHIVED";
  const blocked = archived ? "Archived datasets are read-only" : !can("datasets.write") ? "Needs datasets.write" : undefined;
  const noVersion = !versions.isPending && versions.items.length === 0;

  function changed() {
    merge.reset();
    key.current = newIdempotencyKey();
  }

  function run(dryRun: boolean) {
    merge.mutate(
      { source_dataset_id: sourceId, source_version_id: version || undefined, strategy, dry_run: dryRun, idempotency_key: dryRun ? undefined : key.current },
      {
        onSuccess: (result) => {
          if (result.applied) {
            key.current = newIdempotencyKey();
            notify.success("Merged into the draft");
          }
        },
      },
    );
  }

  const result = merge.data;

  return (
    <Panel
      title={`Merge into ${target.branch?.name ?? target.name}`}
      description="A three-way merge into this draft: only what changed on the incoming side since the common ancestor is brought in."
    >
      <div className="flex flex-col gap-5">
        <div className="grid gap-5 sm:grid-cols-2">
          <Field label="Bring changes from">
            {(p) => (
              <Select
                {...p}
                value={sourceId}
                disabled={sources.length < 2}
                onChange={(e) => {
                  onSource(e.target.value);
                  changed();
                }}
              >
                {sources.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          <Field label="Version" hint={noVersion ? `Take a snapshot of ${sourceName} first: a merge brings in a version.` : undefined}>
            {(p) => (
              <Select
                {...p}
                value={version}
                disabled={noVersion}
                onChange={(e) => {
                  setVersion(e.target.value);
                  changed();
                }}
              >
                <option value="">The latest version</option>
                {versions.items.map((v) => (
                  <option key={v.id} value={v.id}>
                    Version {v.version}
                  </option>
                ))}
              </Select>
            )}
          </Field>
        </div>

        <fieldset className="flex flex-col gap-3">
          <legend className="mb-1 text-sm font-medium">When both sides changed the same object</legend>
          {STRATEGIES.map((s) => (
            <label key={s.id} className="flex cursor-pointer items-start gap-3">
              <input
                type="radio"
                name={radioName}
                value={s.id}
                checked={strategy === s.id}
                onChange={() => {
                  setStrategy(s.id);
                  changed();
                }}
                className="mt-1 size-4 shrink-0 accent-(--ink)"
              />
              <span>
                <span className="block text-base font-medium">{s.label}</span>
                <span className="block max-w-prose text-sm text-muted">{s.help}</span>
              </span>
            </label>
          ))}
        </fieldset>

        <div className="flex flex-wrap gap-3">
          <Button disabled={Boolean(blocked) || noVersion} title={blocked} loading={merge.isPending && merge.variables?.dry_run} onClick={() => run(true)}>
            Preview the merge
          </Button>
          <Button
            variant="primary"
            disabled={Boolean(blocked) || noVersion || (result?.applied ?? false)}
            title={blocked}
            loading={merge.isPending && !merge.variables?.dry_run}
            onClick={() => run(false)}
          >
            Merge into the draft
          </Button>
        </div>

        {merge.isError && (
          <p role="alert" className="text-base font-medium">
            {describeError(merge.error)}
          </p>
        )}
        {result && <MergeOutcome result={result} target={target} preview={merge.variables?.dry_run ?? false} />}
      </div>
    </Panel>
  );
}

function MergeOutcome({ result, target, preview }: { result: MergeResult; target: Dataset; preview: boolean }) {
  const blocking = result.conflicts > 0 && result.strategy === "FAIL";
  const conflicts = `${formatNumber(result.conflicts)} ${result.conflicts === 1 ? "object was" : "objects were"} changed differently on both sides`;
  const nothing = result.added + result.updated + result.removed + result.conflicts === 0;
  let headline = "Preview: nothing has changed yet.";
  if (nothing && !result.applied) headline = "Nothing to merge: this draft already has every change from that version.";
  if (result.applied) headline = "Merged into the draft. Take a snapshot to keep it as a version.";
  else if (preview && blocking) headline = `Preview: ${conflicts}. Stopping on conflicts, this merge would change nothing; choose a side to merge.`;
  else if (blocking) headline = `Stopped: ${conflicts}. Nothing was changed.`;

  return (
    <div className="neo-in flex flex-col gap-4 p-4" role="status">
      <p className="text-base font-medium">{headline}</p>
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
        <Count label="Added" value={result.added} />
        <Count label="Updated" value={result.updated} />
        <Count label="Removed" value={result.removed} />
        <Count label="Already the same" value={result.already_same} />
        <Count label="Conflicts" value={result.conflicts} />
      </dl>
      {result.conflict_list.length > 0 && <ConflictTable conflicts={result.conflict_list} total={result.conflicts} />}
      {result.applied && (
        <div>
          <SnapshotControl dataset={target} />
        </div>
      )}
    </div>
  );
}

function Count({ label, value }: { label: string; value: number }) {
  return (
    <div>
      <dt className="text-sm text-muted">{label}</dt>
      <dd className="text-lg font-semibold tabular-nums">{formatNumber(value)}</dd>
    </div>
  );
}

function describeSide(side: MergeSide) {
  if (!side.present) return "Not a member";
  const parts = [`object version ${shortHash(side.version_id ?? "")}`];
  if (side.label) parts.push(`label ${side.label}`);
  if (side.split) parts.push(side.split);
  const keys = Object.keys(side.metadata ?? {}).length;
  if (keys) parts.push(`${keys} metadata ${keys === 1 ? "key" : "keys"}`);
  return parts.join(", ");
}

function ConflictTable({ conflicts, total }: { conflicts: MergeConflict[]; total: number }) {
  return (
    <div className="flex flex-col gap-2">
      <Table>
        <thead>
          <tr>
            <Th>Object</Th>
            <Th>Common ancestor</Th>
            <Th>This draft</Th>
            <Th>Incoming</Th>
          </tr>
        </thead>
        <tbody>
          {conflicts.map((c) => (
            <tr key={c.object_id}>
              <Td className="mono-data break-all text-sm">
                {c.namespace}/{c.path}
              </Td>
              <Td className="text-sm">{describeSide(c.base)}</Td>
              <Td className="text-sm">{describeSide(c.target)}</Td>
              <Td className="text-sm">{describeSide(c.source)}</Td>
            </tr>
          ))}
        </tbody>
      </Table>
      {total > conflicts.length && (
        <p className="text-sm text-muted">
          Showing the first {formatNumber(conflicts.length)} of {formatNumber(total)}.
        </p>
      )}
    </div>
  );
}

const STRATEGY_NAMES: Record<MergeStrategy, string> = {
  FAIL: "Stop on conflict",
  SOURCE: "Incoming side",
  TARGET: "Draft's side",
};

function MergeHistory({ target, names }: { target: Dataset; names: Record<string, string> }) {
  const merges = useMerges(target.id);

  return (
    <Panel title="Merges into this dataset" description="Every applied merge, newest first.">
      {merges.isPending ? (
        <SkeletonRows rows={2} />
      ) : merges.isError ? (
        <ErrorState error={merges.error} onRetry={() => merges.refetch()} />
      ) : merges.data.length === 0 ? (
        <p className="text-base text-muted">Nothing has been merged into this dataset yet.</p>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>When</Th>
              <Th>From</Th>
              <Th>On conflict</Th>
              <Th className="text-right">Added</Th>
              <Th className="text-right">Updated</Th>
              <Th className="text-right">Removed</Th>
              <Th className="text-right">Conflicts</Th>
            </tr>
          </thead>
          <tbody>
            {merges.data.map((m) => (
              <tr key={m.id}>
                <Td>
                  <Time value={m.created_at} />
                </Td>
                <Td>
                  <Link
                    href={`/app/datasets/${m.source_dataset_id}/versions/${m.source_version_id}`}
                    className="underline-offset-2 hover:underline"
                  >
                    {names[m.source_dataset_id] ?? shortHash(m.source_dataset_id)}
                  </Link>
                </Td>
                <Td>{STRATEGY_NAMES[m.strategy]}</Td>
                <Td className="text-right tabular-nums">{formatNumber(m.added)}</Td>
                <Td className="text-right tabular-nums">{formatNumber(m.updated)}</Td>
                <Td className="text-right tabular-nums">{formatNumber(m.removed)}</Td>
                <Td className="text-right tabular-nums">{formatNumber(m.conflicts)}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </Panel>
  );
}
