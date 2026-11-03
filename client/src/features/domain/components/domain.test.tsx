import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { VersionDomain } from "./version-domain";

const base = `${API}/datasets/d1/versions/v1`;

function routes(publication: object | null = null, recordings: object[] = []) {
  server.use(
    http.get(`${base}/publication`, () => (publication ? HttpResponse.json(publication) : HttpResponse.json({ error: "not found", code: "not_found" }, { status: 404 }))),
    http.get(`${base}/recordings`, () => HttpResponse.json({ recordings })),
    http.get(`${base}/segments`, () => HttpResponse.json({ segments: [] })),
    http.get(`${base}/embedding-indexes`, () => HttpResponse.json({ indexes: [] })),
    http.get(`${base}/retention-cost`, () =>
      HttpResponse.json({ total_bytes: 1, years: 10, note: "Storage only.", tiers: [{ tier: "S3 Standard", price_per_gb_month: 0.023, per_year: 12, total: 120 }] }),
    ),
  );
}

describe("VersionDomain", () => {
  it("publishes, saying what stays private, and shows a DOI that failed", async () => {
    routes();
    let sent: Record<string, unknown> = {};
    server.use(
      http.post(`${base}/publish`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        routes({
          landing_url: "https://c/published/s1",
          publication: { id: "p1", slug: "s1", title: "Logs", license: "CC-BY-4.0", doi_error: "DataCite answered 503", created_at: "2026-11-02T00:00:00Z" },
        });
        return HttpResponse.json({ landing_url: "https://c/published/s1", publication: { doi_error: "DataCite answered 503" } }, { status: 201 });
      }),
    );
    renderWithProviders(<VersionDomain id="d1" versionId="v1" version={3} />, { permissions: ["datasets.manage", "datasets.write", "datasets.read"] });
    expect(await screen.findByText(/File names and data stay private/)).toBeInTheDocument();
    expect(await screen.findByText("$120.00")).toBeInTheDocument();
    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Title"), "Logs");
    await user.type(screen.getByLabelText(/^Creators/), "Ada; Grace");
    await user.type(screen.getByLabelText("Publisher"), "Acme");
    await user.click(screen.getByLabelText("Register a DOI with DataCite"));
    await user.click(screen.getByRole("button", { name: "Publish this snapshot" }));
    await waitFor(() => expect(sent.mint_doi).toBe(true));
    expect(sent.creators).toEqual([
      { name: "Ada", affiliation: "", orcid: "" },
      { name: "Grace", affiliation: "", orcid: "" },
    ]);
    expect(await screen.findByText(/The DOI was not registered: DataCite answered 503/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try the DOI again" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "https://c/published/s1" })).toBeInTheDocument();
  });

  it("asks why before withdrawing", async () => {
    routes({ landing_url: "https://c/published/s1", publication: { id: "p1", slug: "s1", title: "Logs", license: "MIT", doi: "10.5072/osp.s1", created_at: "2026-11-02T00:00:00Z" } });
    let reason = "";
    server.use(
      http.post(`${base}/publication/withdraw`, async ({ request }) => {
        reason = ((await request.json()) as { reason: string }).reason;
        return HttpResponse.json({ landing_url: "x", publication: {} });
      }),
    );
    renderWithProviders(<VersionDomain id="d1" versionId="v1" version={3} />, { permissions: ["datasets.manage"] });
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Withdraw" }));
    await user.type(screen.getByLabelText(/^Why it is withdrawn/), "wrong calibration");
    await user.click(screen.getByRole("button", { name: "Withdraw publication" }));
    await waitFor(() => expect(reason).toBe("wrong calibration"));
  });

  it("shows recordings with their topics, and nothing when there are none", async () => {
    routes(null, [
      { asset_id: "a1", path: "logs/run1.mcap", kind: "MCAP", start_ns: 0, end_ns: 8_000_000_000, message_count: 42, topics: [{ topic: "/imu", schema: "sensor_msgs/Imu", message_count: 42 }] },
    ]);
    renderWithProviders(<VersionDomain id="d1" versionId="v1" version={3} />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("logs/run1.mcap")).toBeInTheDocument();
    expect(screen.getByText("/imu")).toBeInTheDocument();
    expect(screen.getByText("8 s")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add segment" })).not.toBeInTheDocument();
  });

  it("leaves recordings out of a snapshot without any", async () => {
    routes();
    renderWithProviders(<VersionDomain id="d1" versionId="v1" version={3} />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("Embedding indexes")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText("Recordings and segments")).not.toBeInTheDocument());
  });
});
