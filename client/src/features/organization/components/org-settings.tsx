"use client";

import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input, Textarea } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { cleanText, cleanWholeNumber, splitList } from "@/lib/utils/forgive";
import { deactivateOrganization, updateOrganization } from "../api";
import { useOrganizationQuery, useOrgMutation } from "../hooks";
import type { Organization, OrganizationUpdate } from "../types";
import { YourRole } from "./your-role";

function parseDomains(text: string) {
  return splitList(text).map((d) => d.toLowerCase());
}

const MAX_TTL_HOURS = 720;

function ttlError(text: string) {
  if (text.trim() === "") return "";
  const hours = cleanWholeNumber(text);
  if (hours === "") return "Enter a whole number of hours, like 168.";
  if (Number(hours) > MAX_TTL_HOURS) return `Use ${MAX_TTL_HOURS} hours or fewer.`;
  return "";
}

export function OrgSettings() {
  const { can, isPlatformAdmin } = useSession();
  const org = useOrganizationQuery();

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        {org.isPending && <SkeletonRows rows={4} />}
        {org.isError && <ErrorState error={org.error} onRetry={() => org.refetch()} />}
        {org.data && (
          <>
            <Panel title="Organization">
              <DefList
                items={[
                  { label: "Name", value: org.data.name },
                  { label: "Slug", value: <span className="mono-data">{org.data.slug}</span> },
                  { label: "Status", value: <StateBadge>{org.data.status}</StateBadge> },
                  { label: "Created", value: <Time value={org.data.created_at} /> },
                ]}
              />
            </Panel>
            <OrgForm key={JSON.stringify(org.data)} org={org.data} canEdit={can("org.manage")} />
            {can("org.ownership") && <DeactivatePanel org={org.data} />}
          </>
        )}
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        <YourRole />
        {isPlatformAdmin && (
          <Panel title="Platform administration">
            <p className="text-base text-muted">
              You are a platform admin. Organizations, users and quotas are managed in the platform tab.
            </p>
          </Panel>
        )}
      </div>
    </div>
  );
}

function OrgForm({ org, canEdit }: { org: Organization; canEdit: boolean }) {
  const [name, setName] = useState(org.name);
  const [domains, setDomains] = useState((org.allowed_email_domains ?? []).join("\n"));
  const [ttl, setTtl] = useState(org.invitation_ttl_hours ? String(org.invitation_ttl_hours) : "");
  const [error, setError] = useState<string | null>(null);
  const save = useOrgMutation((body: OrganizationUpdate) => updateOrganization(body), ["organization"]);

  const cleanName = cleanText(name);
  const list = parseDomains(domains);
  const ttlProblem = ttlError(ttl);
  const nameProblem = cleanName === "" ? "Enter a name for the organization." : "";
  const changed =
    cleanName !== org.name ||
    list.join(",") !== (org.allowed_email_domains ?? []).join(",") ||
    Number(cleanWholeNumber(ttl) || 0) !== (org.invitation_ttl_hours ?? 0);
  const valid = !nameProblem && !ttlProblem && changed;

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!valid) return;
    setError(null);
    // The API replaces all settings, so every field is sent. Untouched ones carry their current value.
    const body: OrganizationUpdate = { name: cleanName };
    if (list.length > 0) body.allowed_email_domains = list;
    if (ttl.trim() !== "") body.invitation_ttl_hours = Number(cleanWholeNumber(ttl));
    try {
      await save.mutateAsync(body);
      notify.success("Organization settings saved");
    } catch (e) {
      setError(describeError(e));
    }
  }

  return (
    <Panel
      title="Settings"
      description="Saving replaces all three settings at once. Fields you leave alone keep their current values."
    >
      <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
        <Field label="Name" required error={nameProblem}>
          {(p) => <Input {...p} value={name} onChange={(e) => setName(e.target.value)} disabled={!canEdit} />}
        </Field>
        <Field
          label="Allowed email domains"
          hint="One per line, for example example.com. Leave empty to allow any domain in invitations."
        >
          {(p) => (
            <Textarea {...p} value={domains} onChange={(e) => setDomains(e.target.value)} disabled={!canEdit} />
          )}
        </Field>
        <Field
          label="Invitation lifetime (hours)"
          hint="0 to 720. Empty or 0 uses the server default of 168 hours."
          error={ttlProblem}
        >
          {(p) => (
            <Input
              {...p}
              inputMode="numeric"
              value={ttl}
              onChange={(e) => setTtl(e.target.value)}
              disabled={!canEdit}
              className="max-w-40"
            />
          )}
        </Field>
        {error && (
          <p role="alert" className="text-base font-medium">
            {error}
          </p>
        )}
        <div className="flex items-center gap-3">
          <SubmitButton
            valid={canEdit && valid}
            loading={save.isPending}
            hint={
              !canEdit
                ? "Changing settings needs the org.manage permission."
                : nameProblem || ttlProblem || "Change a setting to save."
            }
          >
            Save settings
          </SubmitButton>
        </div>
      </form>
    </Panel>
  );
}

function DeactivatePanel({ org }: { org: Organization }) {
  const [open, setOpen] = useState(false);
  const deactivate = useOrgMutation(() => deactivateOrganization(), ["organization"]);

  return (
    <Panel title="Deactivate organization" description="Owners only.">
      <p className="max-w-prose text-base text-muted">
        Every member request is refused with &quot;organization inactive&quot; until a platform admin reactivates it.
        Data is not deleted.
      </p>
      <div className="mt-4">
        <Button variant="accent" onClick={() => setOpen(true)} disabled={org.status === "DEACTIVATED"}>
          Deactivate organization
        </Button>
      </div>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title={`Deactivate ${org.name}?`}
        description="All members lose access to this organization immediately. Only a platform admin can turn it back on."
        confirmLabel="Deactivate organization"
        confirmText={org.slug}
        onConfirm={async () => {
          await deactivate.mutateAsync();
          notify.success("Organization deactivated");
        }}
      />
    </Panel>
  );
}
