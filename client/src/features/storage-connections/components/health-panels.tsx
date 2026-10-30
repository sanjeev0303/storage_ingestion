"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { FormProvider, useForm } from "react-hook-form";
import { Button } from "@/components/ui/button";
import { ConfirmDialog, Modal } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { EmptyState, ErrorState, SkeletonRows } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { SubmitButton } from "@/components/ui/submit-button";
import { Time } from "@/components/ui/time";
import { notify } from "@/components/ui/toast";
import { describeError, isApiError } from "@/lib/api/errors";
import { useOrganizationId, useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { zodResolver } from "@/lib/validation/resolver";
import {
  checkPosture,
  discardStaged,
  getPosture,
  listDrift,
  promoteCredentials,
  resolveDrift,
  rollbackCredentials,
  stageCredentials,
  testResultFromError,
  updateConnection,
} from "../api";
import { useRefreshStorage } from "../hooks";
import { buildCredentials, emptyCredentials, rotateSchema, type RotateForm } from "../schema";
import type { ChecksumMode, DriftKind, PostureStatus, StorageConnection, TestResponse } from "../types";
import { CredentialFields } from "./credential-fields";
import { TestResult } from "./test-result";

const CHECK_TEXT: Record<string, string> = {
  bucket: "Bucket settings readable",
  public_access_block: "Public access blocked",
  public_policy: "Bucket policy not public",
  public_acl: "ACLs grant nothing to everyone",
  object_ownership: "ACLs disabled",
  default_encryption: "Encrypted at rest by default",
  sse_c_denied: "Customer-provided keys (SSE-C) refused",
  versioning: "Versioning on",
  object_lock: "Object Lock on",
  lifecycle_rules: "No lifecycle rule deletes the platform's objects",
  lifecycle_drift: "Lifecycle rules unchanged",
  incomplete_upload_expiry: "Incomplete uploads expire",
  credential_scope: "Credentials limited to this bucket",
  bucket_owner: "Bucket owner pinned",
  guessable_name: "Bucket name hard to guess",
  self_test: "PUT, multipart, GET and DELETE work",
  region: "Bucket where the connection says",
  customer_key: "Customer-managed key on every write",
};

const STATUS: Record<PostureStatus, { label: string; tone: "done" | "warning" | "failed" | "neutral" }> = {
  PASS: { label: "Pass", tone: "done" },
  WARN: { label: "Warning", tone: "warning" },
  FAIL: { label: "Fail", tone: "failed" },
  NOT_CHECKED: { label: "Not checked", tone: "neutral" },
};

const ORDER: PostureStatus[] = ["FAIL", "WARN", "NOT_CHECKED", "PASS"];

export function PosturePanel({ connection }: { connection: StorageConnection }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const refresh = useRefreshStorage();
  const [probe, setProbe] = useState(false);
  const posture = useQuery({
    queryKey: orgKey(organizationId, "storage", "posture", connection.id),
    queryFn: ({ signal }) => getPosture(connection.id, signal),
    enabled: Boolean(organizationId),
    retry: false,
  });
  const check = useMutation({ mutationFn: checkPosture });
  const notChecked = posture.isError && isApiError(posture.error) && posture.error.code === "not_checked";

  return (
    <Panel
      title="Bucket posture"
      description="What the bucket's own settings allow: public access, encryption, versioning, lock, lifecycle rules and how broad the keys are. Checked at every test and once a day."
      actions={
        posture.data && (
          <StateBadge tone={posture.data.phi_safe ? "done" : "neutral"}>{posture.data.phi_safe ? "Safe for PHI" : "Not safe for PHI"}</StateBadge>
        )
      }
    >
      {posture.isPending ? (
        <SkeletonRows rows={5} />
      ) : notChecked ? (
        <EmptyState title="Not checked yet">Run a connection test, or check now.</EmptyState>
      ) : posture.isError ? (
        <ErrorState error={posture.error} onRetry={() => posture.refetch()} />
      ) : (
        <>
          <p className="mb-3 text-sm text-muted">
            {posture.data.location && (
              <>
                In <span className="mono-data">{posture.data.location}</span>
                {posture.data.location_verified ? " (confirmed by the provider). " : " (declared, not confirmed by the provider). "}
              </>
            )}
            Checked <Time value={posture.data.checked_at} />
            {posture.data.owner_account && (
              <>
                {" "}
                for account <span className="mono-data">{posture.data.owner_account}</span>
              </>
            )}
          </p>
          <ul className="flex flex-col gap-2.5">
            {[...posture.data.checks]
              .sort((a, b) => ORDER.indexOf(a.status) - ORDER.indexOf(b.status))
              .map((c) => (
                <li key={c.name} className="grid gap-1 sm:grid-cols-[8rem_1fr] sm:gap-3">
                  <span>
                    <StateBadge tone={STATUS[c.status].tone}>{STATUS[c.status].label}</StateBadge>
                  </span>
                  <span className="min-w-0">
                    <span className="font-medium">{CHECK_TEXT[c.name] ?? c.name}</span>
                    {c.status !== "PASS" || c.detail ? (
                      <span className="block text-sm text-muted [overflow-wrap:anywhere]">{c.detail}</span>
                    ) : null}
                  </span>
                </li>
              ))}
          </ul>
        </>
      )}
      {can("storage.manage") && (
        <div className="mt-5 flex flex-wrap items-center gap-4">
          <Button
            loading={check.isPending}
            onClick={() =>
              check.mutate(
                { id: connection.id, probe },
                {
                  onSuccess: async (r) => {
                    await refresh();
                    notify.success(r.worst === "PASS" ? "Posture checked: all passed" : `Posture checked: worst result ${STATUS[r.worst].label.toLowerCase()}`);
                  },
                  onError: notify.error,
                },
              )
            }
          >
            Check now
          </Button>
          <Checkbox checked={probe} onChange={(e) => setProbe(e.target.checked)} label="Also try a write with a customer-provided key, which must be refused" />
        </div>
      )}
    </Panel>
  );
}

const CHECKSUM_TEXT: Record<ChecksumMode, string> = {
  AUTO: "Automatic: newer checksums on AWS, only when required elsewhere",
  WHEN_SUPPORTED: "Always send the newer CRC checksums",
  WHEN_REQUIRED: "Only when an operation requires one (older S3-compatible stores)",
};

export function IntegritySettings({ connection }: { connection: StorageConnection }) {
  const refresh = useRefreshStorage();
  const pinnable = connection.provider === "AWS_S3" || connection.provider === "GOOGLE_CLOUD_STORAGE";
  const [mode, setMode] = useState<ChecksumMode>(connection.checksum_mode ?? "AUTO");
  const [owner, setOwner] = useState(connection.expected_owner ?? "");
  const [key, setKey] = useState(connection.encryption_key ?? "");
  const [refused, setRefused] = useState<TestResponse | null>(null);
  const keyChanged = key.trim() !== (connection.encryption_key ?? "");
  const save = useMutation({ mutationFn: () => updateConnection(connection.id, {
    revision: connection.revision,
    ...(mode !== (connection.checksum_mode ?? "AUTO") && { checksum_mode: mode }),
    ...(pinnable && owner.trim() !== (connection.expected_owner ?? "") && { expected_owner: owner.trim() }),
    ...(keyChanged && { encryption_key: key.trim() }),
  }) });
  const ownerValid = owner.trim() === "" || /^[0-9]{6,20}$/.test(owner.trim());
  const changed =
    mode !== (connection.checksum_mode ?? "AUTO") || (pinnable && owner.trim() !== (connection.expected_owner ?? "")) || keyChanged;
  const keyLabel =
    connection.provider === "GOOGLE_CLOUD_STORAGE"
      ? "Cloud KMS key name"
      : connection.provider === "AZURE_BLOB"
        ? "Encryption scope"
        : "KMS key ARN, ID or alias";
  const keysSupported = connection.provider !== "CLOUDFLARE_R2";

  function submit(e: FormEvent) {
    e.preventDefault();
    setRefused(null);
    save.mutate(undefined, {
      onSuccess: async () => {
        await refresh();
        notify.success("Saved; the change passed a connection test");
      },
      onError: (error) => {
        const result = testResultFromError(error);
        if (result) setRefused(result);
        else notify.error(error);
      },
    });
  }

  return (
    <Panel title="Integrity settings" description="Changing either is tested against the bucket before it is saved." tone="in">
      <form onSubmit={submit} className="flex flex-col gap-4">
        <Field label="Request checksums">
          {(props) => (
            <Select {...props} name="checksum_mode" value={mode} onChange={(e) => setMode(e.target.value as ChecksumMode)}>
              {(Object.keys(CHECKSUM_TEXT) as ChecksumMode[]).map((m) => (
                <option key={m} value={m}>
                  {CHECKSUM_TEXT[m]}
                </option>
              ))}
            </Select>
          )}
        </Field>
        {pinnable && (
          <Field
            label={connection.provider === "AWS_S3" ? "Bucket owner account" : "Bucket owner project number"}
            hint="Every request names it, so a bucket re-created under this name by someone else is refused. Empty unpins."
            error={ownerValid ? undefined : "Digits only, such as 123456789012."}
          >
            {(props) => (
              <Input {...props} name="expected_owner" inputMode="numeric" autoComplete="off" spellCheck={false} className="mono-data"
                value={owner} onChange={(e) => setOwner(e.target.value)} />
            )}
          </Field>
        )}
        {keysSupported && (
          <Field
            label={`Customer-managed key: ${keyLabel}`}
            hint="Every write, copy and upload is encrypted with it, and a write with it is tested before saving. Empty uses the bucket's default."
          >
            {(props) => (
              <Input {...props} name="encryption_key" autoComplete="off" spellCheck={false} className="mono-data"
                value={key} onChange={(e) => setKey(e.target.value)} />
            )}
          </Field>
        )}
        {refused && <TestResult result={refused} note="Not saved: the connection failed its test with this change." />}
        <SubmitButton valid={changed && ownerValid} loading={save.isPending} hint="Change a setting to save it.">
          Save settings
        </SubmitButton>
      </form>
    </Panel>
  );
}

export function SecondCredential({ connection }: { connection: StorageConnection }) {
  const refresh = useRefreshStorage();
  const [staging, setStaging] = useState(false);
  const [confirm, setConfirm] = useState<"promote" | "rollback" | "discard" | null>(null);
  const [result, setResult] = useState<{ value: TestResponse; note: string } | null>(null);
  const rollbackOpen = Boolean(connection.previous_credential_until && new Date(connection.previous_credential_until) > new Date());

  return (
    <Panel
      title="Second credential"
      description="Stage new keys beside the current ones, promote them after a fresh test, and roll back for seven days if anything breaks."
      tone="in"
    >
      {connection.staged_credential ? (
        <p className="text-base">
          Version {connection.staged_credential.rotation} is staged
          {connection.staged_credential.staged_at && (
            <>
              , <Time value={connection.staged_credential.staged_at} />
            </>
          )}
          . It is not in use yet.
        </p>
      ) : (
        <p className="text-base text-muted">Nothing staged.</p>
      )}
      {rollbackOpen && (
        <p className="mt-2 text-sm text-muted">
          The replaced credential can be put back until <Time value={connection.previous_credential_until} />.
        </p>
      )}
      <div className="mt-4 flex flex-wrap gap-2">
        {connection.staged_credential ? (
          <>
            <Button variant="primary" onClick={() => setConfirm("promote")}>
              Promote
            </Button>
            <Button onClick={() => setConfirm("discard")}>Discard</Button>
          </>
        ) : (
          <Button onClick={() => setStaging(true)}>Stage new credentials</Button>
        )}
        {rollbackOpen && <Button onClick={() => setConfirm("rollback")}>Roll back</Button>}
      </div>
      {result && (
        <div className="mt-4">
          <TestResult result={result.value} note={result.note} />
        </div>
      )}

      {staging && (
        <StageDialog
          connection={connection}
          onClose={() => setStaging(false)}
          onStaged={(value) => setResult({ value, note: "Staged. The current credentials stay in use until you promote these." })}
        />
      )}
      <ConfirmDialog
        open={confirm === "promote"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Promote the staged credentials?"
        description="They are tested again, writing included, and only then put in use. The current ones are kept for a rollback for seven days."
        confirmLabel="Test and promote"
        onConfirm={async () => {
          try {
            const value = await promoteCredentials(connection.id);
            setResult({ value, note: "Promoted. Revoke the old keys at the provider once nothing else uses them." });
          } catch (error) {
            const refused = testResultFromError(error);
            if (!refused) throw error;
            setResult({ value: refused, note: "Not promoted: the staged credentials failed their test. The current ones are still in use." });
          }
          await refresh();
        }}
      />
      <ConfirmDialog
        open={confirm === "rollback"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Roll back to the replaced credentials?"
        description="The credentials the last promotion replaced go back in use at once, if the provider still accepts them."
        confirmLabel="Roll back"
        onConfirm={async () => {
          await rollbackCredentials(connection.id);
          await refresh();
          notify.success("Rolled back");
        }}
      />
      <ConfirmDialog
        open={confirm === "discard"}
        onOpenChange={(open) => !open && setConfirm(null)}
        title="Discard the staged credentials?"
        description="They are erased without ever being used."
        confirmLabel="Discard"
        onConfirm={async () => {
          await discardStaged(connection.id);
          await refresh();
        }}
      />
    </Panel>
  );
}

function StageDialog({ connection, onClose, onStaged }: { connection: StorageConnection; onClose: () => void; onStaged: (r: TestResponse) => void }) {
  const refresh = useRefreshStorage();
  const [refused, setRefused] = useState<TestResponse | null>(null);
  const [formError, setFormError] = useState<string | null>(null);
  const form = useForm<RotateForm>({
    resolver: zodResolver<RotateForm>(rotateSchema(connection.auth_method)),
    defaultValues: { credentials: { ...emptyCredentials } },
    mode: "onTouched",
  });
  const stage = useMutation({
    mutationFn: (values: RotateForm) => stageCredentials(connection.id, buildCredentials(connection.auth_method, values.credentials)),
  });

  async function submit(values: RotateForm) {
    setRefused(null);
    setFormError(null);
    try {
      const value = await stage.mutateAsync(values);
      await refresh();
      onStaged(value);
      onClose();
    } catch (error) {
      const result = testResultFromError(error);
      if (result) setRefused(result);
      else setFormError(describeError(error));
    } finally {
      // the mutation holds the request body, which holds the new secret
      stage.reset();
      form.reset({ credentials: { ...emptyCredentials } });
    }
  }

  return (
    <Modal
      open
      onOpenChange={(open) => !open && onClose()}
      title="Stage new credentials"
      description="They are tested now, writing included, and kept beside the current ones without being used."
    >
      <FormProvider {...form}>
        <form className="flex flex-col gap-4" onSubmit={form.handleSubmit(submit)} noValidate autoComplete="off">
          <CredentialFields method={connection.auth_method} />
          {refused && <TestResult result={refused} note="Not staged. Enter them again to retry." />}
          {formError && (
            <p role="alert" className="text-base font-medium">
              {formError}
            </p>
          )}
          <div className="flex flex-wrap justify-end gap-2">
            <Button onClick={onClose}>Cancel</Button>
            <SubmitButton valid={form.formState.isValid} loading={stage.isPending} hint="Enter the new credentials.">
              Test and stage
            </SubmitButton>
          </div>
        </form>
      </FormProvider>
    </Modal>
  );
}

const DRIFT_TEXT: Record<DriftKind, string> = {
  OVERWRITTEN: "Replaced",
  DELETED: "Deleted",
  NEW_VERSION: "New version on top",
  UNREADABLE: "Unreadable",
  UNKNOWN_OBJECT: "Not written by the platform",
};

export function DriftPanel({ connection }: { connection: StorageConnection }) {
  const organizationId = useOrganizationId();
  const { can } = useSession();
  const refresh = useRefreshStorage();
  const drift = useQuery({
    queryKey: orgKey(organizationId, "storage", "drift", connection.id),
    queryFn: ({ signal }) => listDrift(connection.id, false, signal),
    enabled: Boolean(organizationId),
  });
  const resolve = useMutation({ mutationFn: resolveDrift });

  return (
    <Panel
      title="Changes outside the platform"
      description="A scan compares what the platform wrote with what the bucket holds now, and looks for objects it never wrote. Snapshots holding a changed object are marked degraded."
    >
      {drift.isPending ? (
        <SkeletonRows rows={3} />
      ) : drift.isError ? (
        <ErrorState error={drift.error} onRetry={() => drift.refetch()} />
      ) : drift.data.findings.length === 0 ? (
        <EmptyState title="Nothing changed outside the platform">The scan visits each connection every few hours.</EmptyState>
      ) : (
        <ul className="flex flex-col gap-3">
          {drift.data.findings.map((f) => (
            <li key={f.id} className="neo-in-sm p-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <StateBadge tone={f.kind === "UNKNOWN_OBJECT" || f.kind === "NEW_VERSION" ? "warning" : "failed"}>{DRIFT_TEXT[f.kind]}</StateBadge>
                {can("storage.manage") && (
                  <Button
                    disabled={resolve.isPending}
                    onClick={() => resolve.mutate(f.id, { onSuccess: () => refresh(), onError: notify.error })}
                  >
                    Mark resolved
                  </Button>
                )}
              </div>
              <p className="mono-data mt-2 text-sm [overflow-wrap:anywhere]">{f.object_key}</p>
              <p className="mt-1 text-base">{f.detail}</p>
              <p className="mt-1 text-sm text-muted">
                Found <Time value={f.detected_at} />
              </p>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}
