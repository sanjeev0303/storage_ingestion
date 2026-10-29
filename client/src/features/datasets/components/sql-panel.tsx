"use client";

import type { AsyncDuckDBConnection } from "@duckdb/duckdb-wasm";
import { useRef, useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Checkbox, Field, Select, Textarea } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { Table, Td, Th } from "@/components/ui/table";
import { api as callApi, apiRaw } from "@/lib/api/client";
import { describeError } from "@/lib/api/errors";
import { sha256Blob } from "@/lib/upload/sha256";
import { formatBytes, formatNumber } from "@/lib/utils/format";
import * as api from "../api";
import {
  cellText,
  fileKind,
  openDuckDB,
  registerRemote,
  sqlString,
  tableName,
  type FileKind,
} from "../sql/duckdb";
import type { VersionMember } from "../types";

// Files above this are not loaded into the browser.
const MAX_FILE_BYTES = 256 * 1024 * 1024;
// Loading many files as one table: at most this many bytes downloaded and
// checked, and at most this many files (more when Parquet is read in place).
const MAX_GROUP_BYTES = 1024 * 1024 * 1024;
const MAX_GROUP_FILES = 500;
const MAX_REMOTE_FILES = 2000;

type Group = "csv" | "parquet" | "json";
const GROUP_KINDS: Record<Group, FileKind[]> = { csv: ["csv", "tsv"], parquet: ["parquet"], json: ["json", "jsonl"] };
const GROUP_TEXT: Record<Group, string> = { csv: "CSV and TSV", parquet: "Parquet", json: "JSON and JSON Lines" };
// The members table holds at most this many members.
const MAX_MEMBERS = 10_000;
const SHOWN_ROWS = 500;

type Loaded = { name: string; path: string; rows: number };
type Result = {
  columns: string[];
  rows: unknown[][];
  total: number;
  ms: number;
};

