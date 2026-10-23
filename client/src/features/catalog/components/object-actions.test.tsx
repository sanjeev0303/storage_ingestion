import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { makeObject } from "../fixtures";
import { DeleteObjectButton, RenameButton } from "./object-actions";

describe("RenameButton", () => {
  it("explains a path that is already taken", async () => {
    let body: unknown;
    server.use(
      http.post(`${API}/catalog/objects/obj-1/rename`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ error: "path exists", code: "path_exists" }, { status: 409 });
      }),
    );
    renderWithProviders(<RenameButton object={makeObject()} />);
    await userEvent.click(screen.getByRole("button", { name: "Rename" }));
    const input = screen.getByLabelText(/New path/);
    await userEvent.clear(input);
    await userEvent.type(input, "2026/dog.png");
    await userEvent.click(screen.getByRole("button", { name: "Rename object" }));

    expect(await screen.findByText(/Another object already uses that path/)).toBeInTheDocument();
    expect(body).toEqual({ revision: 3, path: "2026/dog.png" });
  });

  it("explains a path that is busy", async () => {
    server.use(
      http.post(`${API}/catalog/objects/obj-1/rename`, () =>
        HttpResponse.json({ error: "busy", code: "path_busy" }, { status: 409 }),
      ),
    );
    renderWithProviders(<RenameButton object={makeObject()} />);
    await userEvent.click(screen.getByRole("button", { name: "Rename" }));
    const input = screen.getByLabelText(/New path/);
    await userEvent.clear(input);
    await userEvent.type(input, "other.png");
    await userEvent.click(screen.getByRole("button", { name: "Rename object" }));
    expect(await screen.findByText(/upload to that path is in progress/)).toBeInTheDocument();
  });
});

describe("RenameButton form", () => {
  it("is disabled until the path is new, and shows the length", async () => {
    renderWithProviders(<RenameButton object={makeObject()} />);
    await userEvent.click(screen.getByRole("button", { name: "Rename" }));
    expect(screen.getByRole("button", { name: "Rename object" })).toBeDisabled();
    expect(screen.getByText(/ \/ 1024$/)).toBeInTheDocument();

    const input = screen.getByLabelText(/New path/);
    await userEvent.type(input, "x");
    expect(screen.getByRole("button", { name: "Rename object" })).toBeEnabled();
    await userEvent.clear(input);
    await userEvent.tab();
    expect(await screen.findByRole("alert")).toHaveTextContent("Enter the new path.");
  });
});

describe("DeleteObjectButton", () => {
  it("needs the path typed before it deletes", async () => {
    let deleted = false;
    server.use(
      http.delete(`${API}/catalog/objects/obj-1`, () => {
        deleted = true;
        return HttpResponse.json({ object: makeObject(), versions_in_processing: 0 }, { status: 202 });
      }),
    );
    renderWithProviders(<DeleteObjectButton object={makeObject()} />);
    await userEvent.click(screen.getByRole("button", { name: "Delete object" }));

    const confirm = screen.getAllByRole("button", { name: "Delete object" }).at(-1)!;
    expect(confirm).toBeDisabled();
    expect(screen.getByText(/longer than you request/)).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(/Type .* to confirm/), "2026/cat.png");
    expect(confirm).toBeEnabled();
    await userEvent.click(confirm);
    await screen.findByRole("button", { name: "Delete object" });
    expect(deleted).toBe(true);
  });
});
