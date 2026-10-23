import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeObject, makeVersion } from "../fixtures";
import { VersionDetail } from "./version-detail";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }), useSearchParams: () => new URLSearchParams() }));

const lifecycle = {
  asset_id: "asset-2",
  status: "AVAILABLE",
  retain_until: "2026-03-01T00:00:00Z",
  deletion_grace_seconds: 604800,
};

function serve(version = makeVersion()) {
  server.use(
    http.get(`${API}/catalog/objects/obj-1/versions/current`, () => HttpResponse.json(version)),
    http.get(`${API}/assets/${version.asset_id}/lifecycle`, () => HttpResponse.json({ ...lifecycle, asset_id: version.asset_id })),
  );
}

beforeEach(() => serve());

describe("VersionDetail", () => {
  it("offers download for an available version", async () => {
    renderWithProviders(<VersionDetail object={makeObject()} selector="current" title="Current version" />);
    expect(await screen.findByRole("button", { name: /Create download link/ })).toBeInTheDocument();
    expect(screen.getByText("SHA-256 checksum")).toBeInTheDocument();
    expect(screen.getByText(/store validator, not a checksum/)).toBeInTheDocument();
  });

  it("never offers download, preview or delete for a soft-deleted version", async () => {
    serve(makeVersion({ status: "SOFT_DELETED", current: false, version: 1, asset_id: "asset-1" }));
    server.use(
      http.get(`${API}/catalog/objects/obj-1/versions/current`, () =>
        HttpResponse.json(makeVersion({ status: "SOFT_DELETED", current: false })),
      ),
    );
    renderWithProviders(<VersionDetail object={makeObject()} selector="current" title="Version 1" />);
    expect(await screen.findByText(/This version is soft-deleted/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Create download link/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Show preview" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete this version" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Restore in Operations" })).toHaveAttribute(
      "href",
      "/app/operations/assets?id=asset-2",
    );
  });

  it("shows no actions for a purged version", async () => {
    server.use(
      http.get(`${API}/catalog/objects/obj-1/versions/current`, () => HttpResponse.json(makeVersion({ status: "DELETED" }))),
    );
    renderWithProviders(<VersionDetail object={makeObject()} selector="current" title="Version 1" />);
    expect(await screen.findByText(/bytes were purged|bytes of this version have been purged/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Create download link/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Restore in Operations" })).not.toBeInTheDocument();
  });

  it("hides promote and delete for a read-only role", async () => {
    server.use(
      http.get(`${API}/catalog/objects/obj-1/versions/1`, () =>
        HttpResponse.json(makeVersion({ version: 1, current: false, asset_id: "asset-1" })),
      ),
      http.get(`${API}/assets/asset-1/lifecycle`, () => HttpResponse.json({ ...lifecycle, asset_id: "asset-1" })),
    );
    renderWithProviders(<VersionDetail object={makeObject()} selector={1} title="Version 1" />, {
      permissions: ["assets.read"],
    });
    expect(await screen.findByText(/Your role cannot download files/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Make this the current version" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete this version" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revoke link-based downloads" })).not.toBeInTheDocument();
  });

  it("promotes with the object revision", async () => {
    let body: unknown;
    server.use(
      http.get(`${API}/catalog/objects/obj-1/versions/1`, () =>
        HttpResponse.json(makeVersion({ version: 1, current: false, asset_id: "asset-1" })),
      ),
      http.get(`${API}/assets/asset-1/lifecycle`, () => HttpResponse.json({ ...lifecycle, asset_id: "asset-1" })),
      http.post(`${API}/catalog/objects/obj-1/versions/1/promote`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(makeObject({ revision: 4 }));
      }),
    );
    renderWithProviders(<VersionDetail object={makeObject()} selector={1} title="Version 1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Make this the current version" }));
    await userEvent.click(screen.getByRole("button", { name: "Make current" }));
    await waitFor(() => expect(body).toEqual({ revision: 3 }));
  });

  it("requires the typed path to delete a version", async () => {
    renderWithProviders(<VersionDetail object={makeObject()} selector="current" title="Current version" />);
    await userEvent.click(await screen.findByRole("button", { name: "Delete this version" }));
    const confirm = screen.getByRole("button", { name: "Delete version" });
    expect(confirm).toBeDisabled();
    expect(screen.getByText(/longer than you request/)).toBeInTheDocument();
  });
});
