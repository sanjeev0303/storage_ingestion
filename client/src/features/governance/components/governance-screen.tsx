"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select, Textarea } from "@/components/ui/field";
import { DefList } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, PermissionDenied, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { ExportPanel } from "@/features/protection/components/export-panel";
import { orgKey } from "@/lib/query/keys";
import { formatBytes } from "@/lib/utils/format";
import {
  bulkDelete,
  decideApproval,
  deletionCertificate,
  getGovernance,
  listApprovals,
  publicKey,
  putGovernance,
  verifyCertificate,
  type Approval,
  type ApprovalState,
  type BulkDeletePlan,
  type Certificate,
  type Profile,
} from "../api";

const PROFILE_TEXT: Record<Profile, { name: string; detail: string }> = {
  STANDARD: { name: "Standard", detail: "The settings below, as you choose them." },
  GXP_VAULT: {
    name: "GxP vault",
    detail: "For 21 CFR Part 11 and EU Annex 11 records: every deletion and every loosening change needs a second member, with a reason.",
  },
  SEC_17A4_AUDIT_TRAIL: {
    name: "SEC 17a-4 audit trail",
    detail: "The audit-trail alternative to WORM in Rule 17a-4(f)(2): originals kept, every change recorded, nothing kept under six years, two-person changes.",
  },
};

const KIND_TEXT: Record<Approval["kind"], string> = {
  RETENTION_CHANGE: "Retention change",
  HOLD_RELEASE: "Hold release",
  ASSET_DELETE: "Deletion",
  GOVERNANCE_CHANGE: "Governance change",
};

const STATE_TONE: Record<ApprovalState, "active" | "done" | "neutral" | "failed"> = {
  PENDING: "active",
  EXECUTED: "done",
  REJECTED: "neutral",
  EXPIRED: "neutral",
  FAILED: "failed",
};

export function GovernanceScreen() {
  const { can } = useSession();
  if (!can("retention.manage")) {
    return <PermissionDenied detail="Governance settings and approvals need the retention.manage permission." />;
  }
  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
        <ApprovalsPanel />
        <BulkDeletePanel />
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        <GovernancePanel />
        <CertificatePanel />
        <ExportPanel />
      </div>
    </div>
  );
}

function useGovernanceKeys() {
  const { organizationId } = useSession();
  const client = useQueryClient();
  return () => client.invalidateQueries({ queryKey: orgKey(organizationId, "governance") });
}

function GovernancePanel() {
  const { organizationId } = useSession();
  const settings = useQuery({
    queryKey: orgKey(organizationId, "governance", "settings"),
    queryFn: ({ signal }) => getGovernance(signal),
    enabled: Boolean(organizationId),
  });
  return (
    <Panel title="Governance" description="How changes that loosen protection are made in this organization.">
      {settings.isPending ? (
        <SkeletonRows rows={4} />
      ) : settings.isError ? (
        <ErrorState error={settings.error} onRetry={() => settings.refetch()} />
      ) : (
        <GovernanceForm key={settings.data.updated_at ?? "new"} initial={settings.data} />
      )}
    </Panel>
  );
}

