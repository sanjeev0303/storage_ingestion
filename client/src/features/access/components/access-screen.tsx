"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { Meter } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useSession } from "@/lib/auth/session";
import { PERMISSIONS } from "@/lib/permissions";
import { orgKey } from "@/lib/query/keys";
import { formatBytes } from "@/lib/utils/format";
import {
  acknowledgeAlert,
  createToken,
  getAccessPolicy,
  getEgress,
  listAlerts,
  listLinks,
  listTokens,
  putAccessPolicy,
  revokeLinks,
  revokeToken,
  type AccessPolicy,
  type AlertKind,
  type ApiToken,
} from "../api";

const ALERT_TEXT: Record<AlertKind, string> = {
  READ_SPIKE: "Read spike",
  MASS_LINKS: "Mass share links",
  NEW_NETWORK: "New network",
  EGRESS_BUDGET: "Download budget",
  STORAGE_POSTURE: "Bucket posture",
  LIFECYCLE_CHANGED: "Lifecycle rules changed",
  OUT_OF_BAND_WRITE: "Change outside the platform",
};

const EXPIRY = [
  { value: "168h", label: "7 days" },
  { value: "720h", label: "30 days" },
  { value: "2160h", label: "90 days" },
  { value: "8760h", label: "1 year" },
];

const LINK_TTL = [
  { value: "15m0s", label: "15 minutes" },
  { value: "1h0m0s", label: "1 hour" },
  { value: "24h0m0s", label: "1 day" },
  { value: "168h0m0s", label: "7 days" },
];

const GB = 1024 ** 3;

const splitList = (text: string) => text.split(/[\s,]+/).filter(Boolean);

export function AccessScreen() {
  const { can } = useSession();
  if (!can("assets.read")) {
    return <PermissionDenied detail="Access settings need the assets.read permission." />;
  }
  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <TokensPanel />
        <LinksPanel />
        {can("audit.read") && <AlertsPanel />}
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        {can("org.read") && <EgressPanel />}
        {can("org.read") && <PolicyPanel />}
      </div>
    </div>
  );
}

function useAccessKeys() {
  const { organizationId } = useSession();
  const client = useQueryClient();
  return () => client.invalidateQueries({ queryKey: orgKey(organizationId, "access") });
}

