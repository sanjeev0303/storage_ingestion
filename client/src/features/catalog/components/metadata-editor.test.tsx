import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { axe } from "vitest-axe";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeObject } from "../fixtures";
import { MetadataEditor } from "./metadata-editor";

describe("MetadataEditor", () => {
  it("sends the revision and a merge patch", async () => {
    let body: unknown;
    server.use(
      http.patch(`${API}/catalog/objects/obj-1`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(makeObject({ revision: 4, metadata: { stage: "final", owner: "ml" } }));
      }),
    );
    renderWithProviders(<MetadataEditor object={makeObject()} canEdit />);

    const value = screen.getByLabelText("Value 1");
    await userEvent.clear(value);
    await userEvent.type(value, "final");
    await userEvent.click(screen.getByRole("button", { name: "Add key" }));
    await userEvent.type(screen.getByLabelText(/^Key 2/), "owner");
    await userEvent.type(screen.getByLabelText("Value 2"), "ml");
    await userEvent.click(screen.getByRole("button", { name: "Save metadata" }));

    await screen.findByDisplayValue("ml");
    expect(body).toEqual({ revision: 3, metadata: { stage: "final", owner: "ml" } });
  });

  it("sends null for a removed key", async () => {
    let body: unknown;
    server.use(
      http.patch(`${API}/catalog/objects/obj-1`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(makeObject({ metadata: {} }));
      }),
    );
    renderWithProviders(<MetadataEditor object={makeObject()} canEdit />);
    await userEvent.click(screen.getByRole("button", { name: "Remove key stage" }));
    await userEvent.click(screen.getByRole("button", { name: "Save metadata" }));
    await screen.findByText(/No metadata is set/);
    expect(body).toEqual({ revision: 3, metadata: { stage: null } });
  });

  it("refuses reserved keys before calling the API", async () => {
    let called = false;
    server.use(
      http.patch(`${API}/catalog/objects/obj-1`, () => {
        called = true;
        return HttpResponse.json(makeObject());
      }),
    );
    renderWithProviders(<MetadataEditor object={makeObject()} canEdit />);
    await userEvent.click(screen.getByRole("button", { name: "Add key" }));
    await userEvent.type(screen.getByLabelText(/^Key 2/), "system.owner");
    await userEvent.click(screen.getByRole("button", { name: "Save metadata" }));
    expect(await screen.findByText(/reserved/)).toBeInTheDocument();
    expect(called).toBe(false);
  });

  it("explains a revision conflict and keeps the edits", async () => {
    server.use(
      http.patch(`${API}/catalog/objects/obj-1`, () =>
        HttpResponse.json({ error: "revision conflict", code: "revision_conflict" }, { status: 412 }),
      ),
      http.get(`${API}/catalog/objects/obj-1`, () => HttpResponse.json(makeObject({ revision: 9 }))),
    );
    renderWithProviders(<MetadataEditor object={makeObject()} canEdit />);
    const value = screen.getByLabelText("Value 1");
    await userEvent.clear(value);
    await userEvent.type(value, "mine");
    await userEvent.click(screen.getByRole("button", { name: "Save metadata" }));

    expect(await screen.findByText(/Someone else changed this object/)).toBeInTheDocument();
    expect(screen.getByLabelText("Value 1")).toHaveValue("mine");
  });

  it("is read-only without catalog.write", () => {
    renderWithProviders(<MetadataEditor object={makeObject()} canEdit={false} />, { permissions: ["assets.read"] });
    expect(screen.queryByRole("button", { name: "Save metadata" })).not.toBeInTheDocument();
    expect(screen.getByText("draft")).toBeInTheDocument();
  });

  it("has no accessibility violations", async () => {
    const { container } = renderWithProviders(<MetadataEditor object={makeObject()} canEdit />);
    expect(await axe(container)).toHaveNoViolations();
  });
});
