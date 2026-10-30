"use client";

import { useMutation } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox } from "@/components/ui/field";
import { CopyButton } from "@/components/ui/copy-button";
import { DefList, PageHeader } from "@/components/ui/page";
import { Panel } from "@/components/ui/panel";
import { ErrorState, SkeletonRows } from "@/components/ui/states";
import { Outcome, StateBadge } from "@/components/ui/status";
import { Time } from "@/components/ui/time";
import { ApiError, isApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import {
  deleteConnection,
  disableConnection,
  enableConnection,
  setDefaultConnection,
  testConnection,
} from "../api";
import { useRefreshStorage, useStorageConnection } from "../hooks";
import { AUTH_LABELS, PROVIDER_LABELS } from "../schema";
import type { StorageConnection, TestResponse } from "../types";
import { CAPABILITY_ROWS } from "./capability-rows";
import { EditDialog, RotateDialog } from "./connection-dialogs";
import { DriftPanel, IntegritySettings, PosturePanel, SecondCredential } from "./health-panels";
import { CATEGORY_MESSAGES, TestResult } from "./test-result";

export function describeInUse(error: ApiError) {
  const body = (error.body ?? {}) as {
    active_uploads?: number;
    live_assets?: number;
    datasets?: number;
    is_default?: boolean;
  };
  const reasons: string[] = [];
  if (body.active_uploads) reasons.push(`${body.active_uploads} unfinished ${body.active_uploads === 1 ? "upload" : "uploads"}`);
  if (body.live_assets) reasons.push(`${body.live_assets} ${body.live_assets === 1 ? "asset" : "assets"} stored on it`);
  if (body.datasets) reasons.push(`${body.datasets} ${body.datasets === 1 ? "dataset" : "datasets"} that upload to it`);
  if (body.is_default) reasons.push("it is the organization's default connection");
  if (reasons.length === 0) return error.message;
  return (
    `It cannot be deleted yet: ${reasons.join(", ")}. ` +
    "Objects are never moved to another provider. Delete or let expire what depends on it, set another default if needed, then try again. " +
    "Deleting a connection never touches the contents of your bucket."
  );
}

export function ConnectionDetail({ id }: { id: string }) {
  const query = useStorageConnection(id);

  if (query.isPending) return <SkeletonRows rows={6} />;
  if (query.isError) return <ErrorState error={query.error} onRetry={() => query.refetch()} />;
  return <Loaded connection={query.data} />;
}

function Loaded({ connection }: { connection: StorageConnection }) {
  const { can } = useSession();
  const router = useRouter();
  const refresh = useRefreshStorage();
  const manage = can("storage.manage");
  const isPlatform = connection.provider === "PLATFORM_MANAGED";

  const [result, setResult] = useState<{ value: TestResponse; note?: string } | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);
  const [probe, setProbe] = useState(false);
  const [dialog, setDialog] = useState<"edit" | "rotate" | "default" | "disable" | "delete" | null>(null);

  const test = useMutation({ mutationFn: () => testConnection(connection.id, probe) });
  const enable = useMutation({ mutationFn: () => enableConnection(connection.id) });

  async function run(action: () => Promise<TestResponse>, note: (r: TestResponse) => string) {
    setActionError(null);
    setResult(null);
    try {
      const value = await action();
      setResult({ value, note: note(value) });
      await refresh();
    } catch (error) {
      setActionError(error);
    }
  }

  async function disable() {
    try {
      await disableConnection(connection.id);
    } catch (error) {
      if (isApiError(error) && error.code === "connection_in_use") {
        throw new ApiError({
          status: error.status,
          code: error.code,
          message: "This is the organization's default connection. Set another connection as the default first, then disable this one.",
        });
      }
      throw error;
    }
    await refresh();
  }

  async function remove() {
    try {
      await deleteConnection(connection.id);
    } catch (error) {
      if (isApiError(error) && error.code === "connection_in_use") {
        throw new ApiError({ status: error.status, code: error.code, message: describeInUse(error), body: error.body });
      }
      throw error;
    }
    await refresh();
    router.push("/app/storage");
  }

  const canTest = manage && !isPlatform && (connection.status === "PENDING" || connection.status === "ACTIVE");
  const canEnable = manage && !isPlatform && connection.status === "DISABLED";
  const canSetDefault = manage && connection.status === "ACTIVE" && !connection.is_default;
  const canDisable = manage && !isPlatform && (connection.status === "ACTIVE" || connection.status === "PENDING");
  const canDelete = manage && !isPlatform && (connection.status === "PENDING" || connection.status === "DISABLED");
  const canRotate =
    manage && !isPlatform && connection.has_credentials && connection.auth_method !== "ASSUME_ROLE";

  return (
    <>
      <PageHeader
        title={connection.name}
        crumbs={[{ label: "Storage", href: "/app/storage" }, { label: connection.name }]}
        description={isPlatform ? "Platform-managed storage cannot be changed, tested, disabled or deleted." : PROVIDER_LABELS[connection.provider]}
        actions={
          manage && !isPlatform ? (
            <>
              <Button onClick={() => setDialog("edit")}>Edit</Button>
              {canRotate && <Button onClick={() => setDialog("rotate")}>Rotate credentials</Button>}
            </>
          ) : undefined
        }
      />

      <div className="grid gap-6 lg:grid-cols-3">
        <div className="flex min-w-0 flex-col gap-6 lg:col-span-2">
          <Panel title="Status" actions={<StatusBadges connection={connection} />}>
            <StatusNote connection={connection} />
            <div className="mt-4 flex flex-wrap gap-2">
              {canSetDefault && <Button onClick={() => setDialog("default")}>Set as default</Button>}
              {canEnable && (
                <Button
                  variant="primary"
                  loading={enable.isPending}
                  onClick={() =>
                    run(
                      () => enable.mutateAsync(),
                      (r) => (r.ok ? "The connection is Active again." : "The connection stays Disabled. It still serves what it already holds.")
                    )
                  }
                >
                  Enable
                </Button>
              )}
              {canDisable && <Button onClick={() => setDialog("disable")}>Disable</Button>}
              {canDelete && <Button onClick={() => setDialog("delete")}>Delete</Button>}
            </div>
            {manage && !isPlatform && connection.status === "ACTIVE" && (
              <p className="mt-3 max-w-prose text-sm text-muted">Disable the connection before deleting it.</p>
            )}
          </Panel>

          {canTest && (
            <Panel title="Test" description="Checks the endpoint, the credentials, the bucket and a listing.">
              <Checkbox
                label="Also write, read back and delete one small test object under the ready prefix"
                checked={probe || connection.status === "PENDING"}
                disabled={connection.status === "PENDING"}
                onChange={(e) => setProbe(e.target.checked)}
              />
              {connection.status === "PENDING" && (
                <p className="mt-2 max-w-prose text-sm text-muted">
                  Always done before a new connection becomes Active: every connection takes uploads, so a key that can only read
                  must fail here, not on the first upload.
                </p>
              )}
              <div className="mt-4">
                <Button
                  variant="primary"
                  loading={test.isPending}
                  onClick={() =>
                    run(
                      () => test.mutateAsync(),
                      (r) =>
                        r.ok
                          ? connection.status === "PENDING"
                            ? "The connection is now Active."
                            : ""
                          : "The connection keeps its current status. The failure is recorded on it."
                    )
                  }
                >
                  Test connection
                </Button>
              </div>
            </Panel>
          )}

          {actionError != null && <ErrorState error={actionError} title="That action did not complete" />}
          {result && <TestResult result={result.value} note={result.note || undefined} />}

          <Panel title="Configuration">
            <DefList
              items={[
                { label: "Provider", value: PROVIDER_LABELS[connection.provider] },
                { label: "Authentication", value: AUTH_LABELS[connection.auth_method] },
                { label: "Bucket or container", value: connection.bucket ?? "Not applicable" },
                { label: "Region", value: connection.region ?? "Not set" },
                { label: "Endpoint", value: connection.endpoint ?? "Provider default" },
                { label: "Account", value: connection.account_id ?? "Not set" },
                { label: "Path-style addressing", value: connection.force_path_style ? "On" : "Off" },
                { label: "Role ARN", value: connection.role_arn ?? "Not set" },
                { label: "Staging mode", value: connection.staging_mode ?? "Not set" },
                { label: "Staging prefix", value: connection.staging_prefix ?? "Not set" },
                { label: "Ready prefix", value: connection.ready_prefix ?? "Not set" },
                { label: "Created", value: <Time value={connection.created_at} /> },
              ]}
            />
            {connection.external_id && (
              <div className="mt-4 flex flex-wrap items-center gap-3">
                <span className="text-sm text-muted">External ID for the role trust policy</span>
                <code className="mono-data break-all">{connection.external_id}</code>
                <CopyButton value={connection.external_id} label="Copy external ID" />
              </div>
            )}
          </Panel>

          {!isPlatform && connection.status !== "PENDING" && <PosturePanel connection={connection} />}
          {!isPlatform && connection.status !== "PENDING" && <DriftPanel connection={connection} />}
        </div>

        <aside className="flex min-w-0 flex-col gap-6">
          <Panel title="Last test" tone="in">
            <LastTest connection={connection} />
          </Panel>
          <Panel title="Credentials" tone="in">
            <DefList
              items={[
                {
                  label: "Stored",
                  value: connection.has_credentials
                    ? `Yes, version ${connection.credential_rotation}`
                    : connection.auth_method === "ASSUME_ROLE" || isPlatform
                      ? "None needed"
                      : "No",
                },
                { label: "Last rotated", value: <Time value={connection.credential_rotated_at} /> },
              ]}
            />
            <p className="mt-3 text-sm text-muted">Secrets are never shown after they are saved.</p>
          </Panel>
          {canRotate && <SecondCredential connection={connection} />}
          {manage && !isPlatform && <IntegritySettings key={connection.revision} connection={connection} />}
          <Panel title="Revision" tone="in">
            <p className="mono-data" aria-label={`Revision ${connection.revision}`}>
              {connection.revision}
            </p>
            <p className="mt-1 text-sm text-muted">Edits are checked against this number so two people cannot overwrite each other.</p>
          </Panel>
          {connection.capabilities && (
            <Panel title="Capabilities" description="From the last passing test." tone="in">
              <dl className="flex flex-col gap-2">
                {CAPABILITY_ROWS.map((row) => (
                  <div key={row.label} className="flex justify-between gap-3 text-base">
                    <dt className="text-muted">{row.label}</dt>
                    <dd className="text-right font-medium">{row.value(connection.capabilities!)}</dd>
                  </div>
                ))}
              </dl>
              {connection.capabilities_checked_at && (
                <p className="mt-3 text-sm text-muted">
                  Checked <Time value={connection.capabilities_checked_at} />
                </p>
              )}
            </Panel>
          )}
        </aside>
      </div>

      {dialog === "edit" && <EditDialog connection={connection} open onOpenChange={(open) => !open && setDialog(null)} />}
      {dialog === "rotate" && (
        <RotateDialog
          connection={connection}
          open
          onOpenChange={(open) => !open && setDialog(null)}
          onRotated={(value) => {
            setActionError(null);
            setResult({ value, note: "The new credentials are stored. The old ones are gone." });
          }}
        />
      )}
      <ConfirmDialog
        open={dialog === "default"}
        onOpenChange={(open) => !open && setDialog(null)}
        title={`Make ${connection.name} the default?`}
        description="New uploads that do not name a connection will go here. Objects already stored stay where they are; nothing moves."
        confirmLabel="Set as default"
        onConfirm={async () => {
          await setDefaultConnection(connection.id);
          await refresh();
        }}
      />
      <ConfirmDialog
        open={dialog === "disable"}
        onOpenChange={(open) => !open && setDialog(null)}
        title={`Disable ${connection.name}?`}
        description="It stops taking new uploads. Data already stored keeps being read, processed and deleted through it, so the credentials must keep working. You can enable it again; enabling tests it first."
        confirmLabel="Disable connection"
        onConfirm={disable}
      />
      <ConfirmDialog
        open={dialog === "delete"}
        onOpenChange={(open) => !open && setDialog(null)}
        title={`Delete ${connection.name}?`}
        description="The connection and its stored credentials are erased and cannot be recovered. The contents of the bucket are never touched, and nothing is moved to another provider. It is refused while uploads, assets or datasets still depend on it."
        confirmLabel="Delete connection"
        confirmText={connection.name}
        onConfirm={remove}
      />
    </>
  );
}

