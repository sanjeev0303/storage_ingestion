"use client";

import { useRouter } from "next/navigation";
import { useRef } from "react";
import { Button } from "@/components/ui/button";
import { notify } from "@/components/ui/toast";
import { newIdempotencyKey } from "@/lib/api/client";
import { useSession } from "@/lib/auth/session";
import { useStartJob } from "@/features/validation/hooks";

// Reads every pinned file back from storage and hashes it again: catches
// bytes changed or lost after the snapshot, in the platform's buckets or
// in a customer's own.
export function StorageAuditButton({ datasetId, versionId }: { datasetId: string; versionId: string }) {
  const { can } = useSession();
  const router = useRouter();
  const start = useStartJob(datasetId, "validation");
  // One key per attempt, so a retried click never starts two audits.
  const key = useRef(newIdempotencyKey());

  if (!can("datasets.write")) return null;

  async function audit() {
    try {
      const { job } = await start.mutateAsync({
        version_id: versionId,
        profile_id: "builtin:storage-audit",
        mode: "FULL",
        idempotency_key: key.current,
      });
      key.current = newIdempotencyKey();
      router.push(`/app/validation/jobs/${job.id}`);
    } catch (error) {
      notify.error(error);
    }
  }

  return (
    <Button onClick={audit} loading={start.isPending} title="Re-read every file from storage and check its SHA-256">
      Audit stored bytes
    </Button>
  );
}
