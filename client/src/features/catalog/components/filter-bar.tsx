"use client";

import { Plus, X } from "@phosphor-icons/react";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { SubmitButton } from "@/components/ui/submit-button";
import { cleanText, cleanWholeNumber, splitList } from "@/lib/utils/forgive";
import { cleanPath, pathProblem } from "../clean";
import { emptyFilters, MAX_METADATA_FILTERS, type CatalogFilters } from "../filters";
import { validateKey, validateValue } from "../metadata";

type Scope = "all" | "root" | "named";

function sizeProblem(value: string) {
  if (value.trim() && !cleanWholeNumber(value)) return "Enter a whole number of bytes, for example 1,000.";
  return undefined;
}

function idProblem(value: string) {
  if (splitList(value).length > 1) return "Enter one id.";
  return undefined;
}

function scopeOf(dataset: string | undefined): Scope {
  if (dataset === undefined) return "all";
  return dataset === "" ? "root" : "named";
}

export function FilterBar({ filters, onApply }: { filters: CatalogFilters; onApply: (next: CatalogFilters) => void }) {
  const [draft, setDraft] = useState(filters);
  const [scope, setScope] = useState<Scope>(scopeOf(filters.dataset));
  const [datasetName, setDatasetName] = useState(filters.dataset ?? "");

  function set<K extends keyof CatalogFilters>(key: K, value: CatalogFilters[K]) {
    setDraft((current) => ({ ...current, [key]: value }));
  }

  function setMetadata(index: number, part: "key" | "value", value: string) {
    set(
      "metadata",
      draft.metadata.map((row, i) => (i === index ? { ...row, [part]: value } : row)),
    );
  }

  const datasetError = scope === "named" && !cleanText(datasetName) ? "Enter a dataset name or pick another scope." : undefined;
  const prefixError = pathProblem(cleanPath(draft.prefix)) ?? undefined;
  const minError = sizeProblem(draft.min_size);
  const maxError = sizeProblem(draft.max_size);
  const sizeRangeError =
    !minError && !maxError && cleanWholeNumber(draft.min_size) && cleanWholeNumber(draft.max_size) &&
    Number(cleanWholeNumber(draft.min_size)) > Number(cleanWholeNumber(draft.max_size))
      ? "The maximum must not be below the minimum."
      : undefined;
  const createdError =
    draft.created_after && draft.created_before && draft.created_after > draft.created_before
      ? "Must not be earlier than the start date."
      : undefined;
  const updatedError =
    draft.updated_after && draft.updated_before && draft.updated_after > draft.updated_before
      ? "Must not be earlier than the start date."
      : undefined;
  const objectIdError = idProblem(draft.object_id);
  const assetIdError = idProblem(draft.asset_id);
  // Rows left completely empty are ignored when applying.
  const metadataErrors = draft.metadata.map((row, index) => {
    if (!row.key.trim() && !row.value.trim()) return null;
    const problem = validateKey(row.key.trim()) ?? validateValue(row.value);
    return problem ? `Metadata filter ${index + 1}: ${problem}` : null;
  });
  const metadataError = metadataErrors.find(Boolean) ?? undefined;
  const problems = [
    datasetError,
    prefixError,
    minError ?? maxError ?? sizeRangeError,
    createdError,
    updatedError,
    objectIdError,
    assetIdError,
    metadataError,
  ];
  const firstProblem = problems.find(Boolean);
  const valid = !firstProblem;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    const dataset = scope === "all" ? undefined : scope === "root" ? "" : cleanText(datasetName);
    onApply({
      ...draft,
      dataset,
      prefix: cleanPath(draft.prefix),
      content_type: cleanText(draft.content_type),
      min_size: cleanWholeNumber(draft.min_size),
      max_size: cleanWholeNumber(draft.max_size),
      object_id: splitList(draft.object_id).join(""),
      asset_id: splitList(draft.asset_id).join(""),
      metadata: draft.metadata
        .filter((row) => row.key.trim() || row.value.trim())
        .map((row) => ({ key: row.key.trim(), value: row.value.trim() })),
    });
  }

  function clear() {
    setScope("all");
    setDatasetName("");
    setDraft(emptyFilters);
    onApply(emptyFilters);
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4" aria-label="Catalog filters">
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Field label="Dataset">
          {(props) => (
            <Select {...props} value={scope} onChange={(e) => setScope(e.target.value as Scope)}>
              <option value="all">All datasets</option>
              <option value="root">Root only (no dataset)</option>
              <option value="named">One dataset</option>
            </Select>
          )}
        </Field>
        <Field label="Dataset name" hint="Used when the scope is One dataset." error={datasetError}>
          {(props) => (
            <Input
              {...props}
              value={datasetName}
              disabled={scope !== "named"}
              onChange={(e) => setDatasetName(e.target.value)}
              placeholder="training-images"
              autoComplete="off"
            />
          )}
        </Field>
        <Field label="Path prefix" hint="Matches from the start of the path, any letter case." error={prefixError}>
          {(props) => (
            <Input
              {...props}
              value={draft.prefix}
              onChange={(e) => set("prefix", e.target.value)}
              maxLength={1024}
              placeholder="2026/images/"
              autoComplete="off"
            />
          )}
        </Field>
        <Field label="Order">
          {(props) => (
            <Select {...props} value={draft.order} onChange={(e) => set("order", e.target.value as "path" | "updated")}>
              <option value="path">Path (dataset, then path)</option>
              <option value="updated">Last updated (oldest first)</option>
            </Select>
          )}
        </Field>
      </div>

      <details className="neo-in-sm p-4">
        <summary className="cursor-pointer text-base font-medium">More filters</summary>
        <div className="mt-4 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <Field label="Content type" hint="Exact match, for example image/png.">
            {(props) => (
              <Input {...props} value={draft.content_type} onChange={(e) => set("content_type", e.target.value)} autoComplete="off" />
            )}
          </Field>
          <Field label="Minimum size (bytes)" error={minError ?? sizeRangeError}>
            {(props) => (
              <Input {...props} inputMode="numeric" autoComplete="off" value={draft.min_size} onChange={(e) => set("min_size", e.target.value)} />
            )}
          </Field>
          <Field label="Maximum size (bytes)" error={maxError}>
            {(props) => (
              <Input {...props} inputMode="numeric" autoComplete="off" value={draft.max_size} onChange={(e) => set("max_size", e.target.value)} />
            )}
          </Field>
          <span aria-hidden className="hidden lg:block" />
          <Field label="Created on or after">
            {(props) => (
              <Input {...props} type="date" value={draft.created_after} onChange={(e) => set("created_after", e.target.value)} />
            )}
          </Field>
          <Field label="Created before" error={createdError}>
            {(props) => (
              <Input {...props} type="date" value={draft.created_before} onChange={(e) => set("created_before", e.target.value)} />
            )}
          </Field>
          <Field label="Updated on or after">
            {(props) => (
              <Input {...props} type="date" value={draft.updated_after} onChange={(e) => set("updated_after", e.target.value)} />
            )}
          </Field>
          <Field label="Updated before" error={updatedError}>
            {(props) => (
              <Input {...props} type="date" value={draft.updated_before} onChange={(e) => set("updated_before", e.target.value)} />
            )}
          </Field>
          <Field label="Object ID" error={objectIdError}>
            {(props) => (
              <Input {...props} className="mono-data" value={draft.object_id} onChange={(e) => set("object_id", e.target.value)} autoComplete="off" />
            )}
          </Field>
          <Field label="Version (asset) ID" hint="Finds the object that owns this version." error={assetIdError}>
            {(props) => (
              <Input {...props} className="mono-data" value={draft.asset_id} onChange={(e) => set("asset_id", e.target.value)} autoComplete="off" />
            )}
          </Field>
        </div>

        <fieldset className="mt-4">
          <legend className="text-sm font-medium">Metadata equals (up to {MAX_METADATA_FILTERS})</legend>
          <div className="mt-2 flex flex-col gap-2">
            {draft.metadata.map((row, index) => (
              <div key={index} className="flex flex-wrap items-center gap-2">
                <Input
                  aria-label={`Metadata filter ${index + 1} key`}
                  placeholder="key"
                  value={row.key}
                  onChange={(e) => setMetadata(index, "key", e.target.value)}
                  maxLength={64}
                  className="w-48"
                  autoComplete="off"
                />
                <Input
                  aria-label={`Metadata filter ${index + 1} value`}
                  placeholder="value"
                  value={row.value}
                  onChange={(e) => setMetadata(index, "value", e.target.value)}
                  maxLength={1024}
                  className="w-56"
                  autoComplete="off"
                />
                <Button
                  variant="ghost"
                  aria-label={`Remove metadata filter ${index + 1}`}
                  onClick={() => set("metadata", draft.metadata.filter((_, i) => i !== index))}
                >
                  <X aria-hidden />
                </Button>
              </div>
            ))}
            {draft.metadata.length < MAX_METADATA_FILTERS && (
              <div>
                <Button onClick={() => set("metadata", [...draft.metadata, { key: "", value: "" }])}>
                  <Plus aria-hidden /> Add metadata filter
                </Button>
              </div>
            )}
            {metadataError && (
              <p role="alert" className="text-sm font-medium">
                {metadataError}
              </p>
            )}
          </div>
        </fieldset>

        <Checkbox
          className="mt-4"
          label="Include objects with no published version, and deleted objects"
          checked={draft.include_unpublished}
          onChange={(e) => set("include_unpublished", e.target.checked)}
        />
      </details>

      <div className="flex flex-wrap gap-2">
        <SubmitButton valid={valid} hint={firstProblem}>
          Apply filters
        </SubmitButton>
        <Button onClick={clear}>Clear filters</Button>
      </div>
    </form>
  );
}