function StatusBadges({ connection }: { connection: StorageConnection }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <StateBadge tone={connection.status === "ACTIVE" ? "done" : connection.status === "PENDING" ? "attention" : "neutral"}>
        {connection.status === "ACTIVE" ? "Active" : connection.status === "PENDING" ? "Pending" : connection.status === "DISABLED" ? "Disabled" : connection.status}
      </StateBadge>
      {connection.is_default && <StateBadge tone="active">Default</StateBadge>}
      {connection.provider === "PLATFORM_MANAGED" && <StateBadge>Platform-managed</StateBadge>}
    </div>
  );
}

function StatusNote({ connection }: { connection: StorageConnection }) {
  const text: Record<string, string> = {
    PENDING: "Created but not tested. It cannot take uploads until a test passes.",
    ACTIVE: "Takes new uploads. Only Active connections do.",
    DISABLED: "Takes no new uploads. Data already stored here is still served, so keep the credentials working.",
  };
  return <p className="max-w-prose text-base text-muted">{text[connection.status] ?? ""}</p>;
}

function LastTest({ connection }: { connection: StorageConnection }) {
  if (!connection.last_tested_at) {
    return <Outcome outcome="not_assessed" label="Not tested yet" />;
  }
  const failed = Boolean(connection.last_error_category);
  return (
    <div className="flex flex-col gap-2">
      <Outcome outcome={failed ? "failed" : "passed"} label={failed ? "Last test failed" : "Last test passed"} />
      <p className="text-sm text-muted">
        <Time value={connection.last_tested_at} />
      </p>
      {failed && connection.last_error_category && (
        <p className="text-base">
          <span className="mono-data font-semibold">{connection.last_error_category}</span>
          {": "}
          {CATEGORY_MESSAGES[connection.last_error_category] ?? ""}
        </p>
      )}
      {failed && connection.last_test_ok_at && (
        <p className="text-sm text-muted">
          Last passed <Time value={connection.last_test_ok_at} />
        </p>
      )}
    </div>
  );
}
