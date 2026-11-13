import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { ApiError, backoffMs, blobSource, Client, MemoryJournal, partSizeFor, Sha256, upload } from "../src/index.ts";
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

// A storage and API fake for the network tests: each storage PUT is answered
// by the next function in `puts`, then with success.
function flakyServer(puts: ((url: string, init?: RequestInit) => Promise<Response>)[]) {
  const calls: { method: string; url: string; body?: string }[] = [];
  const waits: number[] = [];
  let urlVersion = 0;
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    calls.push({ method, url, body: typeof init?.body === "string" ? init.body : undefined });
    const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
    if (url.startsWith("https://storage.test/")) {
      const next = puts.shift();
      if (next) return next(url, init);
      return new Response("", { status: 200, headers: { ETag: `"ok"` } });
    }
    if (url.endsWith("/api/v1/uploads") && method === "POST") {
      urlVersion++;
      return json(201, { upload_id: "single", upload_url: `https://storage.test/single-v${urlVersion}` });
    }
    if (url.endsWith("/parts/urls")) {
      urlVersion++;
      const { part_numbers } = JSON.parse(String(init?.body)) as { part_numbers: number[] };
      return json(200, { parts: part_numbers.map((n) => ({ part_number: n, url: `https://storage.test/part-${n}-v${urlVersion}` })) });
    }
    if (url.endsWith("/api/v1/uploads/multipart")) return json(201, { upload_id: "m1" });
    return json(200, {});
  }) as typeof fetch;
  const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: fetchFn, sleep: async (ms) => void waits.push(ms) });
  return { client, calls, waits };
}

const hang = (_url: string, init?: RequestInit) =>
  new Promise<Response>((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal?.reason)));

test("a storage request that stalls is stopped and sent again", async () => {
  const { client, calls } = flakyServer([hang]);
  await upload(client, new Blob(["tiny"]), { path: "a.txt", stallMs: 20 });
  assert.equal(calls.filter((c) => c.method === "PUT").length, 2);
});

test("an expired URL is replaced and the PUT goes again at once", async () => {
  const expired = async () => new Response("<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>", { status: 403 });
  const { client, calls, waits } = flakyServer([expired]);
  await upload(client, new Blob([new Uint8Array(3 * 1024 * 1024)]), { path: "big.bin", partSize: 1024 * 1024, concurrency: 1 });
  const puts = calls.filter((c) => c.method === "PUT").map((c) => c.url);
  assert.equal(puts.length, 4);
  assert.match(puts[0], /part-1-v1/);
  assert.match(puts[1], /part-1-v2/, "a fresh URL for the same part");
  const presigns = calls.filter((c) => c.url.endsWith("/parts/urls")).map((c) => c.body);
  assert.deepEqual(presigns, ['{"part_numbers":[1,2,3]}', '{"part_numbers":[1]}']);
  assert.deepEqual(waits, []);
});

test("Retry-After is respected and a refusal that will not change is not retried", async () => {
  const busy = async () => new Response("slow down", { status: 503, headers: { "Retry-After": "7" } });
  const first = flakyServer([busy]);
  await upload(first.client, new Blob(["tiny"]), { path: "a.txt" });
  assert.deepEqual(first.waits, [7000]);

  const refused = async () => new Response("bad", { status: 400 });
  const second = flakyServer([refused]);
  await assert.rejects(upload(second.client, new Blob(["tiny"]), { path: "a.txt" }), (e: ApiError) => e.status === 400);
  assert.equal(second.calls.filter((c) => c.method === "PUT").length, 1);
  assert.deepEqual(second.waits, []);
});

test("API answers that can change are retried with full jitter up to 30 seconds", async () => {
  let failures = 2;
  const waits: number[] = [];
  const client = new Client({
    baseUrl: "https://api.test",
    token: "t",
    sleep: async (ms) => void waits.push(ms),
    fetch: (async () => (failures-- > 0 ? new Response("", { status: 502 }) : new Response("{}", { status: 200 }))) as typeof fetch,
  });
  await client.request("GET", "/x");
  assert.equal(waits.length, 2);
  assert.ok(waits[0] >= 0 && waits[0] <= 1000 && waits[1] <= 2000);
  assert.equal(backoffMs(0, () => 0), 0);
  assert.equal(backoffMs(3, () => 0.5), 4000);
  assert.ok(backoffMs(20, () => 0.999) < 30_000);
});

