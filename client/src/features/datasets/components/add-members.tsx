"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Modal } from "@/components/ui/dialog";
import { Field, Input, Select, Textarea } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { StateBadge } from "@/components/ui/status";
import { Table, Td, Th } from "@/components/ui/table";
import { describeError } from "@/lib/api/errors";
import { cleanWholeNumber } from "@/lib/utils/forgive";
import { formatBytes } from "@/lib/utils/format";
import { parseKeyValues, parseMemberLines, SPLITS } from "../helpers";
import { useAddMembers, useCatalogSearch } from "../hooks";
import type { AddMembersResponse, DuplicatePolicy, MemberRequest, Split } from "../types";

type Mode = "one" | "lines" | "catalog";

const modes: { value: Mode; label: string }[] = [
  { value: "one", label: "One member" },
  { value: "lines", label: "Paste JSON lines" },
  { value: "catalog", label: "Pick from catalog" },
];

const duplicateHelp: Record<DuplicatePolicy, string> = {
  REJECT: "Reject: an object that is already a member is refused and reported.",
  SKIP: "Skip: an object that is already a member is left as it is.",
  REPLACE: "Replace: an existing member takes the new version, label, split and metadata.",
};

export function AddMembersDialog({ datasetId, open, onClose }: { datasetId: string; open: boolean; onClose: () => void }) {
  const add = useAddMembers(datasetId);
  const [mode, setMode] = useState<Mode>("one");
  const [policy, setPolicy] = useState<DuplicatePolicy>("REJECT");
  const [sent, setSent] = useState<MemberRequest[]>([]);

  function send(members: MemberRequest[]) {
    setSent(members);
    add.mutate({ members, onDuplicate: policy });
  }

  function close() {
    if (add.isPending) return;
    add.reset();
    onClose();
  }

  return (
    <Modal open={open} onOpenChange={(next) => !next && close()} title="Add members">
      <div className="grid gap-4">
        <p className="neo-in-sm p-3 text-sm text-muted">
          A member without a pinned version follows the object&apos;s current version in the draft, and keeps following
          it until you take a snapshot. The snapshot is what pins the exact version.
        </p>

        <div role="group" aria-label="How to add" className="flex flex-wrap gap-2">
          {modes.map((m) => (
            <Button
              key={m.value}
              variant={mode === m.value ? "primary" : "secondary"}
              aria-pressed={mode === m.value}
              onClick={() => setMode(m.value)}
            >
              {m.label}
            </Button>
          ))}
        </div>

        <Field label="If the object is already a member" hint={duplicateHelp[policy]}>
          {(props) => (
            <Select {...props} value={policy} onChange={(e) => setPolicy(e.target.value as DuplicatePolicy)}>
              <option value="REJECT">Reject it</option>
              <option value="SKIP">Skip it</option>
              <option value="REPLACE">Replace it</option>
            </Select>
          )}
        </Field>

        {mode === "one" && <OneMemberForm pending={add.isPending} onSend={send} />}
        {mode === "lines" && <LinesForm pending={add.isPending} onSend={send} />}
        {mode === "catalog" && <CatalogPicker pending={add.isPending} onSend={send} />}

        {add.isError && (
          <p role="alert" className="text-base font-medium">
            {describeError(add.error)}
          </p>
        )}
        {add.data && <MemberResults response={add.data} sent={sent} />}
      </div>
    </Modal>
  );
}

