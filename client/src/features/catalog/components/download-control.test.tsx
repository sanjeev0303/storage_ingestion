import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { DownloadControl } from "./download-control";

const expires = "2026-06-01T10:05:00Z";

function renderControl(props: Partial<Parameters<typeof DownloadControl>[0]> = {}) {
  return renderWithProviders(
    <DownloadControl assetId="asset-2" fileName="cat.png" canDownload canRevoke {...props} />,
  );
}

describe("DownloadControl", () => {
  it("opens a token link through the same-origin proxy", async () => {
    let body: unknown;
    server.use(
      http.post(`${API}/assets/asset-2/download-url`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ url: "/api/v1/downloads/tok123", expires_at: expires, delivery: "token", revocable: true });
      }),
    );
    renderControl();
    await userEvent.selectOptions(screen.getByLabelText("Link type"), "token");
    await userEvent.click(screen.getByRole("button", { name: /Create download link/ }));

    const link = await screen.findByRole("link", { name: /Download cat.png/ });
    expect(link).toHaveAttribute("href", "/api/proxy/downloads/tok123");
    expect(link).toHaveAttribute("rel", expect.stringContaining("noopener"));
    expect(body).toEqual({ mode: "token", filename: "cat.png" });
  });

  it("uses a storage URL as given and warns that it cannot be revoked", async () => {
    server.use(
      http.post(`${API}/assets/asset-2/download-url`, () =>
        HttpResponse.json({ url: "https://s3.example/obj?sig=1", expires_at: expires, delivery: "storage", revocable: false }),
      ),
    );
    renderControl();
    await userEvent.click(screen.getByRole("button", { name: /Create download link/ }));
    const link = await screen.findByRole("link", { name: /Download cat.png/ });
    expect(link).toHaveAttribute("href", "https://s3.example/obj?sig=1");
    expect(screen.getByText(/It cannot be revoked and works for anyone/)).toBeInTheDocument();
  });

  it("reads the delivery the server chose, not the one requested", async () => {
    server.use(
      http.post(`${API}/assets/asset-2/download-url`, () =>
        HttpResponse.json({ url: "/api/v1/downloads/switched", expires_at: expires, delivery: "token", revocable: true }),
      ),
    );
    renderControl();
    await userEvent.click(screen.getByRole("button", { name: /Create download link/ }));
    expect(await screen.findByRole("link", { name: /Download cat.png/ })).toHaveAttribute(
      "href",
      "/api/proxy/downloads/switched",
    );
  });

  it("explains a version that is not available and a busy service", async () => {
    server.use(
      http.post(`${API}/assets/asset-2/download-url`, () =>
        HttpResponse.json({ error: "asset is not available for delivery" }, { status: 409 }),
      ),
    );
    renderControl();
    await userEvent.click(screen.getByRole("button", { name: /Create download link/ }));
    expect(await screen.findByText(/not available for delivery right now/)).toBeInTheDocument();

    server.use(
      http.post(`${API}/assets/asset-2/download-url`, () =>
        HttpResponse.json({ error: "delivery is temporarily unavailable" }, { status: 503, headers: { "Retry-After": "2" } }),
      ),
    );
    await userEvent.click(screen.getByRole("button", { name: /Create download link/ }));
    expect(await screen.findByText(/Try again in 2 seconds/)).toBeInTheDocument();
  });

  it("revokes token links and says what that covers", async () => {
    server.use(http.delete(`${API}/assets/asset-2/download-links`, () => HttpResponse.json({ revoked: 2 })));
    renderControl();
    await userEvent.click(screen.getByRole("button", { name: "Revoke link-based downloads" }));
    expect(screen.getByText(/Direct storage links cannot be revoked/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Revoke links" }));
    await screen.findByRole("button", { name: "Revoke link-based downloads" });
  });

  it("hides revoke without assets.delete and download without assets.download", () => {
    renderControl({ canDownload: false, canRevoke: false });
    expect(screen.queryByRole("button", { name: /Create download link/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revoke link-based downloads" })).not.toBeInTheDocument();
  });
});