test("a saved session the server says has ended is dropped and the upload starts again once", async () => {
  const data = new Uint8Array(3 * 1024 * 1024).fill(3);
  const journal = new MemoryJournal();
  const partSize = 1024 * 1024;
  const sha = realSha(data);
  await journal.set(`${sha}::big.bin`, { uploadId: "old", partSize, parts: { 1: '"e1"' } });
  const starts: string[] = [];
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (url.startsWith("https://storage.test/")) return new Response("", { status: 200, headers: { ETag: '"ok"' } });
    if (url.includes("/uploads/old/")) return json(409, { error: "multipart upload is not active" });
    if (url.endsWith("/api/v1/uploads/multipart")) {
      starts.push((JSON.parse(String(init?.body)) as { idempotency_key: string }).idempotency_key);
      return json(201, { upload_id: "new" });
    }
    if (url.endsWith("/parts/urls")) {
      const { part_numbers } = JSON.parse(String(init?.body)) as { part_numbers: number[] };
      return json(200, { parts: part_numbers.map((n) => ({ part_number: n, url: `https://storage.test/part-${n}` })) });
    }
    return json(200, {});
  }) as typeof fetch;
  const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: fetchFn });

  const r = await upload(client, blobSource(new Blob([data]), "big.bin"), { path: "big.bin", journal, partSize });

  assert.equal(r.uploadId, "new");
  assert.equal(starts.length, 1);
  assert.equal(await journal.get(`${sha}::big.bin`), undefined);
});

test("waiting for the checks stops at its limit and when the signal aborts", async () => {
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    if (url.startsWith("https://storage.test/")) return new Response("", { status: 200 });
    if (url.endsWith("/api/v1/uploads") && method === "POST") return Response.json({ upload_id: "s1", upload_url: "https://storage.test/s1" }, { status: 201 });
    if (method === "GET") return Response.json({ status: "VERIFIED", asset: { id: "a1", status: "PROCESSING" } });
    return Response.json({});
  }) as typeof fetch;
  const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: fetchFn });

  const r = await upload(client, new Blob(["tiny"]), { path: "a.txt", wait: true, pollMs: 5, waitTimeoutMs: 30 });
  assert.equal(r.status, "VERIFIED");

  const controller = new AbortController();
  const began = Date.now();
  setTimeout(() => controller.abort(), 20);
  await assert.rejects(upload(client, new Blob(["tiny"]), { path: "a.txt", wait: true, pollMs: 60_000, signal: controller.signal }));
  assert.ok(Date.now() - began < 5_000, "the wait between checks stops at once");
});

test("the first failed part stops the other workers", async () => {
  const puts: string[] = [];
  let failedAt = 0;
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url.startsWith("https://storage.test/")) {
      puts.push(url);
      if (url.endsWith("part-1")) {
        failedAt = puts.length;
        return new Response("bad", { status: 400 });
      }
      await new Promise((done) => setTimeout(done, 5));
      return new Response("", { status: 200, headers: { ETag: '"ok"' } });
    }
    if (url.endsWith("/api/v1/uploads/multipart")) return Response.json({ upload_id: "m1" }, { status: 201 });
    if (url.endsWith("/parts/urls")) {
      const { part_numbers } = JSON.parse(String(init?.body)) as { part_numbers: number[] };
      return Response.json({ parts: part_numbers.map((n) => ({ part_number: n, url: `https://storage.test/part-${n}` })) });
    }
    return Response.json({});
  }) as typeof fetch;
  const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: fetchFn });

  await assert.rejects(
    upload(client, new Blob([new Uint8Array(16 * 1024)]), { path: "big.bin", partSize: 1024, concurrency: 2, attempts: 1 }),
    (e: ApiError) => e.status === 400,
  );
  const atRejection = puts.length;
  await new Promise((done) => setTimeout(done, 50));

  assert.equal(puts.length, atRejection, "nothing is sent after upload() answers");
  assert.ok(atRejection <= failedAt + 1, `at most the part already in flight goes after the failure (${puts.length} sent)`);
});

test("a storage AccessDenied is a refusal, not an expired URL", async () => {
  const denied = async () => new Response("<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>", { status: 403 });
  const { client, calls } = flakyServer([denied]);
  await assert.rejects(upload(client, new Blob(["tiny"]), { path: "a.txt" }), (e: ApiError) => e.status === 403);
  assert.equal(calls.filter((c) => c.method === "PUT").length, 1);
  assert.equal(calls.filter((c) => c.url.endsWith("/api/v1/uploads")).length, 1, "no new URL was asked for");
});

