"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/field";
import { DefList, Meter } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Table, Td, Th } from "@/components/ui/table";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import { createRule, deleteRule, getBudget, getReport, listMoves, listRules, putBudget, type Budget, type Tier } from "../api";

const usd = new Intl.NumberFormat(undefined, { style: "currency", currency: "USD" });
export const money = (n: number) => usd.format(n);

const PROVIDER_TEXT: Record<string, string> = {
  AWS_S3: "Amazon S3",
  GOOGLE_CLOUD_STORAGE: "Google Cloud Storage",
  AZURE_BLOB: "Azure Blob Storage",
  CLOUDFLARE_R2: "Cloudflare R2",
  S3_COMPATIBLE: "S3-compatible",
  PLATFORM_MANAGED: "Platform storage",
};

export const TIER_TEXT: Record<Tier, string> = {
  STANDARD: "Standard",
  INFREQUENT: "Infrequent",
  ARCHIVE: "Archive",
  DEEP_ARCHIVE: "Deep archive",
};

function useCostKeys() {
  const organizationId = useOrganizationId();
  const client = useQueryClient();
  return { organizationId, refresh: () => client.invalidateQueries({ queryKey: orgKey(organizationId, "costs") }) };
}

// The organization's month: what storage and activity cost, what packing,
// deduplication and tiering saved, the budget, and the tier rules.
export function CostsScreen() {
  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <MonthPanel />
        <RulesPanel />
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        <BudgetPanel />
      </div>
    </div>
  );
}

