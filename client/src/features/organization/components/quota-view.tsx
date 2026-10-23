import { Meter } from "@/components/ui/page";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import type { Quota } from "../types";

// Usage the API reports, paired with its limit. Everything else has a limit only.
export function QuotaMeters({ quota }: { quota: Quota }) {
  const { limits, usage } = quota;
  return (
    <div className="flex flex-col gap-4">
      <Meter label="Stored bytes" used={usage.stored_bytes} limit={limits.max_storage_bytes} format={formatBytes} />
      <Meter label="Stored objects" used={usage.asset_count} limit={limits.max_assets} format={formatNumber} />
      <Meter label="Active uploads" used={usage.active_uploads} limit={limits.max_active_uploads} format={formatNumber} />
      <Meter
        label="Active multipart uploads"
        used={usage.active_multipart}
        limit={limits.max_active_multipart}
        format={formatNumber}
      />
      <Meter
        label="Pending processing"
        used={usage.pending_processing}
        limit={limits.max_pending_processing}
        format={formatNumber}
      />
    </div>
  );
}