function GovernanceForm({ initial }: { initial: Awaited<ReturnType<typeof getGovernance>> }) {
  const [profile, setProfile] = useState<Profile>(initial.profile);
  const [twoPerson, setTwoPerson] = useState(initial.two_person);
  const [approveDeletes, setApproveDeletes] = useState(initial.approve_deletes);
  const [reason, setReason] = useState("");
  const refresh = useGovernanceKeys();
  const save = useMutation({ mutationFn: putGovernance });

  function submit(e: FormEvent) {
    e.preventDefault();
    save.mutate(
      { profile, two_person: twoPerson, approve_deletes: approveDeletes, retention_floor: "", reason: reason.trim() },
      {
        onSuccess: async (answer) => {
          await refresh();
          if ("status" in answer) notify.info("Sent for approval", "Loosening governance needs a second member.");
          else notify.success("Governance saved");
        },
        onError: notify.error,
      },
    );
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <Field label="Profile" hint={PROFILE_TEXT[profile].detail}>
        {(props) => (
          <Select {...props} name="profile" value={profile} onChange={(e) => setProfile(e.target.value as Profile)}>
            {(Object.keys(PROFILE_TEXT) as Profile[]).map((p) => (
              <option key={p} value={p}>
                {PROFILE_TEXT[p].name}
              </option>
            ))}
          </Select>
        )}
      </Field>
      <Checkbox
        checked={twoPerson || profile !== "STANDARD"}
        disabled={profile !== "STANDARD"}
        onChange={(e) => setTwoPerson(e.target.checked)}
        label="Two-person rule: shortening retention, releasing a hold and bulk deletes wait for a second member"
      />
      <Checkbox
        checked={approveDeletes || profile === "GXP_VAULT"}
        disabled={profile === "GXP_VAULT"}
        onChange={(e) => setApproveDeletes(e.target.checked)}
        label="Every deletion needs approval"
      />
      {initial.retention_floor !== "0s" && (
        <p className="text-sm text-muted">Nothing is kept under {initial.retention_floor} while this profile applies.</p>
      )}
      <Field label="Reason" hint="Needed when the change waits for approval.">
        {(props) => <Textarea {...props} name="governance_reason" rows={2} value={reason} onChange={(e) => setReason(e.target.value)} />}
      </Field>
      <SubmitButton valid loading={save.isPending}>
        Save governance
      </SubmitButton>
      {initial.updated_by && (
        <p className="text-sm text-muted">
          Last changed by <span className="mono-data">{initial.updated_by}</span>, <Time value={initial.updated_at} />
        </p>
      )}
    </form>
  );
}

function ApprovalsPanel() {
  const { organizationId, user } = useSession();
  const me = `user:${user.id}`;
  const [state, setState] = useState<ApprovalState | "">("PENDING");
  const [deciding, setDeciding] = useState<Approval | null>(null);
  const approvals = useQuery({
    queryKey: orgKey(organizationId, "governance", "approvals", state),
    queryFn: ({ signal }) => listApprovals(state, signal),
    enabled: Boolean(organizationId),
  });

  return (
    <Panel
      title="Approvals"
      description="Changes that wait for a second member. The member who asked cannot approve their own change."
      actions={
        <Field label="Show" className="w-44">
          {(props) => (
            <Select {...props} name="approval_state" value={state} onChange={(e) => setState(e.target.value as ApprovalState | "")}>
              <option value="PENDING">Waiting</option>
              <option value="">Every approval</option>
            </Select>
          )}
        </Field>
      }
    >
      {approvals.isPending ? (
        <SkeletonRows rows={3} />
      ) : approvals.isError ? (
        <ErrorState error={approvals.error} onRetry={() => approvals.refetch()} />
      ) : approvals.data.approvals.length === 0 ? (
        <EmptyState title={state === "PENDING" ? "Nothing waits for approval" : "No approvals yet"}>
          Under the two-person rule, loosening changes appear here for another member to approve or reject.
        </EmptyState>
      ) : (
        <ul className="flex flex-col gap-3">
          {approvals.data.approvals.map((a) => (
            <li key={a.id} className="neo-in-sm p-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-2">
                  <StateBadge tone={STATE_TONE[a.state]}>{a.state.toLowerCase()}</StateBadge>
                  <span className="font-medium">{KIND_TEXT[a.kind]}</span>
                </div>
                {a.state === "PENDING" && (
                  <Button onClick={() => setDeciding(a)} disabled={a.requested_by === me}>
                    {a.requested_by === me ? "Yours: another member decides" : "Review"}
                  </Button>
                )}
              </div>
              <p className="mt-1 [overflow-wrap:anywhere]">{a.summary}</p>
              <p className="mt-1 text-sm text-muted">
                {a.reason} Asked by <span className="mono-data">{a.requested_by}</span> <Time value={a.requested_at} />
                {a.state === "PENDING" ? (
                  <>
                    , expires <Time value={a.expires_at} />
                  </>
                ) : (
                  a.decided_by && (
                    <>
                      ; decided by <span className="mono-data">{a.decided_by}</span>
                      {a.result && `: ${a.result}`}
                    </>
                  )
                )}
              </p>
            </li>
          ))}
        </ul>
      )}
      {deciding && <DecideDialog approval={deciding} onClose={() => setDeciding(null)} />}
    </Panel>
  );
}

