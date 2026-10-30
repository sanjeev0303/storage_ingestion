import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { ComplianceScreen } from "./compliance-screen";
import { DatasetProvenance } from "./dataset-provenance";
import { EvidenceScreen } from "./evidence-screen";
import { ReviewsScreen } from "./reviews-screen";
import { VersionEvidence } from "./version-evidence";

vi.mock("next/navigation", () => ({ usePathname: () => "/app/audit/evidence" }));

const settings = {
  anchor_connection_id: "",
  anchor_every_minutes: 60,
  anchor_lock_days: 2555,
  review_every_days: 7,
  work_start_hour: 8,
  work_end_hour: 19,
  timezone: "UTC",
  bulk_read_threshold: 500,
  require_lawful_basis: false,
  updated_at: "2026-10-30T10:00:00Z",
};

const anchor = (seq: number, extra: object = {}) => ({
  id: `an${seq}`, chain: "org-1", seq, hash: "ab".repeat(32), body: "{}", created_at: "2026-10-30T10:00:00Z",
  signature_ok: true, chain_ok: true, ...extra,
});

describe("EvidenceScreen", () => {
  it("lists checked anchors, anchors now and saves the settings", async () => {
    let saved: Record<string, unknown> | undefined;
    let anchored = false;
    server.use(
      http.get(`${API}/organization/audit/anchors`, () =>
        HttpResponse.json({ anchors: [anchor(42, { object_key: "ready/org-1/.anchors/42.json" }), anchor(7, { chain_ok: false, problem: "the chain now has a different hash at entry 7" })] })),
      http.post(`${API}/organization/audit/anchors`, () => {
        anchored = true;
        return HttpResponse.json(anchor(43), { status: 201 });
      }),
      http.get(`${API}/evidence-settings`, () => HttpResponse.json(settings)),
      http.put(`${API}/evidence-settings`, async ({ request }) => {
        saved = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(saved);
      }),
      http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [{ id: "sc_lock", name: "Locked bucket", status: "ACTIVE" }] })),
    );
    renderWithProviders(<EvidenceScreen />, { permissions: ["audit.read", "org.manage", "storage.read"] });

    expect(await screen.findByText("ready/org-1/.anchors/42.json")).toBeInTheDocument();
    expect(screen.getByText("Does not match")).toBeInTheDocument();
    expect(screen.getByText(/different hash at entry 7/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Anchor now" }));
    await waitFor(() => expect(anchored).toBe(true));

    await screen.findByRole("option", { name: "Locked bucket" });
    await userEvent.selectOptions(screen.getByLabelText("Anchor connection"), "sc_lock");
    await userEvent.click(screen.getByLabelText(/Require a lawful basis/));
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(saved).toMatchObject({ anchor_connection_id: "sc_lock", require_lawful_basis: true, anchor_every_minutes: 60 }));
  });

  it("will not save a working day that ends before it starts", async () => {
    server.use(
      http.get(`${API}/organization/audit/anchors`, () => HttpResponse.json({ anchors: [] })),
      http.get(`${API}/evidence-settings`, () => HttpResponse.json(settings)),
      http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [] })),
    );
    renderWithProviders(<EvidenceScreen />, { permissions: ["audit.read", "org.manage"] });
    const start = await screen.findByLabelText("Working day starts");
    await userEvent.clear(start);
    await userEvent.type(start, "20");
    expect(screen.getByRole("button", { name: "Save settings" })).toBeDisabled();
    expect(screen.getByText("Fix the fields marked above.")).toBeInTheDocument();
  });
});

const review = {
  id: "rv1", period_start: "2026-10-23T00:00:00Z", period_end: "2026-10-30T00:00:00Z",
  findings: [{ kind: "DELETION", at: "2026-10-29T12:00:00Z", actor: "user-2", detail: "asset.delete on a1", audit_seq: 12 }],
  counts: { DELETION: 1 }, report_sha256: "cd".repeat(32), state: "OPEN", created_by: "worker", created_at: "2026-10-30T00:00:00Z",
};