function TokensPanel() {
  const { organizationId, can } = useSession();
  const [everyone, setEveryone] = useState(false);
  const [creating, setCreating] = useState(false);
  const [revoking, setRevoking] = useState<ApiToken | null>(null);
  const refresh = useAccessKeys();
  const tokens = useQuery({
    queryKey: orgKey(organizationId, "access", "tokens", everyone),
    queryFn: ({ signal }) => listTokens(everyone, signal),
    enabled: Boolean(organizationId),
  });

  return (
    <Panel
      title="API tokens"
      description="Credentials for scripts and vendors. A token never holds more than its member's role, and a dataset or network list confines it further."
      actions={
        <div className="flex flex-wrap items-end gap-3">
          {can("members.manage") && (
            <Checkbox checked={everyone} onChange={(e) => setEveryone(e.target.checked)} label="Every member's" />
          )}
          <Button variant="accent" onClick={() => setCreating(true)}>
            New token
          </Button>
        </div>
      }
    >
      {tokens.isPending ? (
        <SkeletonRows rows={3} />
      ) : tokens.isError ? (
        <ErrorState error={tokens.error} onRetry={() => tokens.refetch()} />
      ) : tokens.data.tokens.length === 0 ? (
        <EmptyState title="No API tokens yet">
          Make one for a CI job or a vendor. For a vendor, list only the datasets they may read: everything else stays invisible to them.
        </EmptyState>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Name</Th>
              <Th>Reaches</Th>
              <Th>Last used</Th>
              <Th>
                <span className="sr-only">Actions</span>
              </Th>
            </tr>
          </thead>
          <tbody>
            {tokens.data.tokens.map((t) => (
              <tr key={t.id}>
                <Td>
                  <span className="font-medium">{t.name}</span>
                  <span className="mono-data block text-sm text-muted">{t.hint}</span>
                </Td>
                <Td className="text-sm">
                  <span className="block">{t.datasets.length > 0 ? `Datasets ${t.datasets.join(", ")}` : "Every dataset"}</span>
                  {t.networks.length > 0 && <span className="mono-data block">{t.networks.join(", ")}</span>}
                  <span className="block text-muted">
                    {t.permissions.length} permissions{t.allow_links ? ", share links" : ""}
                  </span>
                </Td>
                <Td className="text-sm">
                  {t.last_used_at ? (
                    <>
                      <Time value={t.last_used_at} />
                      {t.last_used_ip && <span className="mono-data block text-muted">{t.last_used_ip}</span>}
                    </>
                  ) : (
                    "Never"
                  )}
                </Td>
                <Td className="text-right">
                  {t.revoked_at ? (
                    <StateBadge tone="neutral">revoked</StateBadge>
                  ) : new Date(t.expires_at) < new Date() ? (
                    <StateBadge tone="neutral">expired</StateBadge>
                  ) : (
                    <Button onClick={() => setRevoking(t)}>Revoke</Button>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {creating && <NewTokenDialog onClose={() => setCreating(false)} />}
      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(open) => !open && setRevoking(null)}
        title={`Revoke ${revoking?.name ?? "token"}?`}
        description="Anything using it is refused from the next request. This cannot be undone; make a new token instead."
        confirmLabel="Revoke token"
        onConfirm={async () => {
          if (!revoking) return;
          await revokeToken(revoking.id);
          await refresh();
          notify.success("Token revoked");
        }}
      />
    </Panel>
  );
}

function NewTokenDialog({ onClose }: { onClose: () => void }) {
  const { permissions } = useSession();
  const mine = PERMISSIONS.filter((p) => !permissions || permissions.has(p));
  const [name, setName] = useState("");
  const [chosen, setChosen] = useState<Set<string>>(() => new Set(mine.includes("assets.read") ? ["assets.read"] : []));
  const [datasets, setDatasets] = useState("");
  const [networks, setNetworks] = useState("");
  const [expires, setExpires] = useState("2160h");
  const [allowLinks, setAllowLinks] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const refresh = useAccessKeys();
  const create = useMutation({ mutationFn: createToken });

  const datasetList = splitList(datasets.toLowerCase());
  const valid = name.trim() !== "" && chosen.size > 0;

  function toggle(p: string, on: boolean) {
    const next = new Set(chosen);
    if (on) next.add(p);
    else next.delete(p);
    setChosen(next);
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    create.mutate(
      {
        name: name.trim(),
        permissions: [...chosen],
        datasets: datasetList,
        networks: splitList(networks),
        expires_in: expires,
        allow_links: allowLinks && datasetList.length === 0,
      },
      {
        onSuccess: async (t) => {
          setSecret(t.token);
          await refresh();
        },
        onError: notify.error,
      },
    );
  }

  if (secret) {
    return (
      <Modal
        open
        onOpenChange={(open) => !open && onClose()}
        title="Copy the token now"
        description="It is shown this once. Store it where your script reads secrets; the platform keeps only its hash."
        footer={
          <Button variant="accent" onClick={onClose}>
            Done
          </Button>
        }
      >
        <div className="neo-in-sm flex flex-col gap-3 p-3">
          <span className="mono-data text-sm [overflow-wrap:anywhere]">{secret}</span>
          <CopyButton value={secret} label="Copy token" />
        </div>
        <p className="mt-3 text-sm text-muted">
          Use it as a bearer token, or with the CLI as <span className="mono-data">OSP_API_TOKEN</span>.
        </p>
      </Modal>
    );
  }

  return (
    <Modal open onOpenChange={(open) => !open && onClose()} title="New API token" description="Give it only what the script or vendor needs.">
      <form id="new-token" onSubmit={submit} className="flex flex-col gap-4">
        <Field label="Name" required hint="Who or what uses it, such as ci-nightly or vendor-acme.">
          {(props) => <Input {...props} name="token_name" value={name} autoComplete="off" onChange={(e) => setName(e.target.value)} />}
        </Field>
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-1 text-base font-medium">Permissions</legend>
          <div className="grid max-h-48 gap-1.5 overflow-y-auto sm:grid-cols-2" data-lenis-prevent>
            {mine
              .filter((p) => p !== "org.ownership")
              .map((p) => (
                <Checkbox key={p} checked={chosen.has(p)} onChange={(e) => toggle(p, e.target.checked)} label={<span className="mono-data text-sm">{p}</span>} />
              ))}
          </div>
        </fieldset>
        <Field label="Only these datasets" hint="Separated by spaces or commas. Empty reaches every dataset.">
          {(props) => (
            <Input {...props} name="token_datasets" value={datasets} className="mono-data" autoComplete="off" spellCheck={false} onChange={(e) => setDatasets(e.target.value)} />
          )}
        </Field>
        <Field label="Only from these networks" hint="CIDRs such as 203.0.113.0/24. Empty allows any address.">
          {(props) => (
            <Input {...props} name="token_networks" value={networks} className="mono-data" autoComplete="off" spellCheck={false} onChange={(e) => setNetworks(e.target.value)} />
          )}
        </Field>
        <Field label="Expires after">
          {(props) => (
            <Select {...props} name="token_expires" value={expires} onChange={(e) => setExpires(e.target.value)}>
              {EXPIRY.map((x) => (
                <option key={x.value} value={x.value}>
                  {x.label}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Checkbox
          checked={allowLinks && datasetList.length === 0}
          disabled={datasetList.length > 0}
          onChange={(e) => setAllowLinks(e.target.checked)}
          label={datasetList.length > 0 ? "Share links are never allowed for a token confined to datasets" : "May create share links"}
        />
        <SubmitButton valid={valid} loading={create.isPending} hint="Name it and choose at least one permission.">
          Create token
        </SubmitButton>
      </form>
    </Modal>
  );
}

function LinksPanel() {
  const { organizationId, can } = useSession();
  const [everyone, setEveryone] = useState(false);
  const [revoking, setRevoking] = useState<string[] | null>(null);
  const refresh = useAccessKeys();
  const links = useQuery({
    queryKey: orgKey(organizationId, "access", "links", everyone),
    queryFn: ({ signal }) => listLinks(everyone, true, signal),
    enabled: Boolean(organizationId),
  });

  const count = links.data?.links.length ?? 0;
  const many = (revoking?.length ?? 0) > 1;

  return (
    <Panel
      title="Share links"
      description="Live links that work without signing in. Each one is counted every time it is used."
      actions={
        <div className="flex flex-wrap items-end gap-3">
          {can("assets.delete") && <Checkbox checked={everyone} onChange={(e) => setEveryone(e.target.checked)} label="Everyone's" />}
          {count > 1 && <Button onClick={() => setRevoking(links.data?.links.map((l) => l.id) ?? [])}>Revoke all shown</Button>}
        </div>
      }
    >
      {links.isPending ? (
        <SkeletonRows rows={3} />
      ) : links.isError ? (
        <ErrorState error={links.error} onRetry={() => links.refetch()} />
      ) : count === 0 ? (
        <EmptyState title="No live share links">Share links made from an asset appear here until they expire or are revoked.</EmptyState>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>File</Th>
              <Th>Expires</Th>
              <Th className="text-right">Uses</Th>
              <Th>
                <span className="sr-only">Actions</span>
              </Th>
            </tr>
          </thead>
          <tbody>
            {links.data.links.map((l) => (
              <tr key={l.id}>
                <Td>
                  <span className="[overflow-wrap:anywhere]">{l.file_name}</span>
                  {everyone && <span className="mono-data block text-sm text-muted">{l.principal_id}</span>}
                </Td>
                <Td className="text-sm">
                  <Time value={l.expires_at} />
                </Td>
                <Td className="text-right tabular-nums">{l.uses.toLocaleString()}</Td>
                <Td className="text-right">
                  <Button onClick={() => setRevoking([l.id])}>Revoke</Button>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(open) => !open && setRevoking(null)}
        title={many ? `Revoke ${revoking?.length.toLocaleString()} links?` : "Revoke this link?"}
        description={many ? "Everyone holding one of these links is refused from now on." : "Anyone holding it is refused from now on."}
        confirmLabel={many ? "Revoke links" : "Revoke link"}
        onConfirm={async () => {
          const done = await revokeLinks({ link_ids: revoking ?? [] });
          await refresh();
          notify.success(done.revoked === 1 ? "Link revoked" : `${done.revoked.toLocaleString()} links revoked`);
        }}
      />
    </Panel>
  );
}

function AlertsPanel() {
  const { organizationId } = useSession();
  const [state, setState] = useState<"OPEN" | "">("OPEN");
  const refresh = useAccessKeys();
  const alerts = useQuery({
    queryKey: orgKey(organizationId, "access", "alerts", state),
    queryFn: ({ signal }) => listAlerts(state, signal),
    enabled: Boolean(organizationId),
  });
  const ack = useMutation({ mutationFn: acknowledgeAlert });

  return (
    <Panel
      title="Security alerts"
      description="Each member's reads against their own last two weeks, bursts of share links, sign-ins from new networks and download budgets. Checked every five minutes."
      actions={
        <Field label="Show" className="w-44">
          {(props) => (
            <Select {...props} name="alert_state" value={state} onChange={(e) => setState(e.target.value as "OPEN" | "")}>
              <option value="OPEN">Open</option>
              <option value="">Every alert</option>
            </Select>
          )}
        </Field>
      }
    >
      {alerts.isPending ? (
        <SkeletonRows rows={3} />
      ) : alerts.isError ? (
        <ErrorState error={alerts.error} onRetry={() => alerts.refetch()} />
      ) : alerts.data.alerts.length === 0 ? (
        <EmptyState title={state === "OPEN" ? "No open alerts" : "No alerts yet"}>
          Alerts also go to webhooks subscribed to security.alert.
        </EmptyState>
      ) : (
        <ul className="flex flex-col gap-3">
          {alerts.data.alerts.map((a) => (
            <li key={a.id} className="neo-in-sm p-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-2">
                  <StateBadge tone={a.state === "OPEN" ? "warning" : "neutral"}>{a.state.toLowerCase()}</StateBadge>
                  <span className="font-medium">{ALERT_TEXT[a.kind] ?? a.kind}</span>
                </div>
                {a.state === "OPEN" && (
                  <Button
                    disabled={ack.isPending}
                    onClick={() => ack.mutate(a.id, { onSuccess: () => refresh(), onError: notify.error })}
                  >
                    Acknowledge
                  </Button>
                )}
              </div>
              <p className="mt-1 [overflow-wrap:anywhere]">{a.detail}</p>
              <p className="mt-1 text-sm text-muted">
                <span className="mono-data">{a.principal_id}</span>, <Time value={a.created_at} />
                {a.action_taken && `. Done: ${a.action_taken}`}
                {a.acknowledged_by && (
                  <>
                    . Acknowledged by <span className="mono-data">{a.acknowledged_by}</span>
                  </>
                )}
              </p>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

function EgressPanel() {
  const { organizationId } = useSession();
  const egress = useQuery({
    queryKey: orgKey(organizationId, "access", "egress"),
    queryFn: ({ signal }) => getEgress(signal),
    enabled: Boolean(organizationId),
  });
  return (
    <Panel title="Downloads this month" description="Bytes served by links and downloads, against the organization's budget.">
      {egress.isPending ? (
        <SkeletonRows rows={3} />
      ) : egress.isError ? (
        <ErrorState error={egress.error} onRetry={() => egress.refetch()} />
      ) : (
        <div className="flex flex-col gap-4">
          <Meter label="Organization" used={egress.data.month_bytes} limit={egress.data.month_budget} format={formatBytes} />
          <p className="text-sm text-muted">
            {egress.data.action === "BLOCK" ? "Over budget, downloads are refused until next month." : "Over budget, downloads continue and an alert is raised."}
          </p>
          {egress.data.members.length > 0 && (
            <ul className="flex flex-col gap-2 text-sm">
              {egress.data.members.slice(0, 8).map((m) => (
                <li key={m.principal_id} className="flex flex-wrap justify-between gap-x-3">
                  <span className="mono-data min-w-0 [overflow-wrap:anywhere]">{m.principal_id}</span>
                  <span className="text-muted tabular-nums">
                    {formatBytes(m.bytes)}, {m.reads.toLocaleString()} reads
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </Panel>
  );
}

function PolicyPanel() {
  const { organizationId } = useSession();
  const policy = useQuery({
    queryKey: orgKey(organizationId, "access", "policy"),
    queryFn: ({ signal }) => getAccessPolicy(signal),
    enabled: Boolean(organizationId),
  });
  return (
    <Panel title="Access policy" description="Share links, download budgets and what happens on an anomaly.">
      {policy.isPending ? (
        <SkeletonRows rows={5} />
      ) : policy.isError ? (
        <ErrorState error={policy.error} onRetry={() => policy.refetch()} />
      ) : (
        <PolicyForm key={policy.data.updated_at ?? "default"} initial={policy.data} />
      )}
    </Panel>
  );
}

const toGB = (bytes: number) => (bytes > 0 ? String(Math.round((bytes / GB) * 100) / 100) : "");
const fromGB = (text: string) => Math.round((Number(text) || 0) * GB);

function PolicyForm({ initial }: { initial: AccessPolicy }) {
  const { can } = useSession();
  const editable = can("org.manage");
  const [linksEnabled, setLinksEnabled] = useState(initial.links_enabled);
  const [ttl, setTTL] = useState(initial.max_link_ttl === "0s" ? "" : initial.max_link_ttl);
  const [monthly, setMonthly] = useState(toGB(initial.egress_monthly_bytes));
  const [daily, setDaily] = useState(toGB(initial.egress_daily_member_bytes));
  const [egressAction, setEgressAction] = useState(initial.egress_action);
  const [threshold, setThreshold] = useState(String(initial.mass_link_threshold));
  const [anomaly, setAnomaly] = useState(initial.anomaly_action);
  const refresh = useAccessKeys();
  const save = useMutation({ mutationFn: putAccessPolicy });

  const thresholdNumber = Number(threshold);
  const valid = Number.isInteger(thresholdNumber) && thresholdNumber >= 10 && thresholdNumber <= 100000 && !(Number(monthly) < 0) && !(Number(daily) < 0);

  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate(
      {
        links_enabled: linksEnabled,
        max_link_ttl: ttl,
        egress_monthly_bytes: fromGB(monthly),
        egress_daily_member_bytes: fromGB(daily),
        egress_action: egressAction,
        mass_link_threshold: thresholdNumber,
        anomaly_action: anomaly,
      },
      {
        onSuccess: async () => {
          await refresh();
          notify.success("Access policy saved");
        },
        onError: notify.error,
      },
    );
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <fieldset disabled={!editable} className="flex flex-col gap-4">
        <Checkbox checked={linksEnabled} onChange={(e) => setLinksEnabled(e.target.checked)} label="Members may create share links" />
        <Field label="Longest link" hint="CONFIDENTIAL assets are capped at 1 hour and RESTRICTED at 15 minutes either way.">
          {(props) => (
            <Select {...props} name="max_link_ttl" value={ttl} onChange={(e) => setTTL(e.target.value)}>
              <option value="">Platform limit</option>
              {ttl !== "" && !LINK_TTL.some((x) => x.value === ttl) && <option value={ttl}>{ttl}</option>}
              {LINK_TTL.map((x) => (
                <option key={x.value} value={x.value}>
                  {x.label}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-1 xl:grid-cols-2">
          <Field label="Monthly budget, GB" hint="Empty for none.">
            {(props) => <Input {...props} name="egress_monthly" inputMode="decimal" value={monthly} className="tabular-nums" onChange={(e) => setMonthly(e.target.value)} />}
          </Field>
          <Field label="Per member per day, GB" hint="Empty for none.">
            {(props) => <Input {...props} name="egress_daily" inputMode="decimal" value={daily} className="tabular-nums" onChange={(e) => setDaily(e.target.value)} />}
          </Field>
        </div>
        <Field label="Over budget">
          {(props) => (
            <Select {...props} name="egress_action" value={egressAction} onChange={(e) => setEgressAction(e.target.value as AccessPolicy["egress_action"])}>
              <option value="WARN">Alert and keep serving</option>
              <option value="BLOCK">Refuse downloads</option>
            </Select>
          )}
        </Field>
        <Field label="Share links in five minutes before an alert" hint="Between 10 and 100,000.">
          {(props) => <Input {...props} name="mass_link_threshold" inputMode="numeric" value={threshold} className="tabular-nums" onChange={(e) => setThreshold(e.target.value)} />}
        </Field>
        <Field label="On an anomaly">
          {(props) => (
            <Select {...props} name="anomaly_action" value={anomaly} onChange={(e) => setAnomaly(e.target.value as AccessPolicy["anomaly_action"])}>
              <option value="ALERT">Alert only</option>
              <option value="REVOKE_LINKS">Alert and revoke the member's share links</option>
            </Select>
          )}
        </Field>
      </fieldset>
      {editable ? (
        <SubmitButton valid={valid} loading={save.isPending} hint="The alert threshold is between 10 and 100,000.">
          Save access policy
        </SubmitButton>
      ) : (
        <p className="text-sm text-muted">Changing it needs the org.manage permission.</p>
      )}
      {initial.updated_by && (
        <p className="text-sm text-muted">
          Last changed by <span className="mono-data">{initial.updated_by}</span>, <Time value={initial.updated_at} />
        </p>
      )}
    </form>
  );
}
