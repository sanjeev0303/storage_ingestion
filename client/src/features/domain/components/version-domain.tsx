"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatNumber } from "@/lib/utils/format";
import {
  addSegment,
  createIndex,
  deleteIndex,
  deleteSegment,
  getPublication,
  listIndexes,
  listRecordings,
  listSegments,
  mintDOI,
  publish,
  retentionCost,
  stacCollection,
  stacItems,
  withdraw,
  type EmbeddingIndex,
  type Segment,
} from "../api";

function seconds(startNs: number, endNs: number) {
  return `${((endNs - startNs) / 1e9).toLocaleString(undefined, { maximumFractionDigits: 1 })} s`;
}

function secondsInto(ns: number, origin = 0) {
  return ((ns - origin) / 1e9).toLocaleString(undefined, { maximumFractionDigits: 3 });
}

function saveJSON(name: string, value: object) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(value, null, 2)], { type: "application/json" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  URL.revokeObjectURL(url);
}

function usd(value: number) {
  return value.toLocaleString(undefined, { style: "currency", currency: "USD" });
}

function RetentionCost({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const [years, setYears] = useState(10);
  const cost = useQuery({
    queryKey: orgKey(organizationId, "domain", "cost", versionId, years),
    queryFn: ({ signal }) => retentionCost(id, versionId, years, signal),
    enabled: Boolean(organizationId) && years >= 1 && years <= 100,
  });
  return (
    <div className="flex flex-col gap-3">
      <Field label="Keep it for (years)">{(p) => <Input {...p} type="number" min={1} max={100} value={years} onChange={(e) => setYears(Number(e.target.value))} />}</Field>
      {cost.data && (
        <>
          <Table>
            <thead>
              <tr>
                <Th>Tier</Th>
                <Th className="text-right">A year</Th>
                <Th className="text-right">{`Over ${cost.data.years} years`}</Th>
              </tr>
            </thead>
            <tbody>
              {cost.data.tiers.map((t) => (
                <tr key={t.tier}>
                  <Td className="text-sm">{t.tier}</Td>
                  <Td className="text-right text-sm tabular-nums">{usd(t.per_year)}</Td>
                  <Td className="text-right text-sm tabular-nums">{usd(t.total)}</Td>
                </tr>
              ))}
            </tbody>
          </Table>
          <p className="text-sm text-muted">AWS list prices. {cost.data.note}</p>
        </>
      )}
    </div>
  );
}

// Publishing is public, so the form says what readers will and will not see.
function PublishPanel({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const key = orgKey(organizationId, "domain", "publication", versionId);
  const current = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => getPublication(id, versionId, signal),
    enabled: Boolean(organizationId),
    retry: false,
  });
  const refresh = () => client.invalidateQueries({ queryKey: key });
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [creator, setCreator] = useState("");
  const [affiliation, setAffiliation] = useState("");
  const [orcid, setOrcid] = useState("");
  const [publisher, setPublisher] = useState("");
  const [license, setLicense] = useState("");
  const [wantDOI, setWantDOI] = useState(false);
  const [withdrawing, setWithdrawing] = useState(false);
  const [reason, setReason] = useState("");

  const create = useMutation({
    mutationFn: () =>
      publish(id, versionId, {
        title,
        description,
        publisher,
        license,
        mint_doi: wantDOI,
        creators: creator
          .split(";")
          .map((name) => name.trim())
          .filter(Boolean)
          .map((name) => ({ name, affiliation, orcid })),
      }),
    onSuccess: (r) => {
      notify.success(r.publication.doi_error ? "Published, but the DOI was not registered" : "Published");
      refresh();
    },
    onError: notify.error,
  });
  const retry = useMutation({ mutationFn: () => mintDOI(id, versionId), onSuccess: refresh, onError: notify.error });
  const pull = useMutation({
    mutationFn: () => withdraw(id, versionId, reason),
    onSuccess: () => {
      setWithdrawing(false);
      refresh();
    },
    onError: notify.error,
  });

  const p = current.data?.publication;
  const description_ = p
    ? "Its landing page is public. File names and data stay private."
    : "Gives this snapshot a public landing page with a citation. Readers see the title, creators, licence, file count, size and manifest hash. File names and data stay private.";

  return (
    <Panel title="Publish" description={description_}>
      {current.isPending ? (
        <SkeletonRows rows={2} />
      ) : current.isError ? (
        <ErrorState error={current.error} onRetry={() => current.refetch()} />
      ) : p && current.data ? (
        <div className="flex flex-col gap-4">
          {p.withdrawn_at && (
            <p className="text-base">
              <StateBadge tone="warning">Withdrawn</StateBadge> <span className="break-words">{p.withdrawn_reason}</span>
            </p>
          )}
          <DefList
            items={[
              { label: "Title", value: p.title },
              { label: "Licence", value: p.license },
              {
                label: "Landing page",
                value: (
                  <a href={current.data.landing_url} className="break-all underline underline-offset-2">
                    {current.data.landing_url}
                  </a>
                ),
              },
              {
                label: "DOI",
                value: p.doi ? (
                  <a href={`https://doi.org/${p.doi}`} className="mono-data break-all underline underline-offset-2">
                    {p.doi}
                  </a>
                ) : (
                  <span className="text-muted">None</span>
                ),
              },
              { label: "Published", value: <Time value={p.created_at} /> },
            ]}
          />
          {p.doi_error && <p className="text-base break-words">The DOI was not registered: {p.doi_error}</p>}
          {can("datasets.manage") && (
            <div className="flex flex-wrap gap-2">
              {!p.doi && !p.withdrawn_at && (
                <Button variant="secondary" onClick={() => retry.mutate()} loading={retry.isPending}>
                  {p.doi_error ? "Try the DOI again" : "Register a DOI"}
                </Button>
              )}
              {!p.withdrawn_at && !withdrawing && (
                <Button variant="ghost" onClick={() => setWithdrawing(true)}>
                  Withdraw
                </Button>
              )}
            </div>
          )}
          {withdrawing && (
            <form
              className="flex flex-col gap-3"
              onSubmit={(e: FormEvent) => {
                e.preventDefault();
                pull.mutate();
              }}
            >
              <Field label="Why it is withdrawn" hint="Readers will see this. The page stays, because a DOI must keep resolving, and the bytes are not touched.">
                {(f) => <Textarea {...f} value={reason} onChange={(e) => setReason(e.target.value)} rows={2} maxLength={2000} />}
              </Field>
              <div className="flex flex-wrap gap-2">
                <SubmitButton valid={reason.trim() !== ""} loading={pull.isPending} hint="Say why.">
                  Withdraw publication
                </SubmitButton>
                <Button type="button" variant="ghost" onClick={() => setWithdrawing(false)}>
                  Keep it published
                </Button>
              </div>
            </form>
          )}
          <RetentionCost id={id} versionId={versionId} />
        </div>
      ) : can("datasets.manage") ? (
        <div className="grid gap-6 lg:grid-cols-2">
          <form
            className="flex flex-col gap-3"
            onSubmit={(e: FormEvent) => {
              e.preventDefault();
              create.mutate();
            }}
          >
            <Field label="Title">{(f) => <Input {...f} value={title} onChange={(e) => setTitle(e.target.value)} required maxLength={500} autoComplete="off" />}</Field>
            <Field label="Creators" hint="Names, separated by semicolons.">
              {(f) => <Input {...f} value={creator} onChange={(e) => setCreator(e.target.value)} required autoComplete="off" />}
            </Field>
            <Field label="Affiliation" hint="Optional, for all of them.">
              {(f) => <Input {...f} value={affiliation} onChange={(e) => setAffiliation(e.target.value)} autoComplete="off" />}
            </Field>
            <Field label="ORCID" hint="Optional, when there is one creator.">
              {(f) => <Input {...f} value={orcid} onChange={(e) => setOrcid(e.target.value)} pattern="\d{4}-\d{4}-\d{4}-\d{3}[\dX]" spellCheck={false} autoComplete="off" />}
            </Field>
            <Field label="Publisher">{(f) => <Input {...f} value={publisher} onChange={(e) => setPublisher(e.target.value)} required maxLength={500} autoComplete="organization" />}</Field>
            <Field label="Licence" hint="Leave empty to use the dataset's recorded licence.">
              {(f) => <Input {...f} value={license} onChange={(e) => setLicense(e.target.value)} maxLength={500} autoComplete="off" />}
            </Field>
            <Field label="Description">{(f) => <Textarea {...f} value={description} onChange={(e) => setDescription(e.target.value)} rows={3} maxLength={10000} />}</Field>
            <Checkbox checked={wantDOI} onChange={(e) => setWantDOI(e.target.checked)} label="Register a DOI with DataCite" />
            <div>
              <SubmitButton valid={Boolean(title.trim() && creator.trim() && publisher.trim())} loading={create.isPending} hint="Give it a title, creators and a publisher.">
                Publish this snapshot
              </SubmitButton>
            </div>
          </form>
          <RetentionCost id={id} versionId={versionId} />
        </div>
      ) : (
        <EmptyState title="Not published">Someone who manages datasets can publish this snapshot.</EmptyState>
      )}
    </Panel>
  );
}

