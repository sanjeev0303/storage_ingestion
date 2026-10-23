import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { validateDuration, validateReason, validateScopeKey } from "../validate";
import { AssetsScreen } from "./assets-screen";
import { DeadLettersScreen } from "./dead-letters-screen";
import { RetentionScreen } from "./retention-screen";

describe("validation", () => {
  it("accepts Go durations and rejects days and weeks", () => {
    expect(validateDuration("")).toBeNull();
    expect(validateDuration("720h")).toBeNull();
    expect(validateDuration("1h30m")).toBeNull();
    expect(validateDuration("30d")).toMatch(/Days and weeks/);
    expect(validateDuration("2w")).toMatch(/Days and weeks/);
    expect(validateDuration("soon")).toMatch(/720h/);
  });

  it("checks scope keys and reasons", () => {
    expect(validateScopeKey("TENANT", "")).toBeNull();
    expect(validateScopeKey("TENANT", "x")).not.toBeNull();
    expect(validateScopeKey("DATASET", "")).not.toBeNull();
    expect(validateScopeKey("DATASET", "Bad Name")).not.toBeNull();
    expect(validateScopeKey("DATASET", "train-set.v1")).toBeNull();
    expect(validateReason("  ")).not.toBeNull();
    expect(validateReason("x".repeat(501))).not.toBeNull();
    expect(validateReason("legal request")).toBeNull();
  });
});

const asset = {
  id: "asset-1",
  file_name: "scan.tiff",
  content_type: "image/tiff",
  size_bytes: 2048,
  status: "AVAILABLE",
  deliverable: true,
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:00:00Z",
};

const lifecycle = {
  asset_id: "asset-1",
  status: "AVAILABLE",
  retain_until: "2027-01-01T00:00:00Z",
  deletion_grace_seconds: 604800,
};

function mockAsset() {
  server.use(
    http.get(`${API}/assets/asset-1`, () => HttpResponse.json(asset)),
    http.get(`${API}/assets/asset-1/lifecycle`, () => HttpResponse.json(lifecycle)),
    http.get(`${API}/assets/asset-1/quarantine`, () => HttpResponse.json({ error: "not found", code: "not_found" }, { status: 404 })),
  );
}

