"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { Field, Input, Select } from "@/components/ui/field";
import { PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { fetchFile, getFramework, listFrameworks, regulatorAccess, saveBlob, type Control } from "../api";
import { AuditTabs } from "./audit-tabs";

const STATUS: Record<Control["status"], { text: string; tone: "done" | "warning" | "failed" | "neutral" }> = {
  MET: { text: "Met", tone: "done" },
  PARTIAL: { text: "Partly met", tone: "warning" },
  NOT_MET: { text: "Not met", tone: "failed" },
  ORGANIZATION: { text: "Your procedure", tone: "neutral" },
};

export function ComplianceScreen() {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const [framework, setFramework] = useState("nist-800-171");
  const frameworks = useQuery({
    queryKey: orgKey(organizationId, "evidence", "frameworks"),
    queryFn: ({ signal }) => listFrameworks(signal),
    enabled: Boolean(organizationId),
  });

  return (
    <>
      <PageHeader
        title="Compliance"
        description="Each control, how the platform meets it, and where your organization stands now, with the facts behind it."
      />
      <AuditTabs />
      <div className="mb-6 max-w-md">
        <Field label="Framework">
          {(props) => (
            <Select {...props} value={framework} onChange={(e) => setFramework(e.target.value)}>
              {(frameworks.data?.frameworks ?? [{ id: framework, name: framework }]).map((f) => (
                <option key={f.id} value={f.id}>
                  {f.name}
                </option>
              ))}
            </Select>
          )}
        </Field>
      </div>
      <div className="grid gap-6 lg:grid-cols-3">
        <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
          <ControlsPanel framework={framework} />
        </div>
        <div className="flex min-w-0 flex-col gap-6">
          {framework === "sec-17a-4" && <UndertakingPanel />}
          {can("org.manage") && <RegulatorPanel />}
        </div>
      </div>
    </>
  );
}

function ControlsPanel({ framework }: { framework: string }) {
  const organizationId = useOrganizationId();
  const fw = useQuery({
    queryKey: orgKey(organizationId, "evidence", "framework", framework),
    queryFn: ({ signal }) => getFramework(framework, signal),
    enabled: Boolean(organizationId),
  });
  const download = useMutation({
    mutationFn: () => fetchFile(`/compliance/${encodeURIComponent(framework)}/evidence`),
    onSuccess: (blob) => saveBlob(blob, `${framework}-evidence.zip`),
    onError: notify.error,
  });
  return (
    <Panel
      title={fw.data?.name ?? "Controls"}
      description={fw.data?.note}
      actions={
        <Button onClick={() => download.mutate()} loading={download.isPending}>
          Download evidence
        </Button>
      }
    >
      {fw.isPending ? (
        <SkeletonRows rows={6} />
      ) : fw.isError ? (
        <ErrorState error={fw.error} onRetry={() => fw.refetch()} />
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Control</Th>
              <Th>How it is met</Th>
              <Th>Now</Th>
            </tr>
          </thead>
          <tbody>
            {fw.data.controls.map((c) => (
              <tr key={c.id}>
                <Td>
                  <span className="mono-data block text-sm">{c.id}</span>
                  {c.title}
                </Td>
                <Td className="text-sm">
                  {c.platform}
                  {(c.evidence ?? []).map((e) => (
                    <span key={e} className="block text-muted">
                      {e}
                    </span>
                  ))}
                </Td>
                <Td>
                  <StateBadge tone={STATUS[c.status].tone}>{STATUS[c.status].text}</StateBadge>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </Panel>
  );
}

function UndertakingPanel() {
  const [form, setForm] = useState({ role: "DEO", name: "", title: "", firm: "" });
  const draft = useMutation({
    mutationFn: () => fetchFile("/compliance/sec-17a-4/undertaking", form),
    onSuccess: (blob) => saveBlob(blob, "17a-4-undertaking.txt"),
    onError: notify.error,
  });
  const valid = form.name.trim() !== "" && form.title.trim() !== "" && form.firm.trim() !== "";

  function submit(e: FormEvent) {
    e.preventDefault();
    if (valid) draft.mutate();
  }

  return (
    <Panel title="Undertaking letter" description="A draft of the 17a-4(f)(3)(vii) undertaking, for your counsel to review before it is filed.">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label="Signing as">
          {(props) => (
            <Select {...props} value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
              <option value="DEO">Designated executive officer</option>
              <option value="D3P">Designated third party</option>
            </Select>
          )}
        </Field>
        <Field label="Name" required>
          {(props) => <Input {...props} autoComplete="name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />}
        </Field>
        <Field label="Title" required>
          {(props) => <Input {...props} autoComplete="organization-title" value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} />}
        </Field>
        <Field label="Firm" required>
          {(props) => <Input {...props} autoComplete="organization" value={form.firm} onChange={(e) => setForm({ ...form, firm: e.target.value })} />}
        </Field>
        <div>
          <SubmitButton valid={valid} loading={draft.isPending}>
            Download draft
          </SubmitButton>
        </div>
      </form>
    </Panel>
  );
}

function RegulatorPanel() {
  const [name, setName] = useState("");
  const [days, setDays] = useState("7");
  const issue = useMutation({
    mutationFn: () => regulatorAccess({ name: name.trim(), days: Number(days) }),
    onError: notify.error,
  });
  const n = Number(days);
  const daysError = !Number.isInteger(n) || n < 1 || n > 30 ? "Between 1 and 30 days." : undefined;
  const valid = name.trim() !== "" && !daysError;

  function submit(e: FormEvent) {
    e.preventDefault();
    if (valid) issue.mutate();
  }

  const t = issue.data;
  return (
    <Panel
      title="Regulator access"
      description="A read-only token for an examiner: datasets, files and the audit chain, never changes. Its creation and every read are audited."
    >
      {t ? (
        <div className="flex flex-col gap-3">
          <p className="text-base">
            Token for {t.name}, until <Time value={t.expires_at} />. It is shown once; give it to the examiner now.
          </p>
          <p className="mono-data neo-in-sm break-all p-3 text-sm">{t.token}</p>
          <div className="flex gap-2">
            <CopyButton value={t.token} label="Copy token" />
            <Button variant="ghost" onClick={() => issue.reset()}>
              Done
            </Button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="flex flex-col gap-4">
          <Field label="Examiner" hint="For example FINRA examiner, J. Smith" required>
            {(props) => <Input {...props} maxLength={80} spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} />}
          </Field>
          <Field label="Days" error={daysError}>
            {(props) => <Input {...props} type="number" min={1} max={30} value={days} onChange={(e) => setDays(e.target.value)} />}
          </Field>
          <div>
            <SubmitButton valid={valid} loading={issue.isPending}>
              Issue token
            </SubmitButton>
          </div>
        </form>
      )}
    </Panel>
  );
}