test("a start that hands back an ended session is tried once more under a new key", async () => {
  const starts: string[] = [];
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url.startsWith("https://storage.test/")) return new Response("", { status: 200, headers: { ETag: '"ok"' } });
    if (url.includes("/uploads/old/")) return Response.json({ error: "multipart upload is not active" }, { status: 409 });
    if (url.endsWith("/api/v1/uploads/multipart")) {
      starts.push((JSON.parse(String(init?.body)) as { idempotency_key: string }).idempotency_key);
      return Response.json({ upload_id: starts.length === 1 ? "old" : "new" }, { status: 201 });
    }
    if (url.endsWith("/parts/urls")) {
      const { part_numbers } = JSON.parse(String(init?.body)) as { part_numbers: number[] };
      return Response.json({ parts: part_numbers.map((n) => ({ part_number: n, url: `https://storage.test/part-${n}` })) });
    }
    return Response.json({});
  }) as typeof fetch;
  const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: fetchFn });

  const r = await upload(client, new Blob([new Uint8Array(4096)]), { path: "big.bin", partSize: 1024 });

  assert.equal(r.uploadId, "new");
  assert.equal(starts.length, 2);
  assert.notEqual(starts[0], starts[1]);
});

test("a single upload whose replayed start has ended starts once more under a new key", async () => {
  const starts: string[] = [];
  const completed: string[] = [];
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url.startsWith("https://storage.test/")) return new Response("", { status: 200, headers: { ETag: '"ok"' } });
    if (url.endsWith("/api/v1/uploads")) {
      starts.push((JSON.parse(String(init?.body)) as { idempotency_key: string }).idempotency_key);
      if (starts.length === 1) {
        const error = "upload session is not active: it has ended; start again with a new idempotency key";
        return Response.json({ code: "session_ended", error }, { status: 409 });
      }
      return Response.json({ upload_id: "new", status: "PENDING", upload_url: "https://storage.test/new" }, { status: 201 });
    }
    if (url.endsWith("/complete")) completed.push(url);
    return Response.json({});
  }) as typeof fetch;
  const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: fetchFn });

  const r = await upload(client, new Blob(["tiny"]), { path: "a.txt" });

  assert.equal(r.uploadId, "new");
  assert.equal(starts.length, 2);
  assert.notEqual(starts[0], starts[1]);
  assert.deepEqual(completed, ["https://api.test/api/v1/uploads/new/complete"]);
});

test("a single upload whose replayed start already completed is not sent again", async () => {
  const calls: string[] = [];
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    calls.push(`${init?.method ?? "GET"} ${url}`);
    if (url.endsWith("/api/v1/uploads")) return Response.json({ upload_id: "done", status: "COMPLETED", upload_url: "" });
    return Response.json({ error: "unexpected" }, { status: 400 });
  }) as typeof fetch;
  const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: fetchFn });

  const r = await upload(client, new Blob(["tiny"]), { path: "a.txt" });

  assert.equal(r.uploadId, "done");
  assert.deepEqual(calls, ["POST https://api.test/api/v1/uploads"]);
});

test("waiting stops at a blocked or deleted asset", async () => {
  for (const state of ["BLOCKED", "DELETED", "SOFT_DELETED"]) {
    const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url.startsWith("https://storage.test/")) return new Response("", { status: 200 });
      if (url.endsWith("/api/v1/uploads") && method === "POST") return Response.json({ upload_id: "s1", upload_url: "https://storage.test/s1" }, { status: 201 });
      if (method === "GET") return Response.json({ status: "VERIFIED", asset: { id: "a1", status: state } });
      return Response.json({});
    }) as typeof fetch;
    const client = new Client({ baseUrl: "https://api.test", token: "t", fetch: fetchFn });

    const r = await upload(client, new Blob(["tiny"]), { path: "a.txt", wait: true, pollMs: 5, waitTimeoutMs: 200 });

    assert.equal(r.status, state);
  }
});

test("Node: a file journal keeps every entry written at once", async () => {
  const journal = fileJournal(join(mkdtempSync(join(tmpdir(), "osp-")), "journal.json"));
  const keys = Array.from({ length: 20 }, (_, i) => `k${i}`);
  await Promise.all(keys.map((key) => journal.set(key, { uploadId: key, partSize: 1, parts: {} })));
  for (const key of keys) assert.equal((await journal.get(key))?.uploadId, key);
});
