import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeDataset } from "../fixtures";
import { preflightManifest } from "../helpers";
import type { ManifestImport } from "../types";
import { ImportJob } from "./import-job";
import { ImportsScreen } from "./imports-screen";

const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
  usePathname: () => "/app/datasets/ds-1/imports",
}));

// jsdom files cannot stream, so the tests give them the stream a browser would.
function manifestFile(text: string, size?: number) {
  const file = new File([text], "manifest.jsonl", { type: "application/x-ndjson" });
  Object.defineProperty(file, "stream", { value: () => new Response(text).body });
  if (size) Object.defineProperty(file, "size", { value: size });
  return file;
}

function job(over: Partial<ManifestImport> = {}): ManifestImport {
  return {
    id: "job-1",
    dataset_id: "ds-1",
    state: "QUEUED",
    source: "INLINE",
    duplicate_policy: "REJECT",
    manifest_bytes: 100,
    progress: { lines_read: 0, members_added: 0, members_updated: 0, members_skipped: 0, lines_rejected: 0 },
    attempts: 0,
    cancel_requested: false,
    created_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const goodLine = '{"path":"pets/a.txt","label":"cat"}';

describe("preflightManifest", () => {
  it("counts lines and samples problems without keeping the whole file", async () => {
    const lines = [goodLine, "", "{broken", '{"object_id":"o","path":"p"}', '{"path":"a","tenant_id":"t"}', '{"path":"a","schema_version":2}', "[1]"];
    const result = await preflightManifest(manifestFile(lines.join("\n")) as Blob);
    expect(result.lines).toBe(6);
    expect(result.invalid).toBe(5);
    expect(result.samples.map((s) => s.line)).toEqual([3, 4, 5, 6, 7]);
    expect(result.samples[0].problem).toBe("Not valid JSON.");
    expect(result.samples[2].problem).toContain("tenant_id");
  });

  it("keeps at most 50 samples but counts every invalid line", async () => {
    const text = Array.from({ length: 120 }, () => "nope").join("\n");
    const result = await preflightManifest(manifestFile(text) as Blob);
    expect(result.invalid).toBe(120);
    expect(result.samples).toHaveLength(50);
  });
});

describe("ImportsScreen", () => {
  function setup() {
    server.use(
      http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset())),
      http.get(`${API}/datasets/ds-1/manifests`, () => HttpResponse.json({ imports: [] })),
    );
    return renderWithProviders(<ImportsScreen id="ds-1" />);
  }

  it("documents the manifest format", async () => {
    setup();
    expect(await screen.findByText("Manifest format")).toBeInTheDocument();
    expect(screen.getByText(/tenant_id are refused/)).toBeInTheDocument();
  });

  it("reports preflight problems and still allows the import", async () => {
    setup();
    await userEvent.upload(await screen.findByLabelText(/^Manifest file/), manifestFile(`${goodLine}\n{bad`));
    expect(await screen.findByText(/2 lines, .*1 line has a problem/)).toBeInTheDocument();
    expect(screen.getByText(/Not valid JSON/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start import" })).toBeEnabled();
  });

  it("sends a small file inline with an idempotency key header", async () => {
    push.mockReset();
    let seen: { type: string | null; key: string | null; policy: string | null; sha: string | null } | null = null;
    server.use(
      http.post(`${API}/datasets/ds-1/manifests`, ({ request }) => {
        const url = new URL(request.url);
        seen = {
          type: request.headers.get("content-type"),
          key: request.headers.get("idempotency-key"),
          policy: url.searchParams.get("duplicate_policy"),
          sha: url.searchParams.get("sha256"),
        };
        return HttpResponse.json(job(), { status: 202 });
      }),
    );
    setup();
    await userEvent.upload(await screen.findByLabelText(/^Manifest file/), manifestFile(goodLine));
    await screen.findByText(/1 line,/);
    expect(screen.getByLabelText("Upload method")).toHaveValue("inline");
    await userEvent.selectOptions(screen.getByLabelText("If a member already exists"), "SKIP");
    await userEvent.click(screen.getByRole("button", { name: "Start import" }));
    await waitFor(() => expect(push).toHaveBeenCalledWith("/app/datasets/ds-1/imports/job-1"));
    expect(seen).toMatchObject({ type: "application/x-ndjson", policy: "SKIP" });
    expect(seen!.key).toBeTruthy();
    expect(seen!.sha).toMatch(/^[0-9a-f]{64}$/);
  });

  it("chooses a staged upload for a file over 8 MiB and does not offer inline", async () => {
    setup();
    await userEvent.upload(await screen.findByLabelText(/^Manifest file/), manifestFile(goodLine, 9 * 1024 * 1024));
    await screen.findByText(/1 line,/);
    expect(screen.getByLabelText("Upload method")).toHaveValue("staged");
    expect(screen.getByRole("option", { name: "Inline (up to 8 MiB)" })).toBeDisabled();
  });

  it("maps a 429 too_many_jobs with the wait", async () => {
    server.use(
      http.post(`${API}/datasets/ds-1/manifests`, () =>
        HttpResponse.json({ error: "busy", code: "too_many_jobs" }, { status: 429, headers: { "Retry-After": "60" } }),
      ),
    );
    setup();
    await userEvent.upload(await screen.findByLabelText(/^Manifest file/), manifestFile(goodLine));
    await screen.findByText(/1 line,/);
    await userEvent.click(screen.getByRole("button", { name: "Start import" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Try again in 60 seconds");
  });

  it("is closed for archived datasets and denied without write", async () => {
    server.use(http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset({ lifecycle_status: "ARCHIVED" }))));
    const first = renderWithProviders(<ImportsScreen id="ds-1" />);
    expect(await screen.findByText("Imports are closed")).toBeInTheDocument();
    first.unmount();
    server.use(http.get(`${API}/datasets/ds-1`, () => HttpResponse.json(makeDataset())));
    renderWithProviders(<ImportsScreen id="ds-1" />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("Your role cannot do this")).toBeInTheDocument();
  });
});

describe("ImportJob", () => {
  it("shows progress and pages rejected lines by line number", async () => {
    const afters: (string | null)[] = [];
    server.use(
      http.get(`${API}/datasets/ds-1/manifests/job-1`, () =>
        HttpResponse.json(
          job({
            state: "COMPLETED",
            progress: { lines_read: 8, members_added: 6, members_updated: 0, members_skipped: 0, lines_rejected: 2 },
          }),
        ),
      ),
      http.get(`${API}/datasets/ds-1/manifests/job-1/errors`, ({ request }) => {
        const after = new URL(request.url).searchParams.get("after");
        afters.push(after);
        return after
          ? HttpResponse.json({ errors: [{ line: 8, code: "object_not_found", detail: "unknown" }] })
          : HttpResponse.json({ errors: [{ line: 7, code: "invalid_path", detail: "bad path" }], next_after: 7 });
      }),
    );
    renderWithProviders(<ImportJob id="ds-1" jobId="job-1" />);
    expect(await screen.findByText("Completed")).toBeInTheDocument();
    expect(screen.getByText("Lines rejected").nextSibling).toHaveTextContent("2");
    expect(await screen.findByText("invalid_path")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("object_not_found")).toBeInTheDocument();
    expect(afters).toContain("7");
    expect(screen.queryByRole("button", { name: "Cancel import" })).not.toBeInTheDocument();
  });

  it("cancels a running import and shows that it is stopping on 202", async () => {
    server.use(
      http.get(`${API}/datasets/ds-1/manifests/job-1`, () => HttpResponse.json(job({ state: "RUNNING" }))),
      http.get(`${API}/datasets/ds-1/manifests/job-1/errors`, () => HttpResponse.json({ errors: [] })),
      http.post(`${API}/datasets/ds-1/manifests/job-1/cancel`, () =>
        HttpResponse.json(job({ state: "RUNNING", cancel_requested: true }), { status: 202 }),
      ),
    );
    renderWithProviders(<ImportJob id="ds-1" jobId="job-1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Cancel import" }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel import" }));
    expect(await screen.findByRole("button", { name: "Stopping" })).toBeDisabled();
  });

  it("keeps polling until the import is terminal", async () => {
    let calls = 0;
    server.use(
      http.get(`${API}/datasets/ds-1/manifests/job-1`, () => {
        calls += 1;
        return HttpResponse.json(job({ state: "RUNNING" }));
      }),
      http.get(`${API}/datasets/ds-1/manifests/job-1/errors`, () => HttpResponse.json({ errors: [] })),
    );
    renderWithProviders(<ImportJob id="ds-1" jobId="job-1" />);
    await screen.findByText("Running");
    expect(screen.getByText(/every 3 seconds/)).toBeInTheDocument();
    expect(calls).toBe(1);
  });
});