function DecideDialog({ approval, onClose }: { approval: Approval; onClose: () => void }) {
  const [note, setNote] = useState("");
  const refresh = useGovernanceKeys();
  const decide = useMutation({ mutationFn: decideApproval });

  function run(approve: boolean) {
    decide.mutate(
      { id: approval.id, approve, note: note.trim() },
      {
        onSuccess: async (a) => {
          await refresh();
          notify.success(approve ? (a.state === "FAILED" ? `Approved, but it failed: ${a.result}` : "Approved and carried out") : "Rejected");
          onClose();
        },
        onError: notify.error,
      },
    );
  }

  return (
    <Modal
      open
      onOpenChange={(open) => !open && onClose()}
      title={KIND_TEXT[approval.kind]}
      description={approval.summary}
      footer={
        <>
          <Button onClick={() => run(false)} disabled={decide.isPending}>
            Reject
          </Button>
          <Button variant="accent" onClick={() => run(true)} loading={decide.isPending}>
            Approve and carry out
          </Button>
        </>
      }
    >
      <DefList
        items={[
          { label: "Asked by", value: <span className="mono-data">{approval.requested_by}</span> },
          { label: "Reason", value: approval.reason },
          { label: "Change", value: <pre className="mono-data max-h-48 overflow-auto text-sm whitespace-pre-wrap">{JSON.stringify(approval.payload, null, 2)}</pre> },
        ]}
      />
      <Field label="Note" hint="Recorded with your decision.">
        {(props) => <Textarea {...props} name="decision_note" rows={2} value={note} onChange={(e) => setNote(e.target.value)} />}
      </Field>
    </Modal>
  );
}

function BulkDeletePanel() {
  const { can } = useSession();
  const [dataset, setDataset] = useState("");
  const [prefix, setPrefix] = useState("");
  const [ids, setIds] = useState("");
  const [reason, setReason] = useState("");
  const [plan, setPlan] = useState<BulkDeletePlan | null>(null);
  const [confirming, setConfirming] = useState(false);
  const preview = useMutation({ mutationFn: bulkDelete });

  const assetIds = ids.split(/[\s,]+/).filter(Boolean);
  const valid = (dataset.trim() !== "" || prefix.trim() !== "" || assetIds.length > 0) && reason.trim() !== "";
  const request = (dryRun: boolean) => ({
    ...(assetIds.length > 0 && { asset_ids: assetIds }),
    ...(dataset.trim() && { dataset: dataset.trim().toLowerCase() }),
    ...(prefix.trim() && { path_prefix: prefix.trim() }),
    reason: reason.trim(),
    dry_run: dryRun,
  });

  if (!can("assets.delete")) return null;

  return (
    <Panel title="Delete many assets" description="Preview first: the dry run counts what matches and what holds, retention or locks would keep.">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          preview.mutate(request(true), { onSuccess: setPlan, onError: notify.error });
        }}
        className="flex flex-col gap-4"
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Dataset">
            {(props) => <Input {...props} name="bulk_dataset" value={dataset} className="mono-data" autoComplete="off" spellCheck={false} onChange={(e) => setDataset(e.target.value)} />}
          </Field>
          <Field label="Path starts with">
            {(props) => <Input {...props} name="bulk_prefix" value={prefix} className="mono-data" autoComplete="off" spellCheck={false} onChange={(e) => setPrefix(e.target.value)} />}
          </Field>
        </div>
        <Field label="Or these asset ids" hint="Separated by spaces, commas or lines; at most 10,000.">
          {(props) => <Textarea {...props} name="bulk_ids" rows={2} value={ids} className="mono-data" spellCheck={false} onChange={(e) => setIds(e.target.value)} />}
        </Field>
        <Field label="Reason" required>
          {(props) => <Textarea {...props} name="bulk_reason" rows={2} value={reason} onChange={(e) => setReason(e.target.value)} />}
        </Field>
        <SubmitButton valid={valid} loading={preview.isPending} hint="Choose assets and give a reason.">
          Preview deletion
        </SubmitButton>
      </form>

      {plan && (
        <div role="status" className="neo-in-sm mt-5 flex flex-col gap-3 p-3">
          <DefList
            items={[
              { label: "Matched", value: `${plan.matched.toLocaleString()} assets, ${formatBytes(plan.bytes)}` },
              {
                label: "Held back now",
                value:
                  Object.keys(plan.blocked).length === 0
                    ? `Nothing, of ${plan.checked.toLocaleString()} checked`
                    : Object.entries(plan.blocked)
                        .map(([kind, n]) => `${n.toLocaleString()} by ${kind.replaceAll("_", " ")}`)
                        .join(", ") + ` (of ${plan.checked.toLocaleString()} checked; they wait until free)`,
              },
            ]}
          />
          {plan.matched > 0 && (
            <Button variant="accent" className="self-start" onClick={() => setConfirming(true)}>
              Delete {plan.matched.toLocaleString()} assets
            </Button>
          )}
        </div>
      )}

      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={`Delete ${plan?.matched.toLocaleString() ?? 0} assets?`}
        description="Each is soft-deleted now and purged after its grace period, unless restored. Under the two-person rule this waits for a second member."
        confirmLabel="Delete assets"
        confirmText="DELETE"
        onConfirm={async () => {
          const done = await bulkDelete(request(false));
          setPlan(null);
          if (done.approval_id) notify.info("Sent for approval", "A second member approves it under Approvals.");
          else notify.success(`${done.scheduled.toLocaleString()} assets scheduled for deletion`);
        }}
      />
    </Panel>
  );
}

