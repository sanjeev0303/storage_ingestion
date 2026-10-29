"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useMemo, useState, type FormEvent } from "react";
import { CopyButton } from "@/components/ui/copy-button";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { Field, Input } from "@/components/ui/field";
import { LoadMore } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { describeError, isApiError } from "@/lib/api/errors";
import { cleanText } from "@/lib/utils/forgive";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { lookupObject } from "../api";
import { cleanPath, pathProblem } from "../clean";
import { countActiveFilters, filtersToParams, parseFilters, type CatalogFilters } from "../filters";
import { useCapabilities, useCatalogList } from "../hooks";
import type { CatalogObject } from "../types";
import { FilterBar } from "./filter-bar";

function Capabilities() {
  const { data } = useCapabilities();
  if (!data) return null;
  return (
    <p className="neo-in-sm px-4 py-3 text-base">
      Versioning: logical versions are {data.logical_versioning ? "supported" : "not supported"}; your default storage
      connection reports versioning as {data.provider_versioning ? "on" : "off"}
      {data.provider_versioning_status ? ` (${data.provider_versioning_status})` : ""}.
    </p>
  );
}

function Lookup() {
  const router = useRouter();
  const [dataset, setDataset] = useState("");
  const [path, setPath] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const cleanDataset = cleanText(dataset);
  const cleanedPath = cleanPath(path);
  const pathError = pathProblem(cleanedPath) ?? undefined;
  const valid = cleanedPath !== "" && !pathError;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    setError(null);
    setBusy(true);
    try {
      const found = await lookupObject(cleanDataset, cleanedPath);
      router.push(`/app/catalog/${encodeURIComponent(found.id)}`);
    } catch (e) {
      setError(isApiError(e) && e.isNotFound ? "No object has that dataset and path." : describeError(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} aria-label="Find one object by path" className="flex flex-wrap items-start gap-3">
      <Field label="Dataset of the object" hint="Leave empty for the root namespace.">
        {(props) => (
          <Input
            {...props}
            value={dataset}
            onChange={(e) => {
              setDataset(e.target.value);
              setError(null);
            }}
            autoComplete="off"
          />
        )}
      </Field>
      <Field
        label="Exact path"
        required
        error={pathError ?? (path.trim() && !cleanedPath ? "Enter the path of the object." : error ?? undefined)}
        className="min-w-64 flex-1"
      >
        {(props) => (
          <Input
            {...props}
            value={path}
            onChange={(e) => {
              setPath(e.target.value);
              setError(null);
            }}
            maxLength={1024}
            required
            autoComplete="off"
          />
        )}
      </Field>
      <div className="mt-6">
        <SubmitButton valid={valid} loading={busy} hint="Enter the exact path of the object.">
          Open object
        </SubmitButton>
      </div>
    </form>
  );
}

function ShortSha({ sha }: { sha?: string }) {
  if (!sha) return <span className="text-muted">None</span>;
  return (
    <span className="flex items-center gap-2">
      <span className="mono-data whitespace-nowrap" title={sha}>
        {sha.slice(0, 12)}
      </span>
      <CopyButton value={sha} label="Copy" variant="ghost" />
    </span>
  );
}

function ObjectRow({ object }: { object: CatalogObject }) {
  const current = object.current_version;
  return (
    <tr>
      <Td className="min-w-40">
        <Link href={`/app/catalog/${encodeURIComponent(object.id)}`} className="font-medium underline-offset-2 hover:underline [overflow-wrap:anywhere]">
          {object.path}
        </Link>
        <div className="mt-0.5 text-sm text-muted">{object.dataset ? `Dataset ${object.dataset}` : "Root"}</div>
        {object.deleted_at && (
          <div className="mt-1">
            <StateBadge tone="attention">Deleted</StateBadge>
          </div>
        )}
      </Td>
      <Td className="whitespace-nowrap tabular-nums">{current ? formatBytes(current.size_bytes) : "No version"}</Td>
      <Td>{current?.content_type ?? <span className="text-muted">None</span>}</Td>
      <Td>
        <div className="mb-1 text-xs text-muted">SHA-256</div>
        <ShortSha sha={current?.checksum_sha256} />
      </Td>
      <Td className="tabular-nums">{formatNumber(object.versions)}</Td>
      <Td className="whitespace-nowrap">
        <Time value={current?.published_at} />
      </Td>
      <Td className="whitespace-nowrap">
        <Time value={object.updated_at} />
      </Td>
    </tr>
  );
}

function ListError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  if (isApiError(error) && (error.code === "query_too_broad" || error.status === 422)) {
    return (
      <div role="alert" className="neo-in flex flex-col items-start gap-2 p-8">
        <h3 className="text-md font-semibold">That filter is too broad</h3>
        <p className="max-w-prose text-base text-muted">
          The catalog could not answer within its time limit. Narrow it with a dataset, a longer path prefix, or a
          tighter date or size range, then apply the filters again.
        </p>
      </div>
    );
  }
  return <ErrorState error={error} onRetry={onRetry} />;
}

export function CatalogList() {
  const router = useRouter();
  const search = useSearchParams();
  const query = search.toString();
  const filters = useMemo(() => parseFilters(new URLSearchParams(query)), [query]);
  const list = useCatalogList(query);
  const active = countActiveFilters(filters);
  const pageError = list.isFetchNextPageError ? list.error : null;
  const fetchNext = list.fetchNextPage;

  function apply(next: CatalogFilters) {
    const qs = filtersToParams(next).toString();
    router.push(qs ? `/app/catalog?${qs}` : "/app/catalog");
  }

  return (
    <div className="flex flex-col gap-6">
      <Capabilities />
      <Panel title="Find an object" description="The catalog has no free-text search. Look one up by its exact path, or filter by the fields below.">
        <Lookup />
      </Panel>
      <Panel title="Filter" description={active ? `${active} filter${active === 1 ? "" : "s"} applied. Changing a filter starts the list again from the top.` : undefined}>
        <FilterBar key={query} filters={filters} onApply={apply} />
      </Panel>

      <section aria-label="Objects">
        {list.isPending && <SkeletonRows rows={8} />}
        {list.isError && list.items.length === 0 && <ListError error={list.error} onRetry={() => list.refetch()} />}
        {list.isSuccess && list.items.length === 0 && (
          <EmptyState title={active ? "No objects match these filters" : "The catalog is empty"}>
            {active
              ? "Loosen a filter, or include objects with no published version."
              : "Objects appear here once an upload has been published. Start one under Ingestion."}
          </EmptyState>
        )}
        {list.items.length > 0 && (
          <>
            <Table aria-label="Catalog objects">
              <thead>
                <tr>
                  <Th>Path</Th>
                  <Th>Size</Th>
                  <Th>Content type</Th>
                  <Th>Checksum</Th>
                  <Th>Versions</Th>
                  <Th>Published</Th>
                  <Th>Updated</Th>
                </tr>
              </thead>
              <tbody>
                {list.items.map((object) => (
                  <ObjectRow key={object.id} object={object} />
                ))}
              </tbody>
            </Table>
            <LoadMore
              shown={list.items.length}
              hasNext={Boolean(list.hasNextPage)}
              loading={list.isFetchingNextPage}
              onLoad={() => list.fetchNextPage()}
            />
            {pageError && <ListError error={pageError} onRetry={() => fetchNext()} />}
          </>
        )}
      </section>
    </div>
  );
}