describe("ReviewsScreen", () => {
  it("shows the findings and signs the review with a note", async () => {
    let note: unknown;
    server.use(
      http.get(`${API}/audit-reviews`, () => HttpResponse.json({ reviews: [review] })),
      http.post(`${API}/audit-reviews/rv1/sign`, async ({ request }) => {
        note = ((await request.json()) as { note: string }).note;
        return HttpResponse.json({ ...review, state: "SIGNED", signed_by: "user-1", signed_at: "2026-10-30T11:00:00Z" });
      }),
    );
    renderWithProviders(<ReviewsScreen />, { permissions: ["audit.read"] });
    expect(await screen.findByText("asset.delete on a1")).toBeInTheDocument();
    expect(screen.getByText("Entry 12")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText(/Reviewer's note/), "Deletion was the approved purge.");
    await userEvent.click(screen.getByRole("button", { name: "Sign off review" }));
    await waitFor(() => expect(note).toBe("Deletion was the approved purge."));
  });
});

describe("ComplianceScreen", () => {
  it("shows the controls and a regulator token once", async () => {
    server.use(
      http.get(`${API}/compliance/frameworks`, () =>
        HttpResponse.json({ frameworks: [{ id: "nist-800-171", name: "NIST SP 800-171" }, { id: "sec-17a-4", name: "SEC 17a-4" }] })),
      http.get(`${API}/compliance/nist-800-171`, () =>
        HttpResponse.json({
          id: "nist-800-171", name: "NIST SP 800-171", note: "The organization runs its own procedures.",
          controls: [{ id: "3.3.8", title: "Protect audit information", platform: "Hash chain, anchored", status: "MET", evidence: ["last anchor at entry 42"] }],
        })),
      http.post(`${API}/compliance/regulator-access`, () =>
        HttpResponse.json({ id: "t1", name: "Regulator: FINRA", expires_at: "2026-11-06T00:00:00Z", token: "osp_pat_example" }, { status: 201 })),
    );
    renderWithProviders(<ComplianceScreen />, { permissions: ["audit.read", "org.manage"] });
    expect(await screen.findByText("Protect audit information")).toBeInTheDocument();
    expect(screen.getByText("last anchor at entry 42")).toBeInTheDocument();
    expect(screen.getByText("Met")).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(/Examiner/), "FINRA");
    await userEvent.click(screen.getByRole("button", { name: "Issue token" }));
    expect(await screen.findByText("osp_pat_example")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText("osp_pat_example")).not.toBeInTheDocument();
  });

  it("hides regulator access from roles that cannot manage the organization", async () => {
    server.use(
      http.get(`${API}/compliance/frameworks`, () => HttpResponse.json({ frameworks: [] })),
      http.get(`${API}/compliance/nist-800-171`, () => HttpResponse.json({ id: "nist-800-171", name: "NIST", note: "", controls: [] })),
    );
    renderWithProviders(<ComplianceScreen />, { permissions: ["audit.read"] });
    await screen.findByText("NIST");
    expect(screen.queryByText("Regulator access")).not.toBeInTheDocument();
  });
});

describe("DatasetProvenance", () => {
  it("records the basis, licence and source", async () => {
    let body: unknown;
    server.use(
      http.get(`${API}/datasets/ds1/provenance`, () => HttpResponse.json({ lawful_basis: "", license: "", source: "", source_kind: "" })),
      http.put(`${API}/datasets/ds1/provenance`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ...(body as object), recorded_at: "2026-10-30T10:00:00Z", recorded_by: "user-1" });
      }),
    );
    renderWithProviders(<DatasetProvenance datasetId="ds1" />, { permissions: ["datasets.read", "datasets.manage"] });
    expect(await screen.findByText("Not recorded yet.")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText(/Lawful basis/), "Legitimate interest ");
    await userEvent.type(screen.getByLabelText(/Licence/), "CC BY 4.0");
    await userEvent.type(screen.getByLabelText(/^Source/), "Common Crawl 2026-09");
    await userEvent.selectOptions(screen.getByLabelText("Kind of source"), "WEB_CRAWLED");
    await userEvent.click(screen.getByRole("button", { name: "Record basis" }));
    await waitFor(() =>
      expect(body).toEqual({ lawful_basis: "Legitimate interest", license: "CC BY 4.0", source: "Common Crawl 2026-09", source_kind: "WEB_CRAWLED" }));
  });
});

describe("VersionEvidence", () => {
  it("places a litigation hold and releases it", async () => {
    let placed: unknown;
    let released: unknown;
    let holds = [] as object[];
    server.use(
      http.get(`${API}/datasets/ds1/versions/v1/holds`, () => HttpResponse.json({ holds })),
      http.post(`${API}/datasets/ds1/versions/v1/holds`, async ({ request }) => {
        placed = await request.json();
        holds = [{ matter: "snapshot 3: LITIGATION 24-cv-118", kind: "LITIGATION", hold_ids: ["h1", "h2"], released: false }];
        return HttpResponse.json(holds[0], { status: 201 });
      }),
      http.post(`${API}/datasets/ds1/versions/v1/holds/release`, async ({ request }) => {
        released = await request.json();
        return HttpResponse.json({ released: 2 });
      }),
    );
    renderWithProviders(<VersionEvidence id="ds1" versionId="v1" version={3} />, {
      permissions: ["audit.read", "assets.download", "retention.manage"],
    });
    expect(await screen.findByText("No holds.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Download evidence pack" })).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText(/Reference/), "24-cv-118");
    await userEvent.click(screen.getByRole("button", { name: "Hold this version" }));
    await waitFor(() => expect(placed).toEqual({ kind: "LITIGATION", reference: "24-cv-118", reason: "" }));
    expect(await screen.findByText("snapshot 3: LITIGATION 24-cv-118")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Release" }));
    await userEvent.click(await screen.findByRole("button", { name: "Release hold" }));
    await waitFor(() => expect(released).toEqual({ matter: "snapshot 3: LITIGATION 24-cv-118", reason: "" }));
  });
});
