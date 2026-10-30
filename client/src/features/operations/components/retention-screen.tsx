"use client";

import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input, Select, Textarea } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { PermissionDenied } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes } from "@/lib/utils/format";
import { isPending, listTemplates, placeLegalHold, previewRetentionPolicy, setRetentionPolicy } from "../api";
import type {
  HoldQuery,
  HoldScope,
  LockMode,
  PolicyImpact,
  RetentionPolicyRequest,
  RetentionPolicyResponse,
  RetentionScope,
} from "../types";
import { HoldsInEffect, PoliciesInEffect, readableDuration, useRetentionKeys } from "./retention-current";
import { MAX_REASON, validateDuration, validateReason, validateScopeKey } from "../validate";

const scopeLabels: Record<RetentionScope, string> = {
  TENANT: "Whole organization",
  DATASET: "One dataset",
  ASSET: "One asset",
};

const durationFields = [
  { key: "min_retention", label: "Minimum retention", hint: "How long an asset must be kept before it can be deleted." },
  { key: "max_age", label: "Maximum age", hint: "Assets older than this expire. Empty keeps the current value." },
  { key: "deletion_grace", label: "Deletion grace", hint: "Time between a delete and the purge, at least 1h." },
  { key: "quarantine_retention", label: "Quarantine retention", hint: "How long held files are kept." },
] as const;

type DurationKey = (typeof durationFields)[number]["key"];

// Ids are pasted with stray spaces; dataset names must be lowercase anyway.
function cleanKey(scope: RetentionScope, key: string) {
  if (scope === "TENANT") return key.trim();
  return scope === "DATASET" ? key.trim().toLowerCase() : key.replace(/\s+/g, "");
}

// "1h 30m" and "720H" mean what they look like.
function cleanDuration(value: string) {
  return value.replace(/\s+/g, "").toLowerCase();
}

const METADATA_KEY = /^[A-Za-z0-9._-]{1,64}$/;

export function RetentionScreen() {
  const { can } = useSession();
  if (!can("retention.manage")) {
    return <PermissionDenied detail="Retention policies and legal holds need the retention.manage permission." />;
  }

  return (
    <div className="flex flex-col gap-6">
      <div className="grid gap-6 xl:grid-cols-2">
        <PoliciesInEffect />
        <HoldsInEffect />
      </div>
      <div className="grid gap-6 lg:grid-cols-2">
        <PolicyForm />
        <HoldPanel />
      </div>
    </div>
  );
}

function ScopeFields({
  scope,
  scopeKey,
  keyError,
  onScope,
  onKey,
}: {
  scope: RetentionScope;
  scopeKey: string;
  keyError?: string;
  onScope: (scope: RetentionScope) => void;
  onKey: (key: string) => void;
}) {
  return (
    <>
      <Field label="Scope" required>
        {(props) => (
          <Select {...props} name="scope" value={scope} onChange={(e) => onScope(e.target.value as RetentionScope)}>
            {(Object.keys(scopeLabels) as RetentionScope[]).map((value) => (
              <option key={value} value={value}>
                {scopeLabels[value]}
              </option>
            ))}
          </Select>
        )}
      </Field>
      {scope !== "TENANT" && <ScopeKeyField scope={scope} scopeKey={scopeKey} keyError={keyError} onKey={onKey} />}
    </>
  );
}

function ScopeKeyField({
  scope,
  scopeKey,
  keyError,
  onKey,
}: {
  scope: RetentionScope;
  scopeKey: string;
  keyError?: string;
  onKey: (key: string) => void;
}) {
  return (
    <Field
      label={scope === "DATASET" ? "Dataset name" : "Asset id"}
      required
      error={keyError}
      hint={scope === "DATASET" ? "Lowercase letters, digits, dots, underscores and hyphens." : undefined}
    >
      {(props) => (
        <Input
          {...props}
          name="scope_key"
          value={scopeKey}
          onChange={(e) => onKey(e.target.value)}
          className="mono-data"
          autoComplete="off"
          spellCheck={false}
        />
      )}
    </Field>
  );
}

function ReasonField({ value, error, onChange }: { value: string; error?: string; onChange: (v: string) => void }) {
  return (
    <Field label="Reason" required error={error} hint={`Written to the audit log. Up to ${MAX_REASON} characters.`}>
      {(props) => <Textarea {...props} name="reason" maxLength={MAX_REASON} value={value} onChange={(e) => onChange(e.target.value)} />}
    </Field>
  );
}

function useTemplates() {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, "retention", "templates"),
    queryFn: ({ signal }) => listTemplates(signal),
    enabled: Boolean(organizationId),
    staleTime: Infinity,
  });
}

