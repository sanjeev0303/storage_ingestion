"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input, Select, Textarea } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { PermissionDenied } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { placeLegalHold, releaseLegalHold, setRetentionPolicy } from "../api";
import type { LegalHold, RetentionPolicyRequest, RetentionPolicyResponse, RetentionScope } from "../types";
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

export function RetentionScreen() {
  const { can } = useSession();
  if (!can("retention.manage")) {
    return <PermissionDenied detail="Retention policies and legal holds need the retention.manage permission." />;
  }

  return (
    <div className="flex flex-col gap-6">
      <p className="max-w-prose text-base text-muted">
        The API can write retention policies and legal holds but cannot list or read them back. This screen cannot show
        what is currently set, and legal holds stay invisible to organization owners by design. To see the effective
        result for one asset, look it up on the Assets tab and read its lifecycle.
      </p>
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
          <Select {...props} value={scope} onChange={(e) => onScope(e.target.value as RetentionScope)}>
            {(Object.keys(scopeLabels) as RetentionScope[]).map((value) => (
              <option key={value} value={value}>
                {scopeLabels[value]}
              </option>
            ))}
          </Select>
        )}
      </Field>
      {scope !== "TENANT" && (
        <Field
          label={scope === "DATASET" ? "Dataset name" : "Asset id"}
          required
          error={keyError}
          hint={scope === "DATASET" ? "Lowercase letters, digits, dots, underscores and hyphens." : undefined}
        >
          {(props) => (
            <Input
              {...props}
              value={scopeKey}
              onChange={(e) => onKey(e.target.value)}
              className="mono-data"
              autoComplete="off"
              spellCheck={false}
            />
          )}
        </Field>
      )}
    </>
  );
}

