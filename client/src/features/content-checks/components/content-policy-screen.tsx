"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Field, Input, Select, Textarea } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { notify } from "@/components/ui/toast";
import { useSession } from "@/lib/auth/session";
import { useContentPolicy, useSaveContentPolicy } from "../hooks";
import type { ContentPolicy, PolicyAction } from "../types";
import { FAMILY_TEXT } from "./findings-panel";

// The platform's default per family; a code may differ (shown in the hint).
const DEFAULTS: Record<string, { action: PolicyAction; except?: string }> = {
  active_content: { action: "flag", except: "a PDF launch action rejects" },
  model: { action: "reject", except: "imports of a model's own code and NumPy object arrays only flag" },
  secret: { action: "flag" },
  format: { action: "reject" },
  external: { action: "flag", except: "the engine's own verdict applies when it gives one" },
  malware: { action: "reject" },
};

const ACTION_TEXT: Record<PolicyAction, string> = { allow: "Allow (record nothing)", flag: "Flag and publish", reject: "Reject" };

const CODE_PATTERN = /^[a-z_]+\.[a-z0-9_.-]+$/;

function splitTypes(text: string) {
  return text
    .split(/[\s,]+/)
    .map((t) => t.trim())
    .filter(Boolean);
}

export function ContentPolicyScreen() {
  const policy = useContentPolicy();
  if (policy.isPending) return <SkeletonRows rows={6} />;
  if (policy.isError) return <ErrorState error={policy.error} onRetry={() => policy.refetch()} />;
  return <PolicyForm key={JSON.stringify(policy.data.policy)} initial={policy.data.policy} families={policy.data.finding_families} />;
}

