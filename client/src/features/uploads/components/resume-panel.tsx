"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Panel } from "@/components/ui/panel";
import { Time } from "@/components/ui/time";
import { api } from "@/lib/api/client";
import { formatBytes } from "@/lib/utils/format";
import type { ResumeStore } from "../engine/store";
import type { ResumeRecord } from "../types";

type Props = {
  store: ResumeStore;
  organizationId: string;
  // Upload ids that are running on this page, so they are not listed twice.
  runningIds: string[];
  onResume: (file: File, record: ResumeRecord) => void;
};

export function ResumePanel({ store, organizationId, runningIds, onResume }: Props) {
  const [records, setRecords] = useState<ResumeRecord[]>([]);
  const [mismatch, setMismatch] = useState<string | null>(null);
  const [discarding, setDiscarding] = useState<ResumeRecord | null>(null);
  const picking = useRef<ResumeRecord | null>(null);
  const input = useRef<HTMLInputElement>(null);

  const refresh = useCallback(() => {
    store.list(organizationId).then(setRecords);
  }, [store, organizationId]);

  const running = runningIds.join(",");
  useEffect(refresh, [refresh, running]);

  const shown = records.filter((record) => !runningIds.includes(record.upload_id));
  if (shown.length === 0) return null;

  function choose(file: File | undefined) {
    const record = picking.current;
    if (!file || !record) return;

    const same =
      file.name === record.request.file_name && file.size === record.file_size && file.lastModified === record.file_last_modified;
    if (!same) {
      setMismatch(record.upload_id);
      return;
    }
    setMismatch(null);
    onResume(file, record);
    setRecords((current) => current.filter((r) => r.upload_id !== record.upload_id));
  }

  async function discard(record: ResumeRecord) {
    // A 409 means the upload already finished or closed; either way the saved copy is not needed.
    await api(`/uploads/${record.upload_id}/abort`, { method: "POST" }).catch(() => undefined);
    await store.remove(record.upload_id);
    refresh();
  }

  return (
    <Panel
      title="Unfinished uploads"
      description="Large uploads that stopped before they finished. Choose the same file again to continue."
    >
      <ul className="flex flex-col gap-4">
        {shown.map((record) => (
          <li key={record.upload_id} className="neo-in-sm p-3">
            <p className="font-medium [overflow-wrap:anywhere]">{record.request.file_name}</p>
            <p className="text-sm text-muted">
              {formatBytes(record.file_size)}, {record.parts.length} parts confirmed. Session ends <Time value={record.expires_at} />
            </p>
            {mismatch === record.upload_id && (
              <p role="alert" className="mt-1 text-sm font-medium">
                That is not the same file. The name, size and last modified date must match the original.
              </p>
            )}
            <div className="mt-2 flex flex-wrap gap-2">
              <Button
                onClick={() => {
                  picking.current = record;
                  input.current?.click();
                }}
              >
                Choose file to resume
              </Button>
              <Button onClick={() => setDiscarding(record)}>Discard</Button>
            </div>
          </li>
        ))}
      </ul>
      <p className="mt-3 text-sm text-muted">
        The server keeps no list of uploaded parts, so only parts this browser confirmed are skipped. Sessions last about 30
        minutes; after that the upload starts again from the beginning.
      </p>
      <input ref={input} hidden type="file" onChange={(event) => { choose(event.target.files?.[0]); event.target.value = ""; }} />

      <ConfirmDialog
        open={discarding !== null}
        onOpenChange={(next) => !next && setDiscarding(null)}
        title="Discard this unfinished upload?"
        description={`The server session for ${discarding?.request.file_name ?? "this file"} is closed and the parts already sent are deleted. The file itself is not touched.`}
        confirmLabel="Discard upload"
        onConfirm={() => discarding && discard(discarding)}
      />
    </Panel>
  );
}
