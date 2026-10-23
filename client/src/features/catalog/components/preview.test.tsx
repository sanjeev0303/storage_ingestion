import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it, vi } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { Preview } from "./preview";

afterEach(() => vi.restoreAllMocks());

describe("Preview", () => {
  it("does not offer a preview for svg, html or oversize files", () => {
    const { rerender } = renderWithProviders(
      <Preview assetId="a" contentType="image/svg+xml" sizeBytes={10} status="AVAILABLE" />,
    );
    expect(screen.queryByRole("button", { name: "Show preview" })).not.toBeInTheDocument();
    rerender(<Preview assetId="a" contentType="text/html" sizeBytes={10} status="AVAILABLE" />);
    expect(screen.queryByRole("button", { name: "Show preview" })).not.toBeInTheDocument();
    rerender(<Preview assetId="a" contentType="image/png" sizeBytes={30 * 1024 * 1024} status="AVAILABLE" />);
    expect(screen.getByText(/Too large to preview/)).toBeInTheDocument();
    rerender(<Preview assetId="a" contentType="image/png" sizeBytes={10} status="SOFT_DELETED" />);
    expect(screen.queryByRole("button", { name: "Show preview" })).not.toBeInTheDocument();
  });

  it("shows text as text, never as markup", async () => {
    server.use(http.get(`${API}/assets/a/content`, () => new HttpResponse('<img src=x onerror="alert(1)"><b>hi</b>', { headers: { "Content-Type": "text/plain" } })));
    const { container } = renderWithProviders(
      <Preview assetId="a" contentType="text/plain" sizeBytes={40} status="AVAILABLE" />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Show preview" }));
    const pre = await screen.findByLabelText("File preview");
    expect(pre.textContent).toContain('<img src=x onerror="alert(1)"><b>hi</b>');
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
  });

  it("renders an image from a blob URL and revokes it on unmount", async () => {
    const create = vi.fn(() => "blob:preview-1");
    const revoke = vi.fn();
    URL.createObjectURL = create;
    URL.revokeObjectURL = revoke;
    server.use(http.get(`${API}/assets/a/content`, () => new HttpResponse(new Uint8Array([1, 2, 3]), { headers: { "Content-Type": "image/png" } })));
    const { unmount } = renderWithProviders(
      <Preview assetId="a" contentType="image/png" sizeBytes={3} status="AVAILABLE" />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Show preview" }));
    const image = await screen.findByRole("img");
    expect(image).toHaveAttribute("src", "blob:preview-1");
    unmount();
    expect(revoke).toHaveBeenCalledWith("blob:preview-1");
  });

  it("shows the problem when the bytes cannot be fetched", async () => {
    server.use(http.get(`${API}/assets/a/content`, () => HttpResponse.json({ error: "asset is not available for delivery" }, { status: 409 })));
    renderWithProviders(<Preview assetId="a" contentType="text/plain" sizeBytes={4} status="AVAILABLE" />);
    await userEvent.click(screen.getByRole("button", { name: "Show preview" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/not available for delivery/);
  });
});
