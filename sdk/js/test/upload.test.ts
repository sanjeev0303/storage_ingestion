import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { ApiError, blobSource, Client, MemoryJournal, partSizeFor, Sha256, upload } from "../src/index.ts";
import { fileJournal, fileSource } from "../src/node.ts";

function realSha(data: Uint8Array) {
  return createHash("sha256").update(data).digest("hex");
}

test("incremental SHA-256 matches, however the data is cut", () => {
  const data = new Uint8Array(200_001).map((_, i) => (i * 31) & 0xff);
  for (const cut of [1, 3, 55, 56, 63, 64, 65, 1000, 200_001]) {
    const h = new Sha256();
    for (let at = 0; at < data.length; at += cut) h.update(data.subarray(at, at + cut));
    assert.equal(h.hex(), realSha(data), `cut ${cut}`);
  }
  assert.equal(new Sha256().hex(), realSha(new Uint8Array()));
});

test("part size grows only when a file needs more than 10,000 parts", () => {
  assert.equal(partSizeFor(1024), 8 * 1024 * 1024);
  assert.equal(partSizeFor(200 * 1024 * 1024 * 1024), Math.ceil((200 * 1024 * 1024 * 1024) / 10000));
});

// A fake API and storage, failing on purpose where a test asks.
function fakeServer(options: { failPutAfter?: number } = {}) {
  const state = {
    puts: [] as string[],
    completed: undefined as undefined | { parts: { part_number: number; etag: string }[]; checksum_sha256: string },
    multipartStarts: 0,
    singleBodies: [] as number[],
    transient: 0,
  };
  let puts = 0;
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (url.startsWith("https://storage.test/")) {
      if (options.failPutAfter !== undefined && puts >= options.failPutAfter) throw new TypeError("network down");
      puts++;
      if (url.includes("flaky") && state.transient++ === 0) return new Response("", { status: 503 });
      state.puts.push(url);
      state.singleBodies.push((init?.body as Uint8Array).length);
      return new Response("", { status: 200, headers: { ETag: `"etag-${puts}"` } });
    }
    if (url.endsWith("/api/v1/uploads") && method === "POST") {
      return json(201, { upload_id: "single", upload_url: "https://storage.test/flaky-single", upload_headers: { "x-ms-blob-type": "BlockBlob" } });
    }
    if (url.endsWith("/api/v1/uploads/multipart")) {
      state.multipartStarts++;
      return json(201, { upload_id: "m1" });
    }
    if (url.endsWith("/parts/urls")) {
      const { part_numbers } = JSON.parse(String(init?.body)) as { part_numbers: number[] };
      return json(200, { parts: part_numbers.map((n) => ({ part_number: n, url: `https://storage.test/part-${n}` })) });
    }
    if (url.endsWith("/parts/confirm")) return json(200, {});
    if (url.endsWith("/complete")) {
      if (init?.body) state.completed = JSON.parse(String(init.body));
      return json(200, { status: "COMPLETING" });
    }
    if (url.includes("/api/v1/uploads/") && method === "GET") return json(200, { status: "COMPLETED", asset: { id: "a1", status: "AVAILABLE" } });
    return json(404, { error: "no route", code: "not_found" });
  }) as typeof fetch;
  return { state, client: new Client({ baseUrl: "https://api.test/", token: "t", fetch: fetchFn }) };
}

test("a small file goes in one request, retrying a storage hiccup", async () => {
  const { state, client } = fakeServer();
  const data = new TextEncoder().encode("hello world");
  const r = await upload(client, new Blob([data], { type: "text/plain" }), { dataset: "d", path: "a.txt", wait: true, pollMs: 1 });
  assert.equal(r.sha256, realSha(data));
  assert.equal(r.status, "AVAILABLE");
  assert.equal(state.puts.length, 1);
  assert.equal(state.transient, 2);
});

test("an interrupted multipart upload continues without resending parts", async () => {
  const data = new Uint8Array(1024 * 1024 * 5).fill(7);
  const journal = new MemoryJournal();
  const partSize = 1024 * 1024;
  const first = fakeServer({ failPutAfter: 2 });
  await assert.rejects(upload(first.client, blobSource(new Blob([data]), "big.bin"), { path: "big.bin", journal, partSize, concurrency: 1, attempts: 1 }));
  assert.equal(first.state.puts.length, 2);

  const second = fakeServer();
  const r = await upload(second.client, blobSource(new Blob([data]), "big.bin"), { path: "big.bin", journal, partSize, concurrency: 2 });
  assert.equal(second.state.multipartStarts, 0, "the same upload goes on");
  assert.equal(second.state.puts.length, 3, "only the parts not sent");
  assert.equal(second.state.completed?.parts.length, 5);
  assert.deepEqual(second.state.completed?.parts.map((p) => p.part_number), [1, 2, 3, 4, 5]);
  assert.equal(second.state.completed?.checksum_sha256, r.sha256);
});

test("a refused request says why", async () => {
  const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: (async () => new Response(JSON.stringify({ error: "no", code: "forbidden" }), { status: 403 })) as typeof fetch });
  await assert.rejects(client.request("GET", "/x"), (e: ApiError) => e.status === 403 && e.code === "forbidden");
});

test("Node: a file on disk, and a journal that survives a restart", async () => {
  const dir = mkdtempSync(join(tmpdir(), "osp-js-"));
  const path = join(dir, "f.bin");
  const data = new Uint8Array(300_000).map((_, i) => i & 0xff);
  writeFileSync(path, data);
  const source = await fileSource(path);
  assert.equal(source.size, data.length);
  assert.deepEqual(await source.read(10, 13), data.subarray(10, 13));
  const journal = fileJournal(join(dir, "journal.json"));
  await journal.set("k", { uploadId: "u", partSize: 1, parts: { 1: "e" } });
  const again = fileJournal(join(dir, "journal.json"));
  assert.equal((await again.get("k"))?.uploadId, "u");
  await again.remove("k");
  assert.equal(await journal.get("k"), undefined);
});