function RecordingsPanel({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const recordings = useQuery({
    queryKey: orgKey(organizationId, "domain", "recordings", versionId),
    queryFn: ({ signal }) => listRecordings(id, versionId, signal),
    enabled: Boolean(organizationId),
  });
  const segmentsKey = orgKey(organizationId, "domain", "segments", versionId);
  const segments = useQuery({
    queryKey: segmentsKey,
    queryFn: ({ signal }) => listSegments(id, versionId, signal),
    enabled: Boolean(organizationId),
  });
  const refresh = () => client.invalidateQueries({ queryKey: segmentsKey });
  const list = recordings.data?.recordings ?? [];
  const marked = segments.data?.segments ?? [];
  const [path, setPath] = useState("");
  const [start, setStart] = useState("");
  const [end, setEnd] = useState("");
  const [label, setLabel] = useState("");
  const [deleting, setDeleting] = useState<Segment | null>(null);
  const startOf = new Map(list.map((r) => [r.path, r.start_ns]));
  const add = useMutation({
    // ponytail: a JavaScript number rounds nanoseconds since the epoch to about
    // 256 ns; the SDK sends exact integers when that matters.
    mutationFn: () => {
      const origin = startOf.get(path) ?? 0;
      return addSegment(id, versionId, { path, start_ns: origin + Number(start) * 1e9, end_ns: origin + Number(end) * 1e9, label, note: "" });
    },
    onSuccess: () => {
      setStart("");
      setEnd("");
      setLabel("");
      refresh();
    },
    onError: notify.error,
  });

  if (recordings.isPending) return null;
  // Most snapshots hold no recordings and no segments; then this says nothing.
  if (!recordings.isError && list.length === 0 && marked.length === 0) return null;

  return (
    <Panel title="Recordings and segments" description="MCAP files and rosbag2 folders, as their own summaries describe them. Segments mark a time range inside a file and leave the manifest as it was.">
      <div className="flex flex-col gap-5">
        {recordings.isError ? (
          <ErrorState error={recordings.error} onRetry={() => recordings.refetch()} />
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>File</Th>
                <Th className="text-right">Length</Th>
                <Th className="text-right">Messages</Th>
                <Th>Topics</Th>
              </tr>
            </thead>
            <tbody>
              {list.map((r) => (
                <tr key={r.asset_id}>
                  <Td>
                    <span className="mono-data block break-all text-sm">{r.path}</span>
                    <span className="text-sm text-muted">{r.kind === "MCAP" ? "MCAP" : "rosbag2"}</span>
                  </Td>
                  <Td className="text-right text-sm tabular-nums">{seconds(r.start_ns, r.end_ns)}</Td>
                  <Td className="text-right text-sm tabular-nums">{formatNumber(r.message_count)}</Td>
                  <Td className="text-sm">
                    {r.topics.map((t) => (
                      <span key={t.topic} className="block break-all">
                        <span className="mono-data">{t.topic}</span> <span className="text-muted">{t.schema}</span>
                      </span>
                    ))}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        {marked.length > 0 && (
          <Table>
            <thead>
              <tr>
                <Th>Segment</Th>
                <Th>File</Th>
                <Th className="text-right">Seconds into the file</Th>
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              </tr>
            </thead>
            <tbody>
              {marked.map((s) => (
                <tr key={s.id}>
                  <Td className="text-sm">{s.label || <span className="text-muted">No label</span>}</Td>
                  <Td className="mono-data break-all text-sm">{s.path}</Td>
                  <Td className="text-right text-sm tabular-nums">
                    {secondsInto(s.start_ns, startOf.get(s.path))} to {secondsInto(s.end_ns, startOf.get(s.path))}
                  </Td>
                  <Td>
                    {can("datasets.write") && (
                      <Button variant="ghost" onClick={() => setDeleting(s)} aria-label={`Delete segment ${s.label || s.id}`}>
                        Delete
                      </Button>
                    )}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        {can("datasets.write") && list.length > 0 && (
          <form
            className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5 lg:items-end"
            onSubmit={(e: FormEvent) => {
              e.preventDefault();
              add.mutate();
            }}
          >
            <Field label="File">
              {(f) => (
                <Select {...f} value={path} onChange={(e) => setPath(e.target.value)} required>
                  <option value="">Choose…</option>
                  {list.map((r) => (
                    <option key={r.asset_id} value={r.path}>
                      {r.path}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
            <Field label="From (seconds)">{(f) => <Input {...f} type="number" min={0} step="any" value={start} onChange={(e) => setStart(e.target.value)} required autoComplete="off" />}</Field>
            <Field label="To (seconds)">{(f) => <Input {...f} type="number" min={0} step="any" value={end} onChange={(e) => setEnd(e.target.value)} required autoComplete="off" />}</Field>
            <Field label="Label">{(f) => <Input {...f} value={label} onChange={(e) => setLabel(e.target.value)} maxLength={200} autoComplete="off" />}</Field>
            <div>
              <SubmitButton valid={Boolean(path && start && end)} loading={add.isPending} hint="Choose a file and a range.">
                Add segment
              </SubmitButton>
            </div>
          </form>
        )}
      </div>
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={`Delete ${deleting?.label || "this segment"}?`}
        description="Vectors computed over it go with it. The file is not touched."
        confirmLabel="Delete segment"
        onConfirm={async () => {
          if (deleting) await deleteSegment(id, versionId, deleting.id);
          setDeleting(null);
          refresh();
        }}
      />
    </Panel>
  );
}

function IndexesPanel({ id, versionId }: { id: string; versionId: string }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const client = useQueryClient();
  const key = orgKey(organizationId, "domain", "indexes", versionId);
  const indexes = useQuery({ queryKey: key, queryFn: ({ signal }) => listIndexes(id, versionId, signal), enabled: Boolean(organizationId) });
  const refresh = () => client.invalidateQueries({ queryKey: key });
  const [name, setName] = useState("");
  const [model, setModel] = useState("");
  const [dimensions, setDimensions] = useState("512");
  const [metric, setMetric] = useState("COSINE");
  const [deleting, setDeleting] = useState<EmbeddingIndex | null>(null);
  const create = useMutation({
    mutationFn: () => createIndex(id, versionId, { name, model, dimensions: Number(dimensions), metric }),
    onSuccess: () => {
      setName("");
      setModel("");
      refresh();
    },
    onError: notify.error,
  });
  const list = indexes.data?.indexes ?? [];
  return (
    <Panel title="Embedding indexes" description="Vectors from your own model, over this snapshot's files or segments. The platform computes none; send and query them with the SDK.">
      <div className="flex flex-col gap-4">
        {indexes.isPending ? (
          <SkeletonRows rows={2} />
        ) : indexes.isError ? (
          <ErrorState error={indexes.error} onRetry={() => indexes.refetch()} />
        ) : list.length === 0 ? (
          <EmptyState title="No indexes yet">Create one here, then use domain.put_vectors and domain.query from the Python SDK.</EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Index</Th>
                <Th className="text-right">Vectors</Th>
                <Th>Shape</Th>
                <Th>Id</Th>
                <Th>
                  <span className="sr-only">Actions</span>
                </Th>
              </tr>
            </thead>
            <tbody>
              {list.map((x) => (
                <tr key={x.id}>
                  <Td>
                    <span className="block font-medium">{x.name}</span>
                    {x.model && <span className="block text-sm text-muted">{x.model}</span>}
                  </Td>
                  <Td className="text-right text-sm tabular-nums">{formatNumber(x.vectors)}</Td>
                  <Td className="text-sm">
                    {x.dimensions} dimensions, {x.metric.toLowerCase()}
                  </Td>
                  <Td>
                    <CopyButton value={x.id} variant="ghost" />
                  </Td>
                  <Td>
                    {can("datasets.write") && (
                      <Button variant="ghost" onClick={() => setDeleting(x)} aria-label={`Delete index ${x.name}`}>
                        Delete
                      </Button>
                    )}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        {can("datasets.write") && (
          <form
            className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5 lg:items-end"
            onSubmit={(e: FormEvent) => {
              e.preventDefault();
              create.mutate();
            }}
          >
            <Field label="Name">{(f) => <Input {...f} value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} autoComplete="off" />}</Field>
            <Field label="Model">{(f) => <Input {...f} value={model} onChange={(e) => setModel(e.target.value)} maxLength={200} autoComplete="off" />}</Field>
            <Field label="Dimensions">{(f) => <Input {...f} type="number" min={1} max={4096} value={dimensions} onChange={(e) => setDimensions(e.target.value)} required />}</Field>
            <Field label="Metric">
              {(f) => (
                <Select {...f} value={metric} onChange={(e) => setMetric(e.target.value)}>
                  <option value="COSINE">Cosine</option>
                  <option value="DOT">Dot product</option>
                  <option value="EUCLIDEAN">Euclidean</option>
                </Select>
              )}
            </Field>
            <div>
              <SubmitButton valid={Boolean(name.trim() && Number(dimensions) > 0)} loading={create.isPending} hint="Name it and give its dimensions.">
                Create index
              </SubmitButton>
            </div>
          </form>
        )}
      </div>
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => !open && setDeleting(null)}
        title={`Delete ${deleting?.name ?? "this index"}?`}
        description="Its vectors go with it. The snapshot's files are not touched."
        confirmLabel="Delete index"
        onConfirm={async () => {
          if (deleting) await deleteIndex(deleting.id);
          setDeleting(null);
          refresh();
        }}
      />
    </Panel>
  );
}

function StacPanel({ id, versionId, name }: { id: string; versionId: string; name: string }) {
  const save = useMutation({
    mutationFn: async (what: "collection" | "items") => {
      const data = what === "collection" ? await stacCollection(id, versionId) : await stacItems(id, versionId);
      saveJSON(`${name}-${what}.json`, data);
    },
    onError: notify.error,
  });
  return (
    <Panel title="STAC" description="This snapshot as a STAC catalogue for geospatial tools, with each file's size and SHA-256.">
      <div className="flex flex-wrap gap-2">
        <Button variant="secondary" onClick={() => save.mutate("collection")} loading={save.isPending && save.variables === "collection"}>
          Download collection
        </Button>
        <Button variant="secondary" onClick={() => save.mutate("items")} loading={save.isPending && save.variables === "items"}>
          Download items
        </Button>
      </div>
    </Panel>
  );
}

export function VersionDomain({ id, versionId, version }: { id: string; versionId: string; version: number }) {
  return (
    <>
      <PublishPanel id={id} versionId={versionId} />
      <RecordingsPanel id={id} versionId={versionId} />
      <IndexesPanel id={id} versionId={versionId} />
      <StacPanel id={id} versionId={versionId} name={`version-${version}`} />
    </>
  );
}
