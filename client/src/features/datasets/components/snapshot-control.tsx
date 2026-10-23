"use client";

import Link from "next/link";
import { useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { Modal } from "@/components/ui/dialog";
import { newIdempotencyKey } from "@/lib/api/client";
import { useSession } from "@/lib/auth/session";
import { formatNumber } from "@/lib/utils/format";
import { snapshotMessage } from "../helpers";
import { useCreateSnapshot } from "../hooks";
import type { Dataset } from "../types";

export function SnapshotControl({ dataset }: { dataset: Dataset }) {
  const { can } = useSession();
  const [open, setOpen] = useState(false);
  // One key per attempt: a retry after a timeout reuses it, so a snapshot that did get made is not made twice.
  const key = useRef(newIdempotencyKey());
  const snapshot = useCreateSnapshot(dataset.id);

  const archived = dataset.lifecycle_status === "ARCHIVED";
  const disabled = archived || !can("datasets.write");

  function start() {
    snapshot.mutate(key.current, {
      onSuccess: () => {
        key.current = newIdempotencyKey();
      },
    });
  }

  function close(next: boolean) {
    if (snapshot.isPending) return;
    setOpen(next);
    if (!next) snapshot.reset();
  }

  const done = snapshot.data;

  return (
    <>
      <Button
        variant="primary"
        disabled={disabled}
        onClick={() => setOpen(true)}
        title={archived ? "Archived datasets are read-only" : !can("datasets.write") ? "Needs datasets.write" : undefined}
      >
        Create snapshot
      </Button>

      <Modal
        open={open}
        onOpenChange={close}
        title="Create snapshot"
        description="A snapshot freezes the current draft into a new immutable version."
        footer={
          done ? (
            <Button variant="primary" onClick={() => close(false)}>
              Done
            </Button>
          ) : (
            <>
              <Button onClick={() => close(false)} disabled={snapshot.isPending}>
                Cancel
              </Button>
              <Button variant="primary" onClick={start} loading={snapshot.isPending}>
                {snapshot.isError ? "Try again" : "Create snapshot"}
              </Button>
            </>
          )
        }
      >
        <div className="flex flex-col gap-3 text-base">
          {!done && (
            <p>
              The draft has {formatNumber(dataset.member_count)} {dataset.member_count === 1 ? "member" : "members"}.
              Each is pinned to the object version it has now, or to its current version if it follows one. Existing
              versions are not changed. A dataset that was Ready or Failed goes back to Draft.
            </p>
          )}
          {snapshot.isPending && (
            <p role="status" className="neo-in-sm p-4">
              Creating the snapshot. The server does this in one step, so for a very large dataset it can take
              several minutes. Keep this page open; nothing is created until it finishes.
            </p>
          )}
          {snapshot.isError && (
            <p role="alert" className="font-medium">
              {snapshotMessage(snapshot.error)}
            </p>
          )}
          {done && (
            <div role="status" className="neo-in-sm p-4">
              <p className="font-semibold">
                {done.created ? `Version ${done.version.version} created` : `Version ${done.version.version} already existed`}
              </p>
              <p className="mt-1 text-muted">
                {done.created
                  ? `${formatNumber(done.version.member_count)} members pinned. This version is immutable.`
                  : "This attempt had already produced a version, so the same one is returned and nothing new was made."}
              </p>
              <Link
                href={`/app/datasets/${dataset.id}/versions/${done.version.id}`}
                className="mt-2 inline-block font-medium underline underline-offset-2"
              >
                Open version {done.version.version}
              </Link>
            </div>
          )}
        </div>
      </Modal>
    </>
  );
}