function ReasonField({ value, error, onChange }: { value: string; error?: string; onChange: (v: string) => void }) {
  return (
    <Field label="Reason" required error={error} hint={`Written to the audit log. Up to ${MAX_REASON} characters.`}>
      {(props) => <Textarea {...props} maxLength={MAX_REASON} value={value} onChange={(e) => onChange(e.target.value)} />}
    </Field>
  );
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
  const [reason, setReason] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [result, setResult] = useState<RetentionPolicyResponse | null>(null);

  const key = cleanKey(scope, scopeKey);
  const keyError = validateScopeKey(scope, key);
  const reasonError = validateReason(reason);
  const durationErrors = durationFields.map((f) => validateDuration(cleanDuration(durations[f.key])));
  const setAny = durationFields.some((f) => cleanDuration(durations[f.key]) !== "");
  const noDuration = setAny ? null : "Set at least one duration.";
  const problem = keyError || reasonError || durationErrors.find(Boolean) || noDuration;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (problem) return;
    setConfirming(true);
  }

  async function save() {
    const body: RetentionPolicyRequest = { scope, reason: reason.trim() };
    if (scope !== "TENANT") body.scope_key = key;
    for (const f of durationFields) {
      const value = cleanDuration(durations[f.key]);
      if (value) body[f.key] = value;
    }
    setResult(await setRetentionPolicy(body));
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
        <div className="grid gap-4 sm:grid-cols-2">
          {durationFields.map((f, i) => (
            <Field
              key={f.key}
              label={f.label}
              hint={f.hint}
              error={durationErrors[i] ?? (i === 0 ? noDuration : null) ?? undefined}
            >
              {(props) => (
                <Input
                  {...props}
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
        </div>
        <p className="text-sm text-muted">
          Durations are Go-style: 720h, 1h30m. Only ns, us, ms, s, m and h exist, so write 7 days as 168h. Leave a field
          empty to inherit.
        </p>
        <ReasonField value={reason} error={reasonError ?? undefined} onChange={setReason} />
        <SubmitButton valid={!problem} hint={problem ?? undefined}>
          Review and set policy
        </SubmitButton>
      </form>

      {result && (
        <div role="status" className="neo-in-sm mt-5 p-3">
          <p className="font-semibold">Policy saved</p>
          <DefList
            items={[
              { label: "Scope", value: `${result.scope}${result.scope_key ? ` / ${result.scope_key}` : ""}` },
              { label: "Version", value: result.version },
              { label: "Effective from", value: <Time value={result.effective_from} /> },
              { label: "Policy id", value: <span className="mono-data">{result.id}</span> },
            ]}
          />
        </div>
      )}

      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title="Set this retention policy?"
        description={
          <>
            This takes effect immediately for {scopeLabels[scope].toLowerCase()}
            {scope !== "TENANT" && key ? ` (${key})` : ""}. The API cannot show the current
            policy, so it cannot tell whether your values are shorter. A shorter minimum retention or grace period
            lets data be deleted sooner than it is today. Check them before you continue.
          </>
        }
        confirmLabel="Set policy"
        onConfirm={save}
      />
    </Panel>
  );
}

function HoldPanel() {
  const [scope, setScope] = useState<RetentionScope>("TENANT");
  const [scopeKey, setScopeKey] = useState("");
  const [reason, setReason] = useState("");
  const [placing, setPlacing] = useState(false);
  const [placeError, setPlaceError] = useState<string | null>(null);
  const [placed, setPlaced] = useState<LegalHold[]>([]);

  const [releaseId, setReleaseId] = useState("");
  const [releasing, setReleasing] = useState(false);
  const [released, setReleased] = useState<string | null>(null);

  const key = cleanKey(scope, scopeKey);
  const keyError = validateScopeKey(scope, key);
  const reasonError = validateReason(reason);
  const cleanReleaseId = releaseId.replace(/\s+/g, "");

  async function place(e: FormEvent) {
    e.preventDefault();
    if (keyError || reasonError) return;
    setPlaceError(null);

    setPlacing(true);
    try {
      const hold = await placeLegalHold({
        scope,
        reason: reason.trim(),
        ...(scope !== "TENANT" && { scope_key: key }),
      });
      setPlaced((list) => [hold, ...list]);
      setReason("");
      setScopeKey("");
    } catch (error) {
      setPlaceError(describeError(error));
    } finally {
      setPlacing(false);
    }
  }

  return (
    <Panel
      title="Legal holds"
      description="A hold blocks deletion and purge for everything in its scope until it is released."
    >
      <form onSubmit={place} noValidate className="flex flex-col gap-4">
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
        <ReasonField value={reason} error={reasonError ?? undefined} onChange={setReason} />
        {placeError && (
          <p role="alert" className="text-base font-medium">
            {placeError}
          </p>
        )}
        <SubmitButton valid={!keyError && !reasonError} loading={placing} hint={keyError ?? reasonError ?? undefined}>
          Place legal hold
        </SubmitButton>
      </form>

      {placed.length > 0 && (
        <div className="mt-5">
          <h3 className="text-md font-semibold">Placed in this session</h3>
          <p className="text-sm text-muted">
            The API cannot list holds, so this is the only time you see these ids. Copy them now: leaving this page
            forgets them.
          </p>
          <ul className="mt-3 flex flex-col gap-3">
            {placed.map((hold) => (
              <li key={hold.id} className="neo-in-sm flex flex-wrap items-center justify-between gap-3 p-3">
                <div className="min-w-0">
                  <p className="mono-data [overflow-wrap:anywhere]">{hold.id}</p>
                  <p className="text-sm text-muted">
                    {hold.scope}
                    {hold.scope_key ? ` / ${hold.scope_key}` : ""}, placed <Time value={hold.placed_at} />
                  </p>
                </div>
                <CopyButton value={hold.id} label="Copy id" />
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="mt-6 border-t-2 border-faint pt-5">
        <h3 className="text-md font-semibold">Release a legal hold</h3>
        <p className="mb-3 text-sm text-muted">Paste the hold id you saved when you placed it.</p>
        <div className="flex flex-wrap items-end gap-3">
          <Field
            label="Hold id"
            required
            className="min-w-56 flex-1"
            error={cleanReleaseId === "" ? "Enter the id of the hold to release." : undefined}
          >
            {(props) => (
              <Input
                {...props}
                value={releaseId}
                onChange={(e) => setReleaseId(e.target.value)}
                className="mono-data"
                autoComplete="off"
                spellCheck={false}
              />
            )}
          </Field>
          <Button onClick={() => setReleasing(true)} disabled={cleanReleaseId === ""}>
            Release hold
          </Button>
        </div>
        {released && (
          <p role="status" className="mt-3 text-base font-medium">
            Hold <span className="mono-data">{released}</span> was released.
          </p>
        )}
      </div>

      <ConfirmDialog
        open={releasing}
        onOpenChange={setReleasing}
        title="Release this legal hold?"
        description={
          <>
            Hold <span className="mono-data">{cleanReleaseId}</span> stops protecting its scope. Anything already due
            for deletion becomes eligible for purge.
          </>
        }
        confirmLabel="Release hold"
        onConfirm={async () => {
          const id = cleanReleaseId;
          await releaseLegalHold(id);
          setReleased(id);
          setReleaseId("");
        }}
      />
    </Panel>
  );
}
