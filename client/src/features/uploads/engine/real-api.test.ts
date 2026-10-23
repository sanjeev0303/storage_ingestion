import { File as NodeFile } from "node:buffer";
import { randomBytes } from "node:crypto";
import { http, passthrough } from "msw";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { setActiveOrganization } from "@/lib/api/client";
import { MIB } from "@/lib/upload/parts";
import { UploadHttpError } from "@/lib/upload/put";
import { server } from "@/test/server";
import { FileUpload } from "./file-upload";
import { memoryStore } from "./store";

// Runs the engine against the real stack. Skipped unless REAL_API=1.
//   REAL_API=1 REAL_EMAIL=... REAL_PASSWORD=... REAL_ORG=... npx vitest run src/features/uploads/engine/real-api
const enabled = Boolean(process.env.REAL_API);
const ORIGIN = "http://localhost:3100";

describe.skipIf(!enabled)("engine against the real API and MinIO", () => {
  const realFetch = globalThis.fetch;
  const exposed: string[] = [];

  beforeAll(async () => {
    server.use(http.all("*", () => passthrough()));

    const login = await realFetch(`${ORIGIN}/api/session/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: ORIGIN },
      body: JSON.stringify({ email: process.env.REAL_EMAIL, password: process.env.REAL_PASSWORD }),
    });
    expect(login.status).toBe(200);
    const cookie = login.headers
      .getSetCookie()
      .map((c) => c.split(";")[0])
      .join("; ");

    globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
      const headers = new Headers(init?.headers);
      headers.set("Cookie", cookie);
      headers.set("Origin", ORIGIN);
      return realFetch(typeof input === "string" && input.startsWith("/") ? new URL(input, ORIGIN) : input, { ...init, headers });
    }) as typeof fetch;
    setActiveOrganization(process.env.REAL_ORG ?? null);
  });

  afterAll(() => {
    globalThis.fetch = realFetch;
  });

  // XMLHttpRequest does not exist in Node, so this stands in for putBlob. It also
  // records what the storage server tells a browser at our origin about ETag.
  async function put(url: string, body: Blob, options: { headers?: Record<string, string>; onProgress?: (n: number) => void } = {}) {
    const res = await realFetch(url, { method: "PUT", body, headers: { ...options.headers, Origin: ORIGIN } });
    if (!res.ok) throw new UploadHttpError(res.status, (await res.text()).slice(0, 300));
    exposed.push(`${res.headers.get("access-control-allow-origin")} | ${res.headers.get("access-control-expose-headers")}`);
    options.onProgress?.(body.size);
    return { status: res.status, etag: res.headers.get("etag") };
  }

  async function uploadOf(size: number, name: string) {
    const file = new NodeFile([randomBytes(size)], name, { type: "application/octet-stream" }) as unknown as File;
    const upload = new FileUpload(
      file,
      { file_name: name, content_type: "application/octet-stream", path: `engine-check/${name}` },
      { put: put as never, store: memoryStore(), pollMs: 1000, maxPolls: 90 },
    );
    await upload.run();
    return upload.getSnapshot();
  }

  it("uploads 1 MiB in a single request through to AVAILABLE", async () => {
    const snap = await uploadOf(1 * MIB, `single-${Date.now()}.bin`);
    console.log("single", JSON.stringify({ state: snap.state, mode: snap.mode, asset: snap.asset, failure: snap.failure }));
    expect(snap.failure).toBeUndefined();
    expect(snap.mode).toBe("single");
    expect(snap.state).toBe("available");
  }, 180_000);

  it("uploads 25 MiB in parts through to AVAILABLE", async () => {
    const snap = await uploadOf(25 * MIB, `multi-${Date.now()}.bin`);
    console.log("multipart", JSON.stringify({ state: snap.state, mode: snap.mode, parts: snap.partsTotal, asset: snap.asset, failure: snap.failure }));
    console.log("CORS on part PUT (allow-origin | expose-headers):", exposed);
    expect(snap.failure).toBeUndefined();
    expect(snap.mode).toBe("multipart");
    expect(snap.state).toBe("available");
  }, 300_000);
});