function OneMemberForm({
  pending,
  onSend,
}: {
  pending: boolean;
  onSend: (members: MemberRequest[]) => void;
}) {
  const [by, setBy] = useState<"path" | "object_id">("path");
  const [objectId, setObjectId] = useState("");
  const [namespace, setNamespace] = useState("");
  const [path, setPath] = useState("");
  const [pin, setPin] = useState<"none" | "version_id" | "version">("none");
  const [pinValue, setPinValue] = useState("");
  const [label, setLabel] = useState("");
  const [split, setSplit] = useState<Split | "">("");
  const [metaText, setMetaText] = useState("");

  const objectIdProblem = by === "object_id" && !objectId.trim() ? "Enter an object id." : undefined;
  const pathProblem = by === "path" && !path.trim() ? "Enter an object path." : undefined;
  const versionNumber = Number(cleanWholeNumber(pinValue));
  let pinProblem: string | undefined;
  if (pin === "version_id" && !pinValue.trim()) pinProblem = "Enter the version id (the asset id) to pin.";
  if (pin === "version" && versionNumber < 1) pinProblem = "The version number must be a whole number of 1 or more.";
  const meta = parseKeyValues(metaText);
  const valid = !objectIdProblem && !pathProblem && !pinProblem && !meta.error;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    const member: MemberRequest = {};
    if (by === "object_id") {
      member.object_id = objectId.trim();
    } else {
      member.path = path.trim();
      member.namespace = namespace.trim();
    }
    if (pin === "version_id") member.version_id = pinValue.trim();
    if (pin === "version") member.version = versionNumber;
    if (label.trim()) member.label = label.trim();
    if (split) member.split = split;
    if (Object.keys(meta.values).length) member.metadata = meta.values;
    onSend([member]);
  }

  return (
    <form onSubmit={submit} noValidate className="grid gap-4" aria-label="Add one member">
      <Field label="Find the object by">
        {(props) => (
          <Select {...props} value={by} onChange={(e) => setBy(e.target.value as "path" | "object_id")}>
            <option value="path">Path (and namespace)</option>
            <option value="object_id">Object id</option>
          </Select>
        )}
      </Field>
      {by === "object_id" ? (
        <Field label="Object id" required error={objectIdProblem}>
          {(props) => <Input {...props} value={objectId} onChange={(e) => setObjectId(e.target.value)} spellCheck={false} />}
        </Field>
      ) : (
        <div className="grid gap-4 sm:grid-cols-3">
          <Field label="Namespace" hint="Empty is the root.">
            {(props) => <Input {...props} value={namespace} onChange={(e) => setNamespace(e.target.value)} spellCheck={false} />}
          </Field>
          <Field label="Path" required error={pathProblem} className="sm:col-span-2">
            {(props) => <Input {...props} value={path} onChange={(e) => setPath(e.target.value)} spellCheck={false} />}
          </Field>
        </div>
      )}
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Version" hint="Unpinned follows the object's current version.">
          {(props) => (
            <Select {...props} value={pin} onChange={(e) => setPin(e.target.value as typeof pin)}>
              <option value="none">Follow current version</option>
              <option value="version_id">Pin by version id</option>
              <option value="version">Pin by version number</option>
            </Select>
          )}
        </Field>
        {pin !== "none" && (
          <Field label={pin === "version" ? "Version number" : "Version id"} required error={pinProblem}>
            {(props) => <Input {...props} value={pinValue} onChange={(e) => setPinValue(e.target.value)} spellCheck={false} />}
          </Field>
        )}
        <Field label="Label" hint="Up to 256 characters.">
          {(props) => <Input {...props} value={label} onChange={(e) => setLabel(e.target.value)} maxLength={256} />}
        </Field>
        <Field label="Split">
          {(props) => (
            <Select {...props} value={split} onChange={(e) => setSplit(e.target.value as Split | "")}>
              <option value="">No split</option>
              {SPLITS.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </Select>
          )}
        </Field>
      </div>
      <Field label="Metadata" error={meta.error ?? undefined} hint="One key=value per line.">
        {(props) => (
          <Textarea {...props} value={metaText} onChange={(e) => setMetaText(e.target.value)} className="mono-data" />
        )}
      </Field>
      <div>
        <SubmitButton valid={valid} loading={pending} hint="Fill in the required fields to add the member.">
          Add member
        </SubmitButton>
      </div>
    </form>
  );
}

function LinesForm({
  pending,
  onSend,
}: {
  pending: boolean;
  onSend: (members: MemberRequest[]) => void;
}) {
  const [text, setText] = useState("");
  const parsed = parseMemberLines(text);

  function submit(event: FormEvent) {
    event.preventDefault();
    if (parsed.error) return;
    onSend(parsed.members);
  }

  return (
    <form onSubmit={submit} noValidate className="grid gap-4" aria-label="Add members from JSON lines">
      <Field
        label="Members as JSON lines"
        required
        error={parsed.error ?? undefined}
        hint='One object per line, for example {"path":"pets/cat-1.txt","label":"cat","split":"train"}. Up to 1000 lines. A bad line is reported and the others still go in.'
      >
        {(props) => (
          <Textarea
            {...props}
            value={text}
            onChange={(e) => setText(e.target.value)}
            spellCheck={false}
            className="mono-data min-h-40"
          />
        )}
      </Field>
      <div>
        <SubmitButton valid={!parsed.error} loading={pending} hint="Fix the JSON lines to continue.">
          Add members
        </SubmitButton>
      </div>
    </form>
  );
}

