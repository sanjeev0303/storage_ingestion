import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { AdoptionScreen } from "./adoption-screen";

const connection = {
  id: "conn-1", name: "lab bucket", provider: "AWS_S3", status: "ACTIVE", auth_method: "ACCESS_KEY", bucket: "lab",
  force_path_style: false,
};

const job = {
  id: "ad-1", connection_id: "conn-1", dataset: "", source: "S3_INVENTORY", source_ref: "inv/manifest.json", mode: "DETACH",
  require_digest: true, state: "COMPLETED", entries: 3, adopted: 2, rejected: 1, failed: 0, bytes: 2048, created_by: "u",
  created_at: "2026-10-30T09:00:00Z",
};

describe("AdoptionScreen", () => {
  it("starts an adoption from a connection's inventory report", async () => {
    let sent: unknown;
    server.use(
      http.get(`${API}/adoptions`, () => HttpResponse.json({ adoptions: [] })),
      http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [connection], default_connection_id: "platform" })),
      http.post(`${API}/adoptions`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ ...job, state: "QUEUED" }, { status: 201 });
      }),
    );
    renderWithProviders(<AdoptionScreen />, { permissions: ["ingestion.read", "ingestion.write"] });
    expect(await screen.findByText("Nothing adopted yet")).toBeInTheDocument();
    await userEvent.selectOptions(await screen.findByLabelText(/Storage connection/), "conn-1");
    await userEvent.type(screen.getByLabelText(/manifest.json/), "inv/manifest.json");
    await userEvent.click(screen.getByRole("button", { name: "Start" }));
    await waitFor(() =>
      expect(sent).toEqual({ source: "S3_INVENTORY", connection_id: "conn-1", source_ref: "inv/manifest.json", mode: "DETACH", require_digest: true }),
    );
  });

  it("shows a job's counters and its entries' outcomes", async () => {
    server.use(
      http.get(`${API}/adoptions`, () => HttpResponse.json({ adoptions: [job] })),
      http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [], default_connection_id: "platform" })),
      http.get(`${API}/adoptions/ad-1/results`, () =>
        HttpResponse.json({
          entries: [
            { line: 1, object_key: "run1/a.fastq", path: "run1/a.fastq", size_bytes: 1024, state: "ADOPTED" },
            { line: 2, object_key: "run1/b.fastq", path: "run1/b.fastq", size_bytes: 1, state: "REJECTED", code: "digest_mismatch", detail: "md5" },
          ],
          next_after: 0,
        }),
      ),
    );
    renderWithProviders(<AdoptionScreen />, { permissions: ["ingestion.read"] });
    expect(await screen.findByText(/3 entries: 2 adopted/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Entries" }));
    expect(await screen.findByText("digest_mismatch")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Start" })).not.toBeInTheDocument();
  });
});
