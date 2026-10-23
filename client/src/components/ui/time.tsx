import { formatDateTime } from "@/lib/utils/format";

// Local time in the viewer's zone; hover shows the exact UTC instant.
export function Time({ value }: { value: string | undefined | null }) {
  if (!value) return <span className="text-muted">Not set</span>;
  return (
    <time dateTime={value} title={new Date(value).toISOString()} suppressHydrationWarning>
      {formatDateTime(value)}
    </time>
  );
}