function CatalogPicker({ pending, onSend }: { pending: boolean; onSend: (members: MemberRequest[]) => void }) {
  const [draft, setDraft] = useState("");
  const [prefix, setPrefix] = useState("");
  const [picked, setPicked] = useState<Record<string, MemberRequest>>({});
  const [pinCurrent, setPinCurrent] = useState(false);
  const results = useCatalogSearch(prefix);

  function toggle(objectId: string, checked: boolean, assetId?: string) {
    setPicked((current) => {
      const next = { ...current };
      if (checked) next[objectId] = { object_id: objectId, version_id: assetId };
      else delete next[objectId];
      return next;
    });
  }

  function submit() {
    const members = Object.values(picked).map((m) => (pinCurrent && m.version_id ? m : { object_id: m.object_id }));
    onSend(members);
  }

  const count = Object.keys(picked).length;

  return (
    <div className="grid gap-4">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setPrefix(draft.trim());
        }}
        className="flex items-end gap-2"
        aria-label="Search the catalog"
      >
        <Field label="Path starts with" className="flex-1">
          {(props) => <Input {...props} value={draft} onChange={(e) => setDraft(e.target.value)} spellCheck={false} />}
        </Field>
        <Button type="submit">
          Search
        </Button>
      </form>

      <p className="-mt-2 text-sm text-muted">Searches object paths by prefix. Leave empty to browse.</p>

      {results.isPending ? (
        <SkeletonRows rows={4} />
      ) : results.isError && results.items.length === 0 ? (
        <ErrorState error={results.error} onRetry={() => results.refetch()} />
      ) : results.items.length === 0 ? (
        <EmptyState title="No objects found">
          Only objects with a published version are listed. Try a shorter prefix, or upload files first.
        </EmptyState>
      ) : (
        <>
          <Table>
            <thead>
              <tr>
                <Th>
                  <span className="sr-only">Select</span>
                </Th>
                <Th>Path</Th>
                <Th>Namespace</Th>
                <Th>Current</Th>
                <Th className="text-right">Size</Th>
              </tr>
            </thead>
            <tbody>
              {results.items.map((object) => (
                <tr key={object.id}>
                  <Td>
                    <input
                      type="checkbox"
                      aria-label={`Select ${object.path}`}
                      className="size-4 accent-(--ink)"
                      checked={object.id in picked}
                      onChange={(e) => toggle(object.id, e.target.checked, object.current_version?.asset_id)}
                    />
                  </Td>
                  <Td className="mono-data">{object.path}</Td>
                  <Td className="mono-data">{object.dataset || "(root)"}</Td>
                  <Td>{object.current_version ? `version ${object.current_version.version}` : "None"}</Td>
                  <Td className="text-right tabular-nums">
                    {object.current_version ? formatBytes(object.current_version.size_bytes) : ""}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <LoadMore
            hasNext={Boolean(results.hasNextPage)}
            loading={results.isFetchingNextPage}
            onLoad={() => results.fetchNextPage()}
            shown={results.items.length}
          />
        </>
      )}

      <label className="flex items-start gap-2 text-base">
        <input
          type="checkbox"
          className="mt-0.5 size-4 accent-(--ink)"
          checked={pinCurrent}
          onChange={(e) => setPinCurrent(e.target.checked)}
        />
        <span>Pin each to the version it has now. Otherwise it follows the current version until a snapshot.</span>
      </label>
      <div>
        <Button variant="primary" onClick={submit} disabled={count === 0} loading={pending}>
          Add {count} selected
        </Button>
      </div>
    </div>
  );
}

const outcomeTone = {
  ADDED: "done",
  UPDATED: "done",
  SKIPPED: "neutral",
  REJECTED: "attention",
} as const;

const SHOWN_RESULTS = 200;

function describeInput(member: MemberRequest | undefined) {
  if (!member) return "";
  if (member.path !== undefined) return member.namespace ? `${member.namespace}/${member.path}` : member.path;
  return member.object_id ?? "";
}

function MemberResults({ response, sent }: { response: AddMembersResponse; sent: MemberRequest[] }) {
  const rows = [...response.results].sort((a, b) => Number(b.outcome === "REJECTED") - Number(a.outcome === "REJECTED"));
  return (
    <section aria-label="Results" className="grid gap-3">
      <p role="status" className="text-base font-medium">
        {response.added} added, {response.updated} updated, {response.skipped} skipped, {response.rejected} rejected.
      </p>
      {response.rejected > 0 && (
        <p className="text-sm text-muted">Rejected items were not added. The rest of the request went through.</p>
      )}
      <Table>
        <thead>
          <tr>
            <Th>Item</Th>
            <Th>Result</Th>
            <Th>Detail</Th>
          </tr>
        </thead>
        <tbody>
          {rows.slice(0, SHOWN_RESULTS).map((row) => (
            <tr key={row.index}>
              <Td className="mono-data">
                {row.index + 1}. {describeInput(sent[row.index]) || row.object_id}
              </Td>
              <Td>
                <StateBadge tone={outcomeTone[row.outcome]}>{row.outcome}</StateBadge>
              </Td>
              <Td>
                {row.code && <span className="mono-data">{row.code}</span>}
                {row.error && <span className="ml-2 text-muted">{row.error}</span>}
              </Td>
            </tr>
          ))}
        </tbody>
      </Table>
      {rows.length > SHOWN_RESULTS && (
        <p className="text-sm text-muted">Showing the first {SHOWN_RESULTS} of {rows.length} results, rejected first.</p>
      )}
    </section>
  );
}
