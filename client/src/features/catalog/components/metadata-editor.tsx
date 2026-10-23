"use client";

import { Plus, X } from "@phosphor-icons/react";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { SubmitButton } from "@/components/ui/submit-button";
import { notify } from "@/components/ui/toast";
import { describeError, isApiError } from "@/lib/api/errors";
import { useEditMetadata } from "../hooks";
import { buildPatch, MAX_KEYS, validateKey, validateValue } from "../metadata";
import type { CatalogObject } from "../types";

type Row = { id: number; key: string; value: string };

function rowsFrom(metadata: Record<string, string>): Row[] {
  return Object.entries(metadata).map(([key, value], i) => ({ id: i, key, value }));
}

export function MetadataEditor({ object, canEdit }: { object: CatalogObject; canEdit: boolean }) {
  const [rows, setRows] = useState(() => rowsFrom(object.metadata));
  const [nextId, setNextId] = useState(1000);
  const [formError, setFormError] = useState<string | null>(null);
  const edit = useEditMetadata(object.id);

  function update(id: number, part: "key" | "value", value: string) {
    setRows((current) => current.map((row) => (row.id === id ? { ...row, [part]: value } : row)));
  }

  // Checked on every render, so the save button knows at once.
  const keyErrors: Record<number, string> = {};
  const valueErrors: Record<number, string> = {};
  const seen = new Set<string>();
  for (const row of rows) {
    const key = row.key.trim();
    const keyProblem = validateKey(key);
    if (keyProblem) keyErrors[row.id] = keyProblem;
    else if (seen.has(key)) keyErrors[row.id] = `The key ${key} is used twice.`;
    seen.add(key);
    const valueProblem = validateValue(row.value);
    if (valueProblem) valueErrors[row.id] = valueProblem;
  }
  const edited = Object.fromEntries(rows.map((row) => [row.key.trim(), row.value]));
  const patch = buildPatch(object.metadata, edited);
  const hasErrors = Object.keys(keyErrors).length + Object.keys(valueErrors).length > 0;
  const tooMany = rows.length > MAX_KEYS;
  const changed = Object.keys(patch).length > 0;
  const valid = !hasErrors && !tooMany && changed;
  let hint = "Change a key or a value to save.";
  if (hasErrors) hint = "Fix the keys and values marked above to save.";
  if (tooMany) hint = `An object can hold at most ${MAX_KEYS} metadata keys.`;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    try {
      const saved = await edit.mutateAsync({ revision: object.revision, metadata: patch });
      setRows(rowsFrom(saved.metadata));
      notify.success("Metadata saved");
    } catch (e) {
      if (isApiError(e) && e.status === 412) {
        setFormError("Someone else changed this object while you were editing. It has been reloaded. Your edits are still here: check them and save again.");
      } else {
        setFormError(describeError(e));
      }
    }
  }

  if (!canEdit) {
    const entries = Object.entries(object.metadata);
    return (
      <Panel title="Metadata" description="Your role can read metadata but not change it.">
        {entries.length === 0 ? (
          <p className="text-base text-muted">No metadata is set.</p>
        ) : (
          <dl className="flex flex-col gap-2">
            {entries.map(([key, value]) => (
              <div key={key}>
                <dt className="mono-data text-muted">{key}</dt>
                <dd className="[overflow-wrap:anywhere]">{value}</dd>
              </div>
            ))}
          </dl>
        )}
      </Panel>
    );
  }

  return (
    <Panel title="Metadata" description="Free-form keys you attach to this object. Changes apply to the object, not to one version.">
      <form onSubmit={submit} className="flex flex-col gap-4" aria-label="Edit metadata" noValidate>
        {rows.length === 0 && <p className="text-base text-muted">No metadata is set. Add a key below.</p>}
        {rows.map((row, index) => (
          <div key={row.id} className="flex flex-col gap-2">
            <div className="flex flex-wrap items-start gap-2">
              <Field label={`Key ${index + 1}`} required error={keyErrors[row.id]} className="w-44">
                {(props) => (
                  <Input
                    {...props}
                    className="mono-data"
                    value={row.key}
                    onChange={(e) => update(row.id, "key", e.target.value)}
                    autoComplete="off"
                    maxLength={64}
                  />
                )}
              </Field>
              <Field label={`Value ${index + 1}`} className="min-w-40 flex-1" error={valueErrors[row.id]}>
                {(props) => (
                  <Input
                    {...props}
                    value={row.value}
                    onChange={(e) => update(row.id, "value", e.target.value)}
                    maxLength={1024}
                    autoComplete="off"
                  />
                )}
              </Field>
              <Button
                variant="ghost"
                className="mt-6"
                aria-label={`Remove key ${row.key || index + 1}`}
                onClick={() => setRows((current) => current.filter((r) => r.id !== row.id))}
              >
                <X aria-hidden />
              </Button>
            </div>
          </div>
        ))}
        <div>
          <Button
            disabled={rows.length >= MAX_KEYS}
            onClick={() => {
              setRows((current) => [...current, { id: nextId, key: "", value: "", }]);
              setNextId(nextId + 1);
            }}
          >
            <Plus aria-hidden /> Add key
          </Button>
        </div>
        {formError && (
          <p role="alert" className="text-base font-medium">
            {formError}
          </p>
        )}
        <div>
          <SubmitButton valid={valid} loading={edit.isPending} hint={hint}>
            Save metadata
          </SubmitButton>
        </div>
      </form>
    </Panel>
  );
}