// SQL over one snapshot, run by DuckDB in this browser. It reads only bytes
// the viewer can already download, each checked against the SHA-256 the
// snapshot pinned before it becomes a table.
export function SqlPanel({
  datasetId,
  versionId,
}: {
  datasetId: string;
  versionId: string;
}) {
  const conn = useRef<AsyncDuckDBConnection | null>(null);
  const [members, setMembers] = useState<VersionMember[]>([]);
  const [tables, setTables] = useState<Loaded[]>([]);
  const [chosen, setChosen] = useState("");
  const [sql, setSql] = useState(
    "SELECT split, label, count(*) AS files, sum(size_bytes) AS bytes\nFROM members\nGROUP BY ALL\nORDER BY files DESC",
  );
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Result | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [groupKind, setGroupKind] = useState<Group>("csv");
  const [groupSplit, setGroupSplit] = useState("");
  const [inPlace, setInPlace] = useState(false);

  async function start() {
    setBusy("Starting DuckDB and reading the members");
    setError(null);
    try {
      const db = await openDuckDB();
      const c = await db.connect();
      const all: VersionMember[] = [];
      let after: number | undefined;
      do {
        const page = await api.listVersionMembers(datasetId, versionId, after);
        all.push(...page.members);
        after = page.next_after || undefined;
      } while (after && all.length < MAX_MEMBERS);
      const rows = all.slice(0, MAX_MEMBERS).map((m) => ({
        path: m.path,
        label: m.label ?? null,
        split: m.split ?? null,
        size_bytes: m.size_bytes,
        content_type: m.content_type,
        checksum_sha256: m.checksum_sha256 ?? null,
        object_version: m.object_version,
        availability: m.availability,
        metadata: JSON.stringify(m.metadata ?? {}),
      }));
      await db.registerFileText("members.json", JSON.stringify(rows));
      await c.insertJSONFromPath("members.json", { name: "members" });
      conn.current = c;
      setMembers(all);
      setTruncated(Boolean(after));
      setTables([
        { name: "members", path: "the snapshot's members", rows: rows.length },
      ]);
    } catch (e) {
      setError(`DuckDB could not start: ${describeError(e)}`);
    } finally {
      setBusy(null);
    }
  }

  const loadable = members.filter(
    (m) =>
      m.availability === "AVAILABLE" &&
      fileKind(m.path, m.content_type) !== null,
  );

  async function load() {
    const member = loadable.find((m) => m.path === chosen);
    const c = conn.current;
    if (!member || !c) return;
    if (member.size_bytes > MAX_FILE_BYTES) {
      setError(
        `${member.path} is ${formatBytes(member.size_bytes)}; the browser loads files up to ${formatBytes(MAX_FILE_BYTES)}.`,
      );
      return;
    }
    setError(null);
    setBusy(`Downloading ${member.path}`);
    try {
      const bytes = await fetchChecked(member, setBusy);
      const db = await openDuckDB();
      const name = tableName(member.path, new Set(tables.map((t) => t.name)));
      const kind = fileKind(member.path, member.content_type) as FileKind;
      const file = `${name}.${kind}`;
      await db.registerFileBuffer(file, bytes);
      setBusy(`Reading ${member.path} into the table ${name}`);
      await c.query(
        `CREATE TABLE "${name}" AS SELECT * FROM ${reader(kind, file)}`,
      );
      const count = await c.query(`SELECT count(*) AS n FROM "${name}"`);
      const rows = Number(count.toArray()[0]?.n ?? 0);
      setTables((list) => [...list, { name, path: member.path, rows }]);
      setSql(`SELECT * FROM "${name}" LIMIT 100`);
      setChosen("");
    } catch (e) {
      setError(
        `${member.path} was not loaded: ${e instanceof Error ? e.message : describeError(e)}`,
      );
    } finally {
      setBusy(null);
    }
  }

  const groupFiles = loadable.filter(
    (m) =>
      GROUP_KINDS[groupKind].includes(fileKind(m.path, m.content_type) as FileKind) &&
      (groupSplit === "" || m.split === groupSplit),
  );

  // Every file of a kind (and split) becomes one table, with a _file column
  // naming the file each row came from. Downloaded and checked like a single
  // file, or, for Parquet, read in place by byte ranges.
  async function loadGroup() {
    const c = conn.current;
    if (!c || groupFiles.length === 0) return;
    const remote = inPlace && groupKind === "parquet";
    const bytes = groupFiles.reduce((sum, m) => sum + m.size_bytes, 0);
    if (!remote && (groupFiles.length > MAX_GROUP_FILES || bytes > MAX_GROUP_BYTES)) {
      const hint = groupKind === "parquet" ? " Read the Parquet files in place instead." : " Choose a split to load fewer.";
      setError(
        `${formatNumber(groupFiles.length)} files, ${formatBytes(bytes)}: the browser checks and loads up to ${MAX_GROUP_FILES} files and ${formatBytes(MAX_GROUP_BYTES)} at once.${hint}`,
      );
      return;
    }
    if (remote && groupFiles.length > MAX_REMOTE_FILES) {
      setError(`${formatNumber(groupFiles.length)} files: read in place up to ${MAX_REMOTE_FILES} at once. Choose a split.`);
      return;
    }
    setError(null);
    const db = await openDuckDB();
    const name = tableName(`${groupSplit || "all"}_${groupKind}`, new Set(tables.map((t) => t.name)));
    try {
      const selects: string[] = [];
      for (const [i, member] of groupFiles.entries()) {
        const kind = fileKind(member.path, member.content_type) as FileKind;
        const file = `${name}_${i}.${kind}`;
        if (remote) {
          setBusy(`Opening ${i + 1} of ${groupFiles.length}: ${member.path}`);
          const { url } = await callApi<{ url: string }>(`/assets/${encodeURIComponent(member.version_id)}/download-url`, {
            method: "POST",
            body: { mode: "storage" },
          });
          await registerRemote(db, file, url);
        } else {
          const step = (text: string) => setBusy(`${i + 1} of ${groupFiles.length}: ${text}`);
          await db.registerFileBuffer(file, await fetchChecked(member, step));
        }
        selects.push(`SELECT *, ${sqlString(member.path)} AS _file FROM ${reader(kind, file)}`);
      }
      setBusy(`Reading ${groupFiles.length} files into the table ${name}`);
      await c.query(`CREATE TABLE "${name}" AS ${selects.join(" UNION ALL BY NAME ")}`);
      const count = await c.query(`SELECT count(*) AS n FROM "${name}"`);
      const rows = Number(count.toArray()[0]?.n ?? 0);
      const what = `${formatNumber(groupFiles.length)} ${GROUP_TEXT[groupKind]} files${groupSplit ? ` of ${groupSplit}` : ""}`;
      setTables((list) => [...list, { name, path: remote ? `${what}, read in place (not hash-checked)` : what, rows }]);
      setSql(`SELECT _file, count(*) AS rows FROM "${name}" GROUP BY _file ORDER BY _file`);
    } catch (e) {
      setError(`The files were not loaded: ${e instanceof Error ? e.message : describeError(e)}`);
    } finally {
      setBusy(null);
    }
  }

  async function run(e: FormEvent) {
    e.preventDefault();
    const c = conn.current;
    if (!c || !sql.trim()) return;
    setBusy("Running the query");
    setError(null);
    try {
      const { value: table, ms } = await timed(() => c.query(sql));
      const columns = table.schema.fields.map((f) => f.name);
      const rows: unknown[][] = [];
      for (const row of table.toArray().slice(0, SHOWN_ROWS)) {
        const values = row.toJSON() as Record<string, unknown>;
        rows.push(columns.map((col) => values[col]));
      }
      setResult({
        columns,
        rows,
        total: table.numRows,
        ms,
      });
    } catch (err) {
      setResult(null);
      setError(err instanceof Error ? err.message : describeError(err));
    } finally {
      setBusy(null);
    }
  }

  return (
    <Panel
      title="Query this snapshot"
      description="SQL over the snapshot's members and any CSV, TSV, Parquet or JSON file in it, run by DuckDB in your browser. Nothing is sent to the server but the downloads."
    >
      {tables.length === 0 ? (
        <div className="flex flex-wrap items-center gap-4">
          <Button variant="primary" onClick={start} loading={busy !== null}>
            Start SQL
          </Button>
          <p className="text-sm text-muted">
            Downloads the DuckDB engine once (about 33 MB) and reads up to{" "}
            {formatNumber(MAX_MEMBERS)} members.
          </p>
        </div>
      ) : (
        <div className="flex flex-col gap-5">
          <div className="flex flex-col gap-2">
            <h3 className="text-sm font-semibold">Tables</h3>
            <ul className="flex flex-col gap-1 text-base">
              {tables.map((t) => (
                <li key={t.name}>
                  <span className="mono-data">{t.name}</span>{" "}
                  <span className="text-sm text-muted">
                    {formatNumber(t.rows)} rows from{" "}
                    {t.name === "members" ? (
                      t.path
                    ) : (
                      <span className="mono-data">{t.path}</span>
                    )}
                  </span>
                </li>
              ))}
            </ul>
            {truncated && (
              <p className="text-sm text-muted">
                The snapshot has more members; the table holds the first{" "}
                {formatNumber(MAX_MEMBERS)}.
              </p>
            )}
          </div>

          {loadable.length > 0 && (
            <div className="flex flex-wrap items-end gap-3">
              <Field label="Load a file as a table" className="min-w-64 flex-1">
                {(p) => (
                  <Select
                    {...p}
                    value={chosen}
                    onChange={(e) => setChosen(e.target.value)}
                  >
                    <option value="">Choose a file</option>
                    {loadable.slice(0, 500).map((m) => (
                      <option key={m.path} value={m.path}>
                        {m.path} ({formatBytes(m.size_bytes)})
                      </option>
                    ))}
                  </Select>
                )}
              </Field>
              <Button onClick={load} disabled={!chosen || busy !== null}>
                Load file
              </Button>
            </div>
          )}

          {loadable.length > 0 && (
            <fieldset className="flex flex-wrap items-end gap-3">
              <legend className="mb-2 text-sm font-semibold">Load many files as one table</legend>
              <Field label="Files" className="w-56">
                {(p) => (
                  <Select {...p} value={groupKind} onChange={(e) => setGroupKind(e.target.value as Group)}>
                    {(Object.keys(GROUP_TEXT) as Group[]).map((g) => (
                      <option key={g} value={g}>
                        {GROUP_TEXT[g]}
                      </option>
                    ))}
                  </Select>
                )}
              </Field>
              <Field label="Split" className="w-44">
                {(p) => (
                  <Select {...p} value={groupSplit} onChange={(e) => setGroupSplit(e.target.value)}>
                    <option value="">Every split</option>
                    <option value="train">train</option>
                    <option value="validation">validation</option>
                    <option value="test">test</option>
                  </Select>
                )}
              </Field>
              <Button onClick={loadGroup} disabled={groupFiles.length === 0 || busy !== null}>
                Load {formatNumber(groupFiles.length)} {groupFiles.length === 1 ? "file" : "files"}
              </Button>
              {groupKind === "parquet" && (
                <Checkbox
                  className="basis-full"
                  checked={inPlace}
                  onChange={(e) => setInPlace(e.target.checked)}
                  label="Read in place by byte ranges: only what the query needs is fetched, but the bytes are not checked against their SHA-256"
                />
              )}
            </fieldset>
          )}

          <form onSubmit={run} className="flex flex-col gap-3">
            <Field label="SQL">
              {(p) => (
                <Textarea
                  {...p}
                  rows={6}
                  value={sql}
                  onChange={(e) => setSql(e.target.value)}
                  spellCheck={false}
                  className="mono-data"
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                      e.preventDefault();
                      e.currentTarget.form?.requestSubmit();
                    }
                  }}
                />
              )}
            </Field>
            <div className="flex flex-wrap items-center gap-3">
              <Button
                type="submit"
                variant="primary"
                loading={busy === "Running the query"}
                disabled={busy !== null || !sql.trim()}
              >
                Run query
              </Button>
              <span className="text-sm text-muted">
                Ctrl or Cmd and Enter runs it too.
              </span>
            </div>
          </form>
        </div>
      )}

      {busy && (
        <p role="status" className="mt-4 text-sm">
          {busy}…
        </p>
      )}
      {error && (
        <p
          role="alert"
          className="mt-4 text-base font-medium [overflow-wrap:anywhere]"
        >
          {error}
        </p>
      )}

      {result && (
        <div className="mt-5 flex flex-col gap-2">
          <p className="text-sm text-muted">
            {formatNumber(result.total)} rows in {result.ms} ms
            {result.total > SHOWN_ROWS && `, the first ${SHOWN_ROWS} shown`}.
          </p>
          {result.columns.length > 0 && (
            <div className="max-h-[32rem] overflow-y-auto" data-lenis-prevent>
              <Table>
                <caption className="sr-only">Query result</caption>
                <thead>
                  <tr>
                    {result.columns.map((col) => (
                      <Th key={col} className="mono-data">
                        {col}
                      </Th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {result.rows.map((row, i) => (
                    <tr key={i}>
                      {row.map((value, j) => (
                        <Td
                          key={j}
                          className={
                            typeof value === "number" ||
                            typeof value === "bigint"
                              ? "text-right tabular-nums"
                              : "max-w-xs truncate"
                          }
                          title={value === null ? undefined : cellText(value)}
                        >
                          {value === null ? (
                            <span className="text-muted">null</span>
                          ) : (
                            cellText(value)
                          )}
                        </Td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </Table>
            </div>
          )}
        </div>
      )}
    </Panel>
  );
}

function reader(kind: FileKind, file: string) {
  switch (kind) {
    case "csv":
      return `read_csv_auto('${file}')`;
    case "tsv":
      return `read_csv_auto('${file}', delim = '\t')`;
    case "parquet":
      return `read_parquet('${file}')`;
    case "json":
    case "jsonl":
      return `read_json_auto('${file}')`;
  }
}

// A member's bytes, refused unless they hash to the SHA-256 the snapshot
// pinned.
async function fetchChecked(member: VersionMember, step: (text: string) => void) {
  step(`Downloading ${member.path}`);
  const res = await apiRaw(`/assets/${encodeURIComponent(member.version_id)}/content`, { timeoutMs: 10 * 60_000 });
  const blob = await res.blob();
  step(`Checking ${member.path} against its pinned SHA-256`);
  const digest = await sha256Blob(blob);
  if (member.checksum_sha256 && digest !== member.checksum_sha256) {
    throw new Error(`the downloaded bytes hash to ${digest.slice(0, 12)}, not the pinned ${member.checksum_sha256.slice(0, 12)}`);
  }
  return new Uint8Array(await blob.arrayBuffer());
}

// Runs work and says how long it took.
async function timed<T>(work: () => Promise<T>) {
  const began = performance.now();
  const value = await work();
  return { value, ms: Math.round(performance.now() - began) };
}
