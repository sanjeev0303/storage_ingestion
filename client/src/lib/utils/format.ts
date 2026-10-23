const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

export function formatBytes(bytes: number | undefined | null) {
  if (bytes === undefined || bytes === null || !Number.isFinite(bytes)) return "Unavailable";
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = unit === 0 || value >= 100 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(digits)} ${BYTE_UNITS[unit]}`;
}

const numberFormat = new Intl.NumberFormat();
export function formatNumber(value: number | undefined | null) {
  if (value === undefined || value === null) return "Unavailable";
  return numberFormat.format(value);
}

// Shown in the viewer's timezone, with the zone named so the time is not
// mistaken for UTC. The ISO string stays available in the element's title.
const dateTimeFormat = new Intl.DateTimeFormat(undefined, {
  year: "numeric",
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
  timeZoneName: "short",
});

export function formatDateTime(iso: string | undefined | null) {
  if (!iso) return "Not set";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return dateTimeFormat.format(date);
}

export function formatRelative(iso: string | undefined | null, now = Date.now()) {
  if (!iso) return "Not set";
  const seconds = Math.round((Date.parse(iso) - now) / 1000);
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  const steps: [Intl.RelativeTimeFormatUnit, number][] = [
    ["day", 86400],
    ["hour", 3600],
    ["minute", 60],
  ];
  for (const [unit, size] of steps) {
    if (Math.abs(seconds) >= size) return rtf.format(Math.round(seconds / size), unit);
  }
  return rtf.format(seconds, "second");
}

export function shortId(id: string | undefined | null, length = 8) {
  if (!id) return "";
  return id.length <= length ? id : id.slice(0, length);
}
