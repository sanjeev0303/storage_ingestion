"use client";

import { Plus, Trash } from "@phosphor-icons/react";
import Link from "next/link";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import {
  Checkbox,
  Field,
  Input,
  Select,
  Textarea,
} from "@/components/ui/field";
import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import {
  ErrorState,
  PermissionDenied,
  SkeletonRows,
} from "@/components/ui/states";
import { SubmitButton } from "@/components/ui/submit-button";
import { describeError } from "@/lib/api/errors";
import { cleanText } from "@/lib/utils/forgive";
import { useSession } from "@/lib/auth/session";
import { useCreateProfile, useProfiles } from "../hooks";
import {
  buildProfileRequest,
  type ProfileDraft,
  type RuleDraft,
} from "../schema";
import type { ValidationProfile } from "../types";

const emptyRule = (): RuleDraft => ({
  id: "",
  version: "1",
  type: "",
  severity: "ERROR",
  enabled: true,
  scope: "",
  config: "",
});

export function ProfileForm() {
  const { can } = useSession();
  const profiles = useProfiles();
  const create = useCreateProfile();

  const [draft, setDraft] = useState<ProfileDraft>({
    name: "",
    description: "",
    rules: [emptyRule()],
    maxErrors: "0",
    maxWarnings: "",
  });
  const [created, setCreated] = useState<ValidationProfile | null>(null);

  if (!can("datasets.manage")) {
    return (
      <div className="flex flex-col gap-6">
        <PageHeader
          crumbs={[
            { label: "Validation", href: "/app/validation" },
            { label: "New profile" },
          ]}
          title="New profile"
        />
        <PermissionDenied detail="Creating validation profiles needs the datasets.manage permission, which owners and administrators have." />
      </div>
    );
  }
  if (profiles.isPending) return <SkeletonRows rows={5} />;
  if (profiles.isError)
    return (
      <ErrorState error={profiles.error} onRetry={() => profiles.refetch()} />
    );

  const ruleTypes = profiles.data.rule_types;
  const existing = profiles.data.profiles.find(
    (p) => p.name === cleanText(draft.name) && !p.built_in,
  );
  // When the server knows only one rule type, every rule starts on it.
  const onlyType = ruleTypes.length === 1 ? ruleTypes[0] : "";
  const rules = draft.rules.map((r) => (r.type ? r : { ...r, type: onlyType }));
  const built = buildProfileRequest({ ...draft, rules }, ruleTypes);
  const errors = built.errors;
  const valid = Boolean(built.request);
  const hint = errors.name
    ? "Enter a name for the profile to continue."
    : errors.rules
      ? errors.rules
      : "Fix the highlighted fields to continue.";

  function updateRule(index: number, patch: Partial<RuleDraft>) {
    setDraft((d) => ({
      ...d,
      rules: d.rules.map((r, i) => (i === index ? { ...r, ...patch } : r)),
    }));
  }

  function submit(event: React.FormEvent) {
    event.preventDefault();
    setCreated(null);
    if (!valid || !built.request) return;
    create.mutate(built.request, {
      onSuccess: (profile) => setCreated(profile),
    });
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        crumbs={[
          { label: "Validation", href: "/app/validation" },
          { label: "New profile" },
        ]}
        title="New profile"
        description="A profile lists the rules a snapshot is checked against and how many errors or warnings it may have."
      />
      <form onSubmit={submit} noValidate className="flex flex-col gap-6">
        <Panel title="Details">
          <div className="grid max-w-3xl gap-4">
            <Field
              label="Name"
              required
              error={errors.name}
              hint={
                existing
                  ? `A profile named "${existing.name}" exists at version ${existing.version}. Saving creates version ${existing.version + 1} under the same id. Older versions never change.`
                  : "A new name creates version 1. Using an existing name adds a new version. The server picks the id."
              }
            >
              {(props) => (
                <Input
                  {...props}
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                  maxLength={128}
                />
              )}
            </Field>
            <Field label="Description" error={errors.description}>
              {(props) => (
                <Textarea
                  {...props}
                  maxLength={2000}
                  value={draft.description}
                  onChange={(e) =>
                    setDraft({ ...draft, description: e.target.value })
                  }
                />
              )}
            </Field>
          </div>
        </Panel>

        <Panel
          title="Thresholds"
          description="The overall verdict. Info findings never count."
        >
          <div className="grid max-w-xl gap-4 sm:grid-cols-2">
            <Field
              label="Maximum errors"
              required
              error={errors.maxErrors}
              hint="More errors than this make the version Failed."
            >
              {(props) => (
                <Input
                  {...props}
                  inputMode="numeric"
                  value={draft.maxErrors}
                  onChange={(e) =>
                    setDraft({ ...draft, maxErrors: e.target.value })
                  }
                />
              )}
            </Field>
            <Field
              label="Maximum warnings"
              error={errors.maxWarnings}
              hint="Optional. Empty means warnings never fail a version."
            >
              {(props) => (
                <Input
                  {...props}
                  inputMode="numeric"
                  value={draft.maxWarnings}
                  onChange={(e) =>
                    setDraft({ ...draft, maxWarnings: e.target.value })
                  }
                />
              )}
            </Field>
          </div>
        </Panel>

        <Panel
          title="Rules"
          description="Each rule has a stable id, a version and a type. Config is a JSON object; the server refuses unknown fields."
          actions={
            <Button
              onClick={() =>
                setDraft((d) => ({ ...d, rules: [...d.rules, emptyRule()] }))
              }
            >
              <Plus aria-hidden />
              Add rule
            </Button>
          }
        >
          {errors.rules && (
            <p role="alert" className="mb-3 font-medium">
              {errors.rules}
            </p>
          )}
          <ol className="flex flex-col gap-4">
            {rules.map((rule, index) => {
              const e = errors.rule[index] ?? {};
              return (
                <li key={index}>
                  <fieldset className="neo-in p-4">
                    <legend className="px-1 text-sm font-semibold">
                      Rule {index + 1}
                    </legend>
                    <div className="grid gap-4 md:grid-cols-3">
                      <Field label="Type" error={e.type} required>
                        {(props) => (
                          <Select
                            {...props}
                            value={rule.type}
                            onChange={(ev) =>
                              updateRule(index, { type: ev.target.value })
                            }
                          >
                            <option value="">Choose a type</option>
                            {ruleTypes.map((t) => (
                              <option key={t} value={t}>
                                {t}
                              </option>
                            ))}
                          </Select>
                        )}
                      </Field>
                      <Field label="Rule id" error={e.id} required>
                        {(props) => (
                          <Input
                            {...props}
                            value={rule.id}
                            onChange={(ev) =>
                              updateRule(index, { id: ev.target.value })
                            }
                          />
                        )}
                      </Field>
                      <Field label="Rule version" error={e.version} required>
                        {(props) => (
                          <Input
                            {...props}
                            inputMode="numeric"
                            value={rule.version}
                            onChange={(ev) =>
                              updateRule(index, { version: ev.target.value })
                            }
                          />
                        )}
                      </Field>
                      <Field label="Severity">
                        {(props) => (
                          <Select
                            {...props}
                            value={rule.severity}
                            onChange={(ev) =>
                              updateRule(index, {
                                severity: ev.target
                                  .value as RuleDraft["severity"],
                              })
                            }
                          >
                            <option value="ERROR">Error</option>
                            <option value="WARNING">Warning</option>
                            <option value="INFO">Info (never counts)</option>
                          </Select>
                        )}
                      </Field>
                      <Field
                        label="Scope"
                        hint="Leave on default to let the type decide."
                      >
                        {(props) => (
                          <Select
                            {...props}
                            value={rule.scope}
                            onChange={(ev) =>
                              updateRule(index, {
                                scope: ev.target.value as RuleDraft["scope"],
                              })
                            }
                          >
                            <option value="">Default for the type</option>
                            <option value="DATASET">Dataset</option>
                            <option value="OBJECT_METADATA">
                              Object metadata
                            </option>
                            <option value="OBJECT_CONTENT">
                              Object content
                            </option>
                            <option value="SPLIT">Split</option>
                          </Select>
                        )}
                      </Field>
                      <div className="flex items-end pb-2">
                        <Checkbox
                          label="Enabled"
                          checked={rule.enabled}
                          onChange={(ev) =>
                            updateRule(index, { enabled: ev.target.checked })
                          }
                        />
                      </div>
                    </div>
                    <Field
                      label="Config (JSON object)"
                      error={e.config}
                      hint='Optional. Example: {"keys": ["license", "source"]}'
                      className="mt-4"
                    >
                      {(props) => (
                        <Textarea
                          {...props}
                          className="mono-data min-h-20"
                          spellCheck={false}
                          value={rule.config}
                          onChange={(ev) =>
                            updateRule(index, { config: ev.target.value })
                          }
                        />
                      )}
                    </Field>
                    {rules.length > 1 && (
                      <div className="mt-3">
                        <Button
                          variant="ghost"
                          onClick={() =>
                            setDraft((d) => ({
                              ...d,
                              rules: d.rules.filter((_, i) => i !== index),
                            }))
                          }
                        >
                          <Trash aria-hidden />
                          Remove rule {index + 1}
                        </Button>
                      </div>
                    )}
                  </fieldset>
                </li>
              );
            })}
          </ol>
        </Panel>

        <div className="flex flex-col items-start gap-3">
          <SubmitButton valid={valid} loading={create.isPending} hint={hint}>
            Create profile
          </SubmitButton>
          {create.isError && (
            <p role="alert" className="max-w-prose font-medium">
              {describeError(create.error)}
            </p>
          )}
          {created && (
            <p role="status" className="max-w-prose">
              Saved as version {created.version}.{" "}
              <Link
                href={`/app/validation/profiles/${encodeURIComponent(created.id)}?version=${created.version}`}
                className="font-semibold underline underline-offset-2"
              >
                Open the profile
              </Link>
            </p>
          )}
        </div>
      </form>
    </div>
  );
}