function PolicyForm() {
  const [scope, setScope] = useState<RetentionScope>("TENANT");
  const [scopeKey, setScopeKey] = useState("");
  const [durations, setDurations] = useState<Record<DurationKey, string>>({
    min_retention: "",
    max_age: "",
    deletion_grace: "",
    quarantine_retention: "",
  });
  const [template, setTemplate] = useState("");
  const [lockMode, setLockMode] = useState<LockMode>("GOVERNANCE");
  const [retainFrom, setRetainFrom] = useState("");
  const [reason, setReason] = useState("");
  const [impact, setImpact] = useState<PolicyImpact | null>(null);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);
  const [result, setResult] = useState<RetentionPolicyResponse | null>(null);
  const templates = useTemplates();
  const refresh = useRetentionKeys();

  const chosen = templates.data?.templates.find((t) => t.id === template);
  const key = cleanKey(scope, scopeKey);
  const keyError = validateScopeKey(scope, key);
  const reasonError = validateReason(reason);
  const durationErrors = durationFields.map((f) => validateDuration(cleanDuration(durations[f.key])));
  const setAny = durationFields.some((f) => cleanDuration(durations[f.key]) !== "") || Boolean(chosen);
  const noDuration = setAny ? null : "Set at least one duration, or choose a template.";
  const retainFromError =
    retainFrom !== "" && !METADATA_KEY.test(retainFrom) ? "A metadata key: letters, digits, dot, underscore and hyphen." : null;
  const compliance = lockMode === "COMPLIANCE";
  const problem = keyError || reasonError || durationErrors.find(Boolean) || noDuration || retainFromError;

  function body(): RetentionPolicyRequest {
    const out: RetentionPolicyRequest = { scope, reason: reason.trim(), lock_mode: lockMode };
    if (scope !== "TENANT") out.scope_key = key;
    if (template) out.template = template;
    if (retainFrom) out.retain_from_key = retainFrom;
    for (const f of durationFields) {
      const value = cleanDuration(durations[f.key]);
      if (value) out[f.key] = value;
    }
    return out;
  }

  // The dry run answers what the change covers and whether it loosens
  // protection, so the confirmation says so instead of guessing.
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (problem) return;
    setPreviewError(null);
    setChecking(true);
    try {
      const preview = await previewRetentionPolicy(body());
      setImpact(preview.impact);
    } catch (error) {
      setPreviewError(describeError(error));
    } finally {
      setChecking(false);
    }
  }

  async function save() {
    const answer = await setRetentionPolicy({ ...body(), confirm_compliance: compliance || undefined });
    await refresh();
    if (isPending(answer)) {
      notify.info("Sent for approval", "A second member with retention.manage approves it under Governance.");
      return;
    }
    setResult(answer);
  }

  return (
    <Panel title="Set a retention policy" description="A new version applies immediately. Versions are kept, never edited.">
      <form onSubmit={submit} noValidate className="flex flex-col gap-4">
        <ScopeFields
          scope={scope}
          scopeKey={scopeKey}
          keyError={keyError ?? undefined}
          onScope={(next) => {
            setScope(next);
            setScopeKey("");
          }}
          onKey={setScopeKey}
        />
        <Field
          label="Start from a template"
          hint={
            chosen
              ? `${chosen.citation}. ${chosen.note} A starting point: confirm with counsel that it fits.`
              : "Optional. Fills the minimum retention, lock mode and start date it names."
          }
        >
          {(props) => (
            <Select
              {...props}
              name="template"
              value={template}
              onChange={(e) => {
                const next = templates.data?.templates.find((t) => t.id === e.target.value);
                setTemplate(e.target.value);
                if (next) {
                  setLockMode(next.lock_mode);
                  setRetainFrom(next.retain_from_key ?? "");
                }
              }}
            >
              <option value="">No template</option>
              {templates.data?.templates.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name} ({readableDuration(t.min_retention)})
                </option>
              ))}
            </Select>
          )}
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          {durationFields.map((f, i) => (
            <Field
              key={f.key}
              label={f.label}
              hint={
                i === 0 && chosen && !durations.min_retention ? `${readableDuration(chosen.min_retention)} from the template.` : f.hint
              }
              error={durationErrors[i] ?? (i === 0 ? noDuration : null) ?? undefined}
            >
              {(props) => (
                <Input
                  {...props}
                  name={f.key}
                  value={durations[f.key]}
                  onChange={(e) => setDurations({ ...durations, [f.key]: e.target.value })}
                  placeholder="720h"
                  className="mono-data"
                  autoComplete="off"
                  spellCheck={false}
                />
              )}
            </Field>
          ))}
          <Field
            label="Object Lock mode"
            hint={
              compliance
                ? "Compliance: nobody, the bucket owner included, can delete or shorten a locked version before its date."
                : "Governance: only an identity allowed to bypass it can lift a lock. The platform never does."
            }
          >
            {(props) => (
              <Select {...props} name="lock_mode" value={lockMode} onChange={(e) => setLockMode(e.target.value as LockMode)}>
                <option value="GOVERNANCE">Governance</option>
                <option value="COMPLIANCE">Compliance</option>
              </Select>
            )}
          </Field>
          <Field
            label="Count retention from"
            error={retainFromError ?? undefined}
            hint="A date in each asset's metadata, such as trade_date or study_end_date. Empty counts from creation; a date never shortens it."
          >
            {(props) => (
              <Input
                {...props}
                name="retain_from_key"
                value={retainFrom}
                onChange={(e) => setRetainFrom(e.target.value.trim())}
                placeholder="study_end_date…"
                className="mono-data"
                autoComplete="off"
                spellCheck={false}
              />
            )}
          </Field>
        </div>
        <p className="text-sm text-muted">
          Durations are Go-style: 720h, 1h30m. Only ns, us, ms, s, m and h exist, so write 7 days as 168h. Leave a field
          empty to inherit.
        </p>
        <ReasonField value={reason} error={reasonError ?? undefined} onChange={setReason} />
        {previewError && (
          <p role="alert" className="text-base font-medium">
            {previewError}
          </p>
        )}
        <SubmitButton valid={!problem} loading={checking} hint={problem ?? undefined}>
          Review the change
        </SubmitButton>
      </form>

      {result && (
        <div role="status" className="neo-in-sm mt-5 p-3">
          <p className="font-semibold">Policy saved</p>
          <DefList
            items={[
              { label: "Scope", value: `${result.scope}${result.scope_key ? ` / ${result.scope_key}` : ""}` },
              { label: "Version", value: result.version },
              { label: "Lock mode", value: result.lock_mode || "GOVERNANCE" },
              { label: "Effective from", value: <Time value={result.effective_from} /> },
              { label: "Policy id", value: <span className="mono-data">{result.id}</span> },
            ]}
          />
        </div>
      )}

      <ConfirmDialog
        open={impact !== null}
        onOpenChange={(open) => !open && setImpact(null)}
        title="Set this retention policy?"
        description={
          impact && (
            <span className="flex flex-col gap-2">
              <span>
                It covers {impact.assets.toLocaleString()} available {impact.assets === 1 ? "asset" : "assets"} (
                {formatBytes(impact.bytes)}) in {scopeLabels[scope].toLowerCase()}
                {scope !== "TENANT" && key ? ` ${key}` : ""}.
              </span>
              {impact.loosens && (
                <span className="font-medium text-ink">
                  It keeps data for less time, or less firmly, than the policy it replaces.
                  {impact.needs_approval && " A second member must approve it before it applies."}
                </span>
              )}
              {impact.notes?.map((n) => <span key={n}>{n}</span>)}
            </span>
          )
        }
        confirmLabel={impact?.needs_approval ? "Request approval" : "Set policy"}
        confirmText={compliance ? "COMPLIANCE" : undefined}
        onConfirm={save}
      />
    </Panel>
  );
}

