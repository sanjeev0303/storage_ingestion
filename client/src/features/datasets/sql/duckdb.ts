"use client";

import type { AsyncDuckDB } from "@duckdb/duckdb-wasm";

// One DuckDB per tab, started the first time someone opens the SQL panel.
// The engine and its workers come from this origin (scripts/copy-duckdb.mjs),
// never from a CDN.
let starting: Promise<AsyncDuckDB> | null = null;

export function openDuckDB(): Promise<AsyncDuckDB> {
  if (!starting) {
    starting = start().catch((error) => {
      starting = null; // a later attempt may succeed
      throw error;
    });
  }
  return starting;
}

async function start() {
  const duckdb = await import("@duckdb/duckdb-wasm");
  const bundle = await duckdb.selectBundle({
    mvp: { mainModule: "/duckdb/duckdb-mvp.wasm", mainWorker: "/duckdb/duckdb-browser-mvp.worker.js" },
    eh: { mainModule: "/duckdb/duckdb-eh.wasm", mainWorker: "/duckdb/duckdb-browser-eh.worker.js" },
  });
  const worker = new Worker(bundle.mainWorker!);
  const db = new duckdb.AsyncDuckDB(new duckdb.VoidLogger(), worker);
  await db.instantiate(bundle.mainModule, bundle.pthreadWorker);
  // sum() of integers is a HUGEINT, which Arrow hands over as a decimal:
  // shown as a number, like other big integers.
  await db.open({ query: { castBigIntToDouble: true, castDecimalToDouble: true, castTimestampToDate: true } });
  return db;
}

// Lets DuckDB read a remote file by HTTP byte ranges, fetching only the
// parts a query needs (Parquet footers, the row groups and columns asked for).
export async function registerRemote(db: AsyncDuckDB, name: string, url: string) {
  const duckdb = await import("@duckdb/duckdb-wasm");
  await db.registerFileURL(name, url, duckdb.DuckDBDataProtocol.HTTP, false);
}

// A text value as a SQL string literal.
export function sqlString(value: string) {
  return `'${value.replaceAll("'", "''")}'`;
}

// A file name made into a table name people can type.
export function tableName(path: string, taken: Set<string>) {
  const base = (path.split("/").pop() ?? "data").replace(/\.[^.]*$/, "");
  let name = base.toLowerCase().replace(/[^a-z0-9_]+/g, "_").replace(/^_+|_+$/g, "") || "data";
  if (/^[0-9]/.test(name)) name = `t_${name}`;
  let unique = name;
  for (let n = 2; taken.has(unique); n++) unique = `${name}_${n}`;
  return unique;
}

export type FileKind = "csv" | "tsv" | "parquet" | "json" | "jsonl";

export function fileKind(path: string, contentType: string): FileKind | null {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  if (ext === "csv" || contentType.includes("csv")) return "csv";
  if (ext === "tsv" || contentType.includes("tab-separated")) return "tsv";
  if (ext === "parquet") return "parquet";
  if (ext === "jsonl" || ext === "ndjson" || contentType.includes("ndjson")) return "jsonl";
  if (ext === "json" || contentType === "application/json") return "json";
  return null;
}

// Shown the way people read it: big integers and dates as text, objects as JSON.
export function cellText(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "bigint") return value.toString();
  if (value instanceof Date) return value.toISOString();
  // Arrow's 128-bit numbers are typed arrays whose toString is the number.
  if (ArrayBuffer.isView(value)) return String(value);
  if (typeof value === "object") return JSON.stringify(value, (_, v) => (typeof v === "bigint" ? v.toString() : v));
  return String(value);
}