describe("AssetsScreen", () => {
  it("shows the returned deletion state only after the API answers", async () => {
    mockAsset();
    server.use(
      http.delete(`${API}/assets/asset-1`, () =>
        HttpResponse.json(
          {
            asset_id: "asset-1",
            status: "SOFT_DELETED",
            created: true,
            deletion: { id: "del-1", state: "SCHEDULED", purge_after: "2026-11-01T00:00:00Z", restorable: true },
          },
          { status: 202 },
        ),
      ),
    );
    renderWithProviders(<AssetsScreen initialId="asset-1" />);

    await userEvent.click(await screen.findByRole("button", { name: "Delete asset" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/scheduled for purge/)).toBeInTheDocument();
    expect(screen.queryByText("Deletion scheduled")).not.toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole("button", { name: "Schedule deletion" }));

    expect(await screen.findByText("Deletion scheduled")).toBeInTheDocument();
    expect(screen.getByText("SCHEDULED")).toBeInTheDocument();
    expect(screen.getByText("Can be restored until then")).toBeInTheDocument();
  });

  it("explains a missing processing record instead of erroring", async () => {
    mockAsset();
    renderWithProviders(<AssetsScreen initialId="asset-1" />);
    expect(await screen.findByText("No processing record")).toBeInTheDocument();
  });

  it("disables delete without assets.delete", async () => {
    mockAsset();
    renderWithProviders(<AssetsScreen initialId="asset-1" />, { permissions: ["assets.read", "quarantine.read"] });
    expect(await screen.findByRole("button", { name: "Delete asset" })).toBeDisabled();
  });

  it("offers a restore for a soft-deleted asset and shows the returned status", async () => {
    server.use(
      http.get(`${API}/assets/asset-1`, () => HttpResponse.json({ ...asset, status: "SOFT_DELETED" })),
      http.get(`${API}/assets/asset-1/lifecycle`, () =>
        HttpResponse.json({
          ...lifecycle,
          status: "SOFT_DELETED",
          deletion: { id: "del-1", state: "SCHEDULED", purge_after: "2026-11-01T00:00:00Z", restorable: true },
        }),
      ),
      http.get(`${API}/assets/asset-1/quarantine`, () => HttpResponse.json({}, { status: 404 })),
      http.post(`${API}/assets/asset-1/restore`, () => HttpResponse.json({ asset_id: "asset-1", status: "AVAILABLE" })),
    );
    renderWithProviders(<AssetsScreen initialId="asset-1" />);

    await userEvent.click(await screen.findByRole("button", { name: "Restore asset" }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Restore asset" }));

    expect(await screen.findByText("Restored")).toBeInTheDocument();
  });
});

describe("RetentionScreen", () => {
  it("flags day units after the field is left and keeps submit disabled with a reason", async () => {
    renderWithProviders(<RetentionScreen />);

    const button = screen.getByRole("button", { name: "Review and set policy" });
    expect(button).toBeDisabled();
    await userEvent.type(screen.getByLabelText("Minimum retention"), "30d");
    await userEvent.tab();

    expect(await screen.findByText(/Days and weeks are not accepted/)).toBeInTheDocument();
    expect(button).toBeDisabled();
    expect(screen.getAllByText(/A reason is required/).length).toBeGreaterThan(0);
  });

  it("counts reason characters and enables submit once valid", async () => {
    renderWithProviders(<RetentionScreen />);

    await userEvent.type(screen.getByLabelText("Minimum retention"), "1h 30m");
    await userEvent.type(screen.getAllByLabelText(/Reason/)[0], "audit");
    expect(screen.getAllByText("5 / 500").length).toBe(1);
    expect(screen.getByRole("button", { name: "Review and set policy" })).toBeEnabled();
  });

  it("confirms, then sends a valid policy", async () => {
    let body: unknown;
    server.use(
      http.put(`${API}/retention/policies`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({
          id: "pol-1",
          scope: "TENANT",
          scope_key: "",
          version: 3,
          effective_from: "2026-10-22T10:00:00Z",
        });
      }),
    );
    renderWithProviders(<RetentionScreen />);

    await userEvent.type(screen.getByLabelText("Minimum retention"), "720h");
    await userEvent.type(screen.getAllByLabelText(/Reason/)[0], "policy review");
    await userEvent.click(screen.getByRole("button", { name: "Review and set policy" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(body).toBeUndefined();
    await userEvent.click(within(dialog).getByRole("button", { name: "Set policy" }));

    expect(await screen.findByText("Policy saved")).toBeInTheDocument();
    expect(body).toEqual({ scope: "TENANT", min_retention: "720h", reason: "policy review" });
  });

  it("shows a placed legal hold id with a copy button", async () => {
    server.use(
      http.post(`${API}/legal-holds`, () =>
        HttpResponse.json(
          { id: "hold-123", scope: "TENANT", scope_key: "", placed_at: "2026-10-22T10:00:00Z" },
          { status: 201 },
        ),
      ),
    );
    renderWithProviders(<RetentionScreen />);

    await userEvent.type(screen.getAllByLabelText(/Reason/)[1], "court order");
    await userEvent.click(screen.getByRole("button", { name: "Place legal hold" }));

    expect(await screen.findByText("hold-123")).toBeInTheDocument();
    expect(screen.getByText("Placed in this session")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Copy id" })).toBeInTheDocument();
  });

  it("is blocked without retention.manage", () => {
    renderWithProviders(<RetentionScreen />, { permissions: ["assets.read"] });
    expect(screen.getByText(/need the retention.manage permission/)).toBeInTheDocument();
  });
});

const letter = {
  id: "dl-1",
  event_id: "evt-1",
  event_type: "asset.promoted",
  consumer: "indexer",
  status: "OPEN",
  category: "EXHAUSTED",
  last_error: "timeout",
  created_at: "2026-10-22T09:00:00Z",
};

describe("DeadLettersScreen", () => {
  const admin = { user: { platform_role: "ADMIN" as const } };

  it("explains to non-admins that it is platform only", () => {
    renderWithProviders(<DeadLettersScreen />);
    expect(screen.getByText(/limited to platform administrators/)).toBeInTheDocument();
  });

  it("says when the service is not wired", async () => {
    server.use(http.get(`${API}/admin/dead-letters`, () => new HttpResponse("404 page not found", { status: 404 })));
    renderWithProviders(<DeadLettersScreen />, admin);
    expect(await screen.findByText(/not enabled on this deployment/)).toBeInTheDocument();
  });

  it("requires a reason, then sends it and shows the replay event id", async () => {
    const bodies: unknown[] = [];
    server.use(
      http.get(`${API}/admin/dead-letters`, () => HttpResponse.json({ dead_letters: [letter] })),
      http.get(`${API}/admin/dead-letters/dl-1`, () => HttpResponse.json(letter)),
      http.post(`${API}/admin/dead-letters/dl-1/replay`, async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ replay_event_id: "evt-replay-9" });
      }),
    );
    renderWithProviders(<DeadLettersScreen />, admin);

    await userEvent.click(await screen.findByRole("button", { name: /Inspect/ }));
    await userEvent.click(await screen.findByRole("button", { name: "Replay" }));
    const dialog = await screen.findByRole("alertdialog");

    await userEvent.click(within(dialog).getByRole("button", { name: "Replay" }));
    expect(await within(dialog).findByText(/A reason is required/)).toBeInTheDocument();
    expect(bodies).toHaveLength(0);

    await userEvent.type(within(dialog).getByLabelText(/Reason/), "consumer fixed");
    await userEvent.click(within(dialog).getByRole("button", { name: "Replay" }));

    expect(await screen.findByText("evt-replay-9")).toBeInTheDocument();
    expect(bodies).toEqual([{ reason: "consumer fixed" }]);
  }, 20000);
});