const holdScopeLabels: Record<HoldScope, string> = { ...scopeLabels, QUERY: "Assets matching a query" };

function parseMetadata(text: string): { value?: Record<string, string>; error?: string } {
  const out: Record<string, string> = {};
  for (const line of text.split("\n").map((l) => l.trim()).filter(Boolean)) {
    const at = line.indexOf("=");
    if (at <= 0) return { error: "Write metadata as key=value, one per line." };
    out[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  return { value: Object.keys(out).length ? out : undefined };
}

function HoldPanel() {
  const [scope, setScope] = useState<HoldScope>("TENANT");
  const [scopeKey, setScopeKey] = useState("");
  const [reason, setReason] = useState("");
  const [matter, setMatter] = useState("");
  const [custodians, setCustodians] = useState("");
  const [query, setQuery] = useState({ dataset: "", path_prefix: "", metadata: "", created_after: "" });
  const [placing, setPlacing] = useState(false);
  const [placeError, setPlaceError] = useState<string | null>(null);
  const refresh = useRetentionKeys();

  const isQuery = scope === "QUERY";
  const key = isQuery ? "" : cleanKey(scope, scopeKey);
  const keyError = isQuery ? null : validateScopeKey(scope, key);
  const reasonError = validateReason(reason);
  const metadata = parseMetadata(query.metadata);
  const emptyQuery = isQuery && !query.dataset && !query.path_prefix && !metadata.value && !query.created_after;
  const queryError = isQuery
    ? (metadata.error ?? (emptyQuery ? "Set at least one condition." : null) ?? (!matter.trim() ? "Name the matter." : null))
    : null;
  const problem = keyError || reasonError || queryError;

  async function place(e: FormEvent) {
    e.preventDefault();
    if (problem) return;
    setPlaceError(null);
    setPlacing(true);
    try {
      const q: HoldQuery = {};
      if (query.dataset) q.dataset = query.dataset.trim().toLowerCase();
      if (query.path_prefix) q.path_prefix = query.path_prefix.trim();
      if (metadata.value) q.metadata = metadata.value;
      if (query.created_after) q.created_after = new Date(query.created_after).toISOString();
      await placeLegalHold({
        scope,
        reason: reason.trim(),
        ...(scope !== "TENANT" && !isQuery && { scope_key: key }),
        ...(isQuery && { query: q }),
        ...(matter.trim() && { matter: matter.trim() }),
        ...(custodians.trim() && { custodians: custodians.split(",").map((c) => c.trim()).filter(Boolean) }),
      });
      await refresh();
      notify.success("Legal hold placed");
      setReason("");
      setScopeKey("");
    } catch (error) {
      setPlaceError(describeError(error));
    } finally {
      setPlacing(false);
    }
  }

  return (
    <Panel title="Place a legal hold" description="A hold blocks deletion and purge for everything in its scope until it is released.">
      <form onSubmit={place} noValidate className="flex flex-col gap-4">
        <Field
          label="Scope"
          required
          hint={isQuery ? "The query is checked whenever an asset is, so files created after the hold are held too." : undefined}
        >
          {(props) => (
            <Select
              {...props}
              name="hold_scope"
              value={scope}
              onChange={(e) => {
                setScope(e.target.value as HoldScope);
                setScopeKey("");
              }}
            >
              {(Object.keys(holdScopeLabels) as HoldScope[]).map((value) => (
                <option key={value} value={value}>
                  {holdScopeLabels[value]}
                </option>
              ))}
            </Select>
          )}
        </Field>
        {!isQuery && scope !== "TENANT" && (
          <ScopeKeyField scope={scope} scopeKey={scopeKey} keyError={keyError ?? undefined} onKey={setScopeKey} />
        )}
        {isQuery && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Dataset">
              {(props) => (
                <Input
                  {...props}
                  name="query_dataset"
                  value={query.dataset}
                  autoComplete="off"
                  spellCheck={false}
                  className="mono-data"
                  onChange={(e) => setQuery({ ...query, dataset: e.target.value })}
                />
              )}
            </Field>
            <Field label="Path starts with">
              {(props) => (
                <Input
                  {...props}
                  name="query_path"
                  value={query.path_prefix}
                  autoComplete="off"
                  spellCheck={false}
                  className="mono-data"
                  placeholder="contracts/2026/…"
                  onChange={(e) => setQuery({ ...query, path_prefix: e.target.value })}
                />
              )}
            </Field>
            <Field label="Metadata" hint="key=value, one per line; all must match.">
              {(props) => (
                <Textarea
                  {...props}
                  name="query_metadata"
                  rows={2}
                  value={query.metadata}
                  spellCheck={false}
                  className="mono-data"
                  onChange={(e) => setQuery({ ...query, metadata: e.target.value })}
                />
              )}
            </Field>
            <Field label="Created on or after">
              {(props) => (
                <Input
                  {...props}
                  name="query_created_after"
                  type="datetime-local"
                  value={query.created_after}
                  onChange={(e) => setQuery({ ...query, created_after: e.target.value })}
                />
              )}
            </Field>
          </div>
        )}
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Matter" required={isQuery} hint="The case or investigation this hold is for.">
            {(props) => <Input {...props} name="matter" value={matter} autoComplete="off" onChange={(e) => setMatter(e.target.value)} />}
          </Field>
          <Field label="Custodians" hint="Comma-separated.">
            {(props) => (
              <Input {...props} name="custodians" value={custodians} autoComplete="off" onChange={(e) => setCustodians(e.target.value)} />
            )}
          </Field>
        </div>
        <ReasonField value={reason} error={reasonError ?? undefined} onChange={setReason} />
        {placeError && (
          <p role="alert" className="text-base font-medium">
            {placeError}
          </p>
        )}
        <SubmitButton valid={!problem} loading={placing} hint={problem ?? undefined}>
          Place legal hold
        </SubmitButton>
      </form>
    </Panel>
  );
}