function CertificatePanel() {
  const [deletionId, setDeletionId] = useState("");
  const [result, setResult] = useState<{ cert: Certificate; hashOk: boolean; signature: string } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function look(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const cert = await deletionCertificate(deletionId.trim());
      const key = await publicKey().catch(() => null);
      const check = await verifyCertificate(cert, key?.public_key ?? null);
      setResult({ cert, ...check });
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy(false);
    }
  }

  const signatureText: Record<string, string> = {
    valid: "Signature valid for the platform's key",
    invalid: "Signature does not match",
    unsigned: "Not signed on this platform; only its SHA-256 is checked",
    unsupported: "This browser cannot check Ed25519; verify it with osp certificate --verify",
  };

  return (
    <Panel title="Disposition certificates" description="Every permanent deletion leaves one, checked here in your browser.">
      <form onSubmit={look} className="flex flex-col gap-3">
        <Field label="Deletion id">
          {(props) => <Input {...props} name="deletion_id" value={deletionId} className="mono-data" autoComplete="off" spellCheck={false} onChange={(e) => setDeletionId(e.target.value)} />}
        </Field>
        <SubmitButton valid={deletionId.trim() !== ""} loading={busy} hint="Paste a deletion id.">
          Check certificate
        </SubmitButton>
      </form>
      {error && (
        <p role="alert" className="mt-3 text-base font-medium">
          {error}
        </p>
      )}
      {result && (
        <div role="status" className="neo-in-sm mt-4 flex flex-col gap-2 p-3 text-sm">
          <StateBadge tone={result.hashOk && result.signature === "valid" ? "done" : result.hashOk && result.signature !== "invalid" ? "warning" : "failed"}>
            {result.hashOk ? signatureText[result.signature] : "SHA-256 does not match the body"}
          </StateBadge>
          <DefList
            items={[
              { label: "File", value: String(result.cert.body.file_name ?? "") },
              { label: "SHA-256", value: <span className="mono-data [overflow-wrap:anywhere]">{String(result.cert.body.checksum_sha256 ?? "")}</span> },
              { label: "Requested by", value: <span className="mono-data">{String(result.cert.body.requested_by ?? "")}</span> },
              { label: "Destroyed", value: <Time value={String(result.cert.body.completed_at ?? "")} /> },
              { label: "Key", value: <span className="mono-data">{result.cert.key_id || "none"}</span> },
            ]}
          />
        </div>
      )}
    </Panel>
  );
}
