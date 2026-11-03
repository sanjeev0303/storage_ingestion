import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import { ChatSettings } from "./chat-settings";
import { ConnectScreen } from "./connect-screen";
import { SharePage } from "./share-page";
import { ShareToChat } from "./share-to-chat";

const share = { id: "s1", asset_id: "a1", integration_id: "c1", note: "for review", expires_at: "2099-01-01T00:00:00Z", opens: 2,
  created_by: "u1", created_at: "2026-11-02T00:00:00Z", path: "docs/brief.txt", size_bytes: 2048 };

describe("ChatSettings", () => {
  it("adds a channel and revokes a live link", async () => {
    let added: unknown;
    let revoked = false;
    server.use(
      http.get(`${API}/chat-integrations`, () => HttpResponse.json({ integrations: [] })),
      http.get(`${API}/chat-shares`, () => HttpResponse.json({ shares: [share] })),
      http.post(`${API}/chat-integrations`, async ({ request }) => {
        added = await request.json();
        return HttpResponse.json({ id: "c1" }, { status: 201 });
      }),
      http.delete(`${API}/chat-shares/s1`, () => {
        revoked = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderWithProviders(<ChatSettings />, { permissions: ["org.read", "org.manage"] });
    expect(await screen.findByText("docs/brief.txt")).toBeInTheDocument();
    expect(screen.getByText("Live")).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText(/^Name/), "#data-review");
    await userEvent.type(screen.getByLabelText(/^Webhook URL/), "https://hooks.slack.com/services/T/B/C");
    await userEvent.click(screen.getByRole("button", { name: "Add channel" }));
    await waitFor(() => expect(added).toEqual({ kind: "SLACK", name: "#data-review", url: "https://hooks.slack.com/services/T/B/C" }));
    await userEvent.click(screen.getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revoked).toBe(true));
  });

  it("does not offer channel changes without org.manage", async () => {
    server.use(
      http.get(`${API}/chat-integrations`, () => HttpResponse.json({ integrations: [] })),
      http.get(`${API}/chat-shares`, () => HttpResponse.json({ shares: [] })),
    );
    renderWithProviders(<ChatSettings />, { permissions: ["org.read"] });
    expect(await screen.findByText("No channels yet")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add channel" })).not.toBeInTheDocument();
  });
});

describe("ShareToChat", () => {
  it("posts to the chosen channel", async () => {
    let body: unknown;
    server.use(
      http.get(`${API}/chat-integrations`, () => HttpResponse.json({ integrations: [{ id: "c1", kind: "SLACK", name: "#eng", url_host: "hooks.slack.com", created_at: "" }] })),
      http.post(`${API}/chat-shares`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(share, { status: 201 });
      }),
    );
    renderWithProviders(<ShareToChat assetId="a1" />);
    await userEvent.click(screen.getByRole("button", { name: "Share to chat" }));
    await screen.findByRole("option", { name: "#eng" });
    await userEvent.selectOptions(screen.getByLabelText("Channel"), "c1");
    await userEvent.click(screen.getByRole("button", { name: "Post link" }));
    await waitFor(() => expect(body).toEqual({ asset_id: "a1", integration_id: "c1", note: "", expires_in: "24h" }));
  });
});

describe("SharePage", () => {
  it("says a dead link has stopped working", async () => {
    server.use(http.get(`${API}/chat-shares/s1`, () => HttpResponse.json({ error: "gone", code: "link_expired" }, { status: 410 })));
    renderWithProviders(<SharePage id="s1" />);
    expect(await screen.findByText("This link has stopped working")).toBeInTheDocument();
  });

  it("shows a live one", async () => {
    server.use(http.get(`${API}/chat-shares/s1`, () => HttpResponse.json(share)));
    renderWithProviders(<SharePage id="s1" />);
    expect(await screen.findByRole("button", { name: "Download" })).toBeInTheDocument();
    expect(screen.getByText("for review")).toBeInTheDocument();
  });
});

describe("ConnectScreen", () => {
  it("shows the LFS setup and what an import found", async () => {
    server.use(
      http.get(`${API}/datasets/d1`, () => HttpResponse.json({ id: "d1", name: "game", revision: 1 })),
      http.get(`${API}/datasets/d1/collections`, () =>
        HttpResponse.json({ collections: [{ id: "x", format: "RSMF", source: "slack.rsmf", export_sha256: "ab", items: 3, verified: 2,
          mismatches: ["e1: attachments not in the file: a.jpg"], retention: "61320h", created_at: "2026-11-02T00:00:00Z" }] })),
    );
    renderWithProviders(<ConnectScreen id="d1" />);
    expect(await screen.findByText("2 of 3")).toBeInTheDocument();
    expect(screen.getByText("1 to look at")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText(/lfs\.url \$OSP_API\/lfs\/game/)).toBeInTheDocument());
  });
});