function MonthPanel() {
  const { organizationId } = useCostKeys();
  const report = useQuery({
    queryKey: orgKey(organizationId, "costs", "report"),
    queryFn: ({ signal }) => getReport(signal),
    enabled: Boolean(organizationId),
  });
  const r = report.data;
  return (
    <Panel title="This month" description="Estimated at the providers' list prices. Real bills depend on region, agreements and discounts.">
      {report.isPending ? (
        <SkeletonRows rows={4} />
      ) : report.isError || !r ? (
        <ErrorState error={report.error} onRetry={() => report.refetch()} />
      ) : (
        <div className="flex flex-col gap-5">
          <p className="text-3xl font-semibold tabular-nums">{money(r.total_usd)}</p>
          <DefList
            items={[
              { label: "Storage", value: money(r.storage_usd) },
              { label: "Requests", value: `${money(r.request_usd)} for ${formatNumber(r.reads)} reads, ${formatNumber(r.writes)} writes` },
              { label: "Egress", value: `${money(r.egress_usd)} for ${formatBytes(r.egress_bytes)}` },
              { label: "Restores", value: r.restored_bytes ? `${money(r.restore_usd)} for ${formatBytes(r.restored_bytes)}` : "None" },
            ]}
          />
          {r.stored.length > 0 && (
            <Table>
              <thead>
                <tr>
                  <Th>Provider</Th>
                  <Th>Tier</Th>
                  <Th className="text-right">Objects</Th>
                  <Th className="text-right">Size</Th>
                  <Th className="text-right">A month</Th>
                </tr>
              </thead>
              <tbody>
                {r.stored.map((l) => (
                  <tr key={`${l.connection_id}-${l.tier}`}>
                    <Td translate="no">{PROVIDER_TEXT[l.provider] ?? l.provider}</Td>
                    <Td>{TIER_TEXT[l.tier]}</Td>
                    <Td className="text-right tabular-nums">{formatNumber(l.objects)}</Td>
                    <Td className="text-right tabular-nums">{formatBytes(l.bytes)}</Td>
                    <Td className="text-right tabular-nums">{money(l.monthly_usd)}</Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
          <div>
            <h3 className="text-base font-semibold">Saved</h3>
            <ul className="mt-2 grid gap-1 text-base">
              <li>
                Packing: {formatNumber(r.savings.packed_files)} files in {formatNumber(r.savings.packs)} packs, so a full read makes{" "}
                {formatNumber(r.savings.reads_saved_per_pass)} fewer requests.
              </li>
              <li>
                Deduplication: {formatBytes(r.savings.shared_bytes)} not stored twice, about {money(r.savings.dedup_usd_month)} a month.
              </li>
              <li>Tiering: about {money(r.savings.tiering_usd_month)} a month against keeping everything in Standard.</li>
            </ul>
          </div>
          <p className="text-sm text-muted">{r.note}</p>
        </div>
      )}
    </Panel>
  );
}

function BudgetPanel() {
  const { organizationId, refresh } = useCostKeys();
  const { can } = useSession();
  const budget = useQuery({
    queryKey: orgKey(organizationId, "costs", "budget"),
    queryFn: ({ signal }) => getBudget(signal),
    enabled: Boolean(organizationId),
  });
  const report = useQuery({
    queryKey: orgKey(organizationId, "costs", "report"),
    queryFn: ({ signal }) => getReport(signal),
    enabled: Boolean(organizationId),
  });
  return (
    <Panel
      title="Monthly budget"
      description="Downloads, links and streams are checked against it. Past it, an alert is raised, and with Block reads stop until the month turns."
    >
      {budget.isPending ? (
        <SkeletonRows rows={2} />
      ) : budget.isError ? (
        <ErrorState error={budget.error} onRetry={() => budget.refetch()} />
      ) : (
        <div className="flex flex-col gap-4">
          {budget.data.monthly_usd > 0 && report.data && (
            <Meter label="Spent this month" used={report.data.total_usd} limit={budget.data.monthly_usd} format={money} />
          )}
          <BudgetForm key={budget.data.updated_at ?? "none"} budget={budget.data} editable={can("org.manage")} onSaved={refresh} />
        </div>
      )}
    </Panel>
  );
}

function BudgetForm({ budget, editable, onSaved }: { budget: Budget; editable: boolean; onSaved: () => void }) {
  const [dollars, setDollars] = useState(budget.monthly_usd ? String(budget.monthly_usd) : "");
  const [reads, setReads] = useState(budget.monthly_requests ? String(budget.monthly_requests) : "");
  const [action, setAction] = useState<Budget["action"]>(budget.action);
  const usdValue = dollars.trim() === "" ? 0 : Number(dollars);
  const readsValue = reads.trim() === "" ? 0 : Number(reads);
  const usdError = !Number.isFinite(usdValue) || usdValue < 0 ? "A dollar amount, or empty for no limit." : undefined;
  const readsError = !Number.isInteger(readsValue) || readsValue < 0 ? "A whole number of reads, or empty for no limit." : undefined;
  const save = useMutation({
    mutationFn: () => putBudget({ monthly_usd: usdValue, monthly_requests: readsValue, action }),
    onSuccess: () => {
      notify.success("Budget saved");
      onSaved();
    },
    onError: notify.error,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!usdError && !readsError) save.mutate();
  };
  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <Field label="Dollars a month" hint="Empty for no cost limit." error={usdError}>
        {(props) => (
          <Input {...props} name="budget_usd" inputMode="decimal" autoComplete="off" className="tabular-nums" disabled={!editable}
            value={dollars} onChange={(e) => setDollars(e.target.value)} placeholder="No limit…" />
        )}
      </Field>
      <Field label="Reads a month" hint="Downloads, links and streamed files." error={readsError}>
        {(props) => (
          <Input {...props} name="budget_reads" inputMode="numeric" autoComplete="off" className="tabular-nums" disabled={!editable}
            value={reads} onChange={(e) => setReads(e.target.value)} placeholder="No limit…" />
        )}
      </Field>
      <Field label="Past the budget">
        {(props) => (
          <Select {...props} name="budget_action" disabled={!editable} value={action} onChange={(e) => setAction(e.target.value as Budget["action"])}>
            <option value="ALERT">Alert only</option>
            <option value="BLOCK">Alert and stop reads</option>
          </Select>
        )}
      </Field>
      {editable && (
        <div>
          <SubmitButton valid={!usdError && !readsError} loading={save.isPending}>
            Save budget
          </SubmitButton>
        </div>
      )}
    </form>
  );
}

function RulesPanel() {
  const { organizationId, refresh } = useCostKeys();
  const { can } = useSession();
  const rules = useQuery({
    queryKey: orgKey(organizationId, "costs", "rules"),
    queryFn: ({ signal }) => listRules(signal),
    enabled: Boolean(organizationId) && can("storage.read"),
  });
  const moves = useQuery({
    queryKey: orgKey(organizationId, "costs", "moves"),
    queryFn: ({ signal }) => listMoves(signal),
    enabled: Boolean(organizationId) && can("storage.read"),
  });
  const [removing, setRemoving] = useState<{ id: string; name: string } | null>(null);
  if (!can("storage.read")) return null;
  const list = rules.data?.rules ?? [];

  return (
    <Panel
      title="Tier rules"
      description="Objects old enough and not read for a while move to a colder, cheaper tier. Held, locked, packed and shared objects, and snapshots a running training job uses, are left where they are."
    >
      <div className="flex flex-col gap-5">
        {rules.isPending ? (
          <SkeletonRows rows={2} />
        ) : rules.isError ? (
          <ErrorState error={rules.error} onRetry={() => rules.refetch()} />
        ) : list.length === 0 ? (
          <EmptyState title="No rules">Everything stays in the tier it was uploaded to.</EmptyState>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Rule</Th>
                <Th>Moves to</Th>
                <Th>When</Th>
                <Th className="sr-only">Actions</Th>
              </tr>
            </thead>
            <tbody>
              {list.map((r) => (
                <tr key={r.id}>
                  <Td>
                    <span className="block font-semibold">{r.name}</span>
                    <span className="block text-sm text-muted">{r.dataset_id ? "One dataset's objects" : "Every object"}</span>
                  </Td>
                  <Td>{TIER_TEXT[r.tier]}</Td>
                  <Td className="text-sm">
                    {r.min_age_days > 0 && `${r.min_age_days} days old`}
                    {r.min_age_days > 0 && r.unread_days > 0 && ", "}
                    {r.unread_days > 0 && `unread ${r.unread_days} days`}
                  </Td>
                  <Td className="text-right">
                    {can("storage.manage") && (
                      <Button onClick={() => setRemoving({ id: r.id, name: r.name })} aria-label={`Delete rule ${r.name}`}>
                        Delete
                      </Button>
                    )}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        {can("storage.manage") && <RuleForm onSaved={refresh} />}
        <ConfirmDialog
          open={removing !== null}
          onOpenChange={(open) => !open && setRemoving(null)}
          title={`Delete the rule ${removing?.name ?? ""}?`}
          description="Objects it already moved stay where they are; nothing more moves by it."
          confirmLabel="Delete rule"
          onConfirm={async () => {
            if (removing) await deleteRule(removing.id);
            await refresh();
          }}
        />
        {(moves.data?.moves.length ?? 0) > 0 && (
          <div>
            <h3 className="text-base font-semibold">Recent moves</h3>
            <ul className="mt-2 grid gap-1.5 text-sm">
              {moves.data?.moves.slice(0, 8).map((m) => (
                <li key={m.id} className="flex flex-wrap items-center gap-2">
                  <StateBadge tone={m.state === "MOVED" ? "done" : "failed"}>{m.state === "MOVED" ? "Moved" : "Failed"}</StateBadge>
                  <span className="mono-data">{m.asset_id.slice(0, 12)}</span>
                  <span>
                    {TIER_TEXT[m.from_tier]} to {TIER_TEXT[m.to_tier]}, {formatBytes(m.size_bytes)}
                  </span>
                  {m.reason && <span className="min-w-0 text-muted [overflow-wrap:anywhere]">{m.reason}</span>}
                  <Time value={m.created_at} />
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </Panel>
  );
}

function RuleForm({ onSaved }: { onSaved: () => void }) {
  const [name, setName] = useState("");
  const [tier, setTier] = useState<Tier>("ARCHIVE");
  const [age, setAge] = useState("90");
  const [unread, setUnread] = useState("30");
  const ageDays = Number(age || 0);
  const unreadDays = Number(unread || 0);
  const daysError =
    !Number.isInteger(ageDays) || !Number.isInteger(unreadDays) || ageDays < 0 || unreadDays < 0
      ? "Whole days."
      : ageDays === 0 && unreadDays === 0
        ? "Set an age or an unread time, or new uploads would move at once."
        : undefined;
  const valid = name.trim() !== "" && !daysError;
  const save = useMutation({
    mutationFn: () => createRule({ name: name.trim(), tier, min_age_days: ageDays, unread_days: unreadDays }),
    onSuccess: () => {
      notify.success("Rule added: the next pass applies it");
      setName("");
      onSaved();
    },
    onError: notify.error,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (valid) save.mutate();
  };
  return (
    <form onSubmit={submit} className="grid gap-4 sm:grid-cols-2">
      <Field label="Rule name" required>
        {(props) => <Input {...props} name="rule_name" autoComplete="off" value={name} onChange={(e) => setName(e.target.value)} placeholder="Cold after 90 days…" />}
      </Field>
      <Field label="Move to">
        {(props) => (
          <Select {...props} name="rule_tier" value={tier} onChange={(e) => setTier(e.target.value as Tier)}>
            <option value="INFREQUENT">Infrequent</option>
            <option value="ARCHIVE">Archive</option>
            <option value="DEEP_ARCHIVE">Deep archive</option>
          </Select>
        )}
      </Field>
      <Field label="Days since upload" error={daysError}>
        {(props) => <Input {...props} name="rule_age" inputMode="numeric" autoComplete="off" className="tabular-nums" value={age} onChange={(e) => setAge(e.target.value)} />}
      </Field>
      <Field label="Days without a read">
        {(props) => <Input {...props} name="rule_unread" inputMode="numeric" autoComplete="off" className="tabular-nums" value={unread} onChange={(e) => setUnread(e.target.value)} />}
      </Field>
      <div className="sm:col-span-2">
        <SubmitButton valid={valid} loading={save.isPending}>
          Add rule
        </SubmitButton>
      </div>
    </form>
  );
}
