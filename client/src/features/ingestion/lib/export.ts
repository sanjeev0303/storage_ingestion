import type { EntriesPage, IngestionEntry } from "../types";

export const EXPORT_CAP = 50_000;
const COLUMNS = ["position", "path", "state", "error_code", "error_detail", "attempts", "size_bytes", "updated_at"] as const;

// Cells starting with = + - @ would run as formulas when the CSV is opened in a spreadsheet.
export function csvCell(value: unknown) {
  let text = value === undefined || value === null ? "" : String(value);
  if (/^[=+\-@\t\r]/.test(text)) text = `'${text}`;
  return /[",\r\n]/.test(text) ? `"${text.replace(/"/g, '""')}"` : text;
}

type FetchPage = (after: number | undefined, limit: number) => Promise<EntriesPage>;

// The API has no error report download, so the browser pages through /failures
// and builds the CSV itself. Only the text chunks are kept, not the entries.
export async function exportFailuresCsv(
  fetchPage: FetchPage,
  options: { cap?: number; signal?: AbortSignal; onProgress?: (rows: number) => void } = {},
) {
  const { cap = EXPORT_CAP, signal, onProgress } = options;
  const chunks: string[] = [`${COLUMNS.join(",")}\n`];
  let rows = 0;
  let after: number | undefined;
  let capped = false;

  while (!capped) {
    if (signal?.aborted) throw new DOMException("The operation was aborted.", "AbortError");
    const page = await fetchPage(after, 1000);
    const entries: IngestionEntry[] = page.entries ?? [];
    let text = "";
    for (const entry of entries) {
      if (rows >= cap) {
        capped = true;
        break;
      }
      text += `${COLUMNS.map((column) => csvCell(entry[column])).join(",")}\n`;
      rows++;
    }
    chunks.push(text);
    onProgress?.(rows);
    if (page.next_after === undefined || entries.length === 0) break;
    after = page.next_after;
  }

  return { blob: new Blob(chunks, { type: "text/csv;charset=utf-8" }), rows, capped };
}