function PolicyForm({ initial, families }: { initial: ContentPolicy; families: string[] }) {
  const { can } = useSession();
  const canManage = can("org.manage");
  const save = useSaveContentPolicy();
  const [actions, setActions] = useState<Record<string, PolicyAction>>(initial.finding_actions ?? {});
  const [allowed, setAllowed] = useState(initial.allowed_types.join("\n"));
  const [denied, setDenied] = useState(initial.denied_types.join("\n"));
  const [maxMiB, setMaxMiB] = useState(initial.max_size_bytes ? String(Math.round(initial.max_size_bytes / 2 ** 20)) : "");
  const [unknown, setUnknown] = useState(initial.unknown_type_action);
  const [newCode, setNewCode] = useState("");
  const [newAction, setNewAction] = useState<PolicyAction>("reject");

  const codeOverrides = Object.entries(actions).filter(([key]) => key.includes("."));
  const newCodeProblem =
    newCode === ""
      ? undefined
      : !CODE_PATTERN.test(newCode) || !families.includes(newCode.split(".")[0])
        ? "A code is a family and a name, such as secret.aws_access_key."
        : newCode.startsWith("malware.")
          ? "Malware findings always reject."
          : undefined;
  const maxProblem = maxMiB !== "" && !(Number(maxMiB) > 0) ? "A whole number of MiB, or empty for the platform's limit." : undefined;

  function setFamily(family: string, value: string) {
    setActions((current) => {
      const next = { ...current };
      if (value === "") delete next[family];
      else next[family] = value as PolicyAction;
      return next;
    });
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate(
      {
        finding_actions: actions,
        allowed_types: splitTypes(allowed),
        denied_types: splitTypes(denied),
        max_size_bytes: maxMiB === "" ? 0 : Math.round(Number(maxMiB) * 2 ** 20),
        unknown_type_action: unknown,
      },
      { onSuccess: () => notify.success("Content policy saved. It applies to the next upload."), onError: notify.error },
    );
  }

  return (
    <form onSubmit={submit} className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <Panel
          title="What a finding does"
          description="Every upload is checked inside the quarantine gate before it is published. A flagged file is published with the finding on its record; a rejected one is not."
        >
          <div className="flex flex-col gap-5">
            {families.map((family) => {
              const text = FAMILY_TEXT[family];
              const def = DEFAULTS[family];
              const fixed = family === "malware";
              return (
                <Field
                  key={family}
                  label={text?.title ?? family}
                  hint={
                    <>
                      {text?.detail} Default: {def ? ACTION_TEXT[def.action].toLowerCase() : "flag"}
                      {def?.except ? `, except ${def.except}` : ""}.
                    </>
                  }
                >
                  {(props) => (
                    <Select
                      {...props}
                      className="max-w-xs"
                      value={fixed ? "reject" : (actions[family] ?? "")}
                      disabled={!canManage || fixed}
                      onChange={(e) => setFamily(family, e.target.value)}
                    >
                      {fixed ? (
                        <option value="reject">Always reject</option>
                      ) : (
                        <>
                          <option value="">Platform default</option>
                          <option value="allow">{ACTION_TEXT.allow}</option>
                          <option value="flag">{ACTION_TEXT.flag}</option>
                          <option value="reject">{ACTION_TEXT.reject}</option>
                        </>
                      )}
                    </Select>
                  )}
                </Field>
              );
            })}
          </div>
        </Panel>

        <Panel title="Exceptions by code" description="One finding treated differently from its family, such as model.pickle_imports set to allow.">
          {codeOverrides.length === 0 ? (
            <p className="text-base text-muted">None. Every finding follows its family.</p>
          ) : (
            <ul className="flex flex-col gap-2">
              {codeOverrides.map(([code, action]) => (
                <li key={code} className="neo-in-sm flex flex-wrap items-center justify-between gap-3 px-3 py-2">
                  <span>
                    <span translate="no" className="mono-data">{code}</span> <span className="text-muted">{ACTION_TEXT[action].toLowerCase()}</span>
                  </span>
                  {canManage && (
                    <Button type="button" onClick={() => setFamily(code, "")}>
                      Remove
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          )}
          {canManage && (
            <div className="mt-4 flex flex-wrap items-end gap-3">
              <Field label="Finding code" error={newCodeProblem} className="min-w-64 flex-1">
                {(props) => (
                  <Input
                    {...props}
                    name="finding_code"
                    value={newCode}
                    spellCheck={false}
                    autoComplete="off"
                    placeholder="model.pickle_imports…"
                    onChange={(e) => setNewCode(e.target.value.trim().toLowerCase())}
                  />
                )}
              </Field>
              <Field label="Action" className="w-56">
                {(props) => (
                  <Select {...props} name="finding_action" value={newAction} onChange={(e) => setNewAction(e.target.value as PolicyAction)}>
                    <option value="allow">{ACTION_TEXT.allow}</option>
                    <option value="flag">{ACTION_TEXT.flag}</option>
                    <option value="reject">{ACTION_TEXT.reject}</option>
                  </Select>
                )}
              </Field>
              <Button
                type="button"
                disabled={newCode === "" || newCodeProblem !== undefined}
                onClick={() => {
                  setFamily(newCode, newAction);
                  setNewCode("");
                }}
              >
                Add exception
              </Button>
            </div>
          )}
        </Panel>

        <Panel title="Types and size" description="Narrows the platform's own rules; it can never loosen them.">
          <div className="grid gap-5 sm:grid-cols-2">
            <Field label="Allowed types" hint="One per line, such as image/png or image/*. Empty allows what the platform allows.">
              {(props) => (
                <Textarea {...props} name="allowed_types" rows={4} value={allowed} disabled={!canManage} spellCheck={false} onChange={(e) => setAllowed(e.target.value)} />
              )}
            </Field>
            <Field label="Refused types" hint="Added to the platform's refused types.">
              {(props) => (
                <Textarea {...props} name="denied_types" rows={4} value={denied} disabled={!canManage} spellCheck={false} onChange={(e) => setDenied(e.target.value)} />
              )}
            </Field>
            <Field label="Largest file, in MiB" error={maxProblem} hint="Empty uses the platform's limit; a larger number has no effect.">
              {(props) => (
                <Input {...props} name="max_size_mib" inputMode="numeric" value={maxMiB} disabled={!canManage} onChange={(e) => setMaxMiB(e.target.value.trim())} />
              )}
            </Field>
            <Field label="Files of no known type">
              {(props) => (
                <Select {...props} name="unknown_type_action" value={unknown} disabled={!canManage} onChange={(e) => setUnknown(e.target.value as ContentPolicy["unknown_type_action"])}>
                  <option value="">Platform default</option>
                  <option value="reject">Reject</option>
                </Select>
              )}
            </Field>
          </div>
        </Panel>
      </div>

      <div className="flex min-w-0 flex-col gap-6">
        <Panel title="Save">
          {canManage ? (
            <div className="flex flex-col items-start gap-3">
              <SubmitButton valid={maxProblem === undefined} loading={save.isPending} hint="Fix the largest file size first.">
                Save content policy
              </SubmitButton>
              <p className="text-sm text-muted">It applies to uploads processed from now on. Published files keep their findings.</p>
            </div>
          ) : (
            <p className="text-base text-muted">Changing the content policy needs the org.manage permission.</p>
          )}
        </Panel>
        <Panel title="Where findings show">
          <ul className="flex list-disc flex-col gap-2 pl-5 text-sm">
            <li>On each asset, under Operations, with the check that found it.</li>
            <li>In readiness reports, as the content-checks rule of the built-in profiles.</li>
            <li>To webhooks subscribed to object.flagged.</li>
            <li>In the SDK and CLI: osp findings, osp blast-radius.</li>
          </ul>
        </Panel>
      </div>
    </form>
  );
}
