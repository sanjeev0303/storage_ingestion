import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { WebhooksScreen } from "./webhooks-screen";

const EVENTS = ["object.available", "dataset.snapshot_created", "webhook.test"];

const hook = {
  id: "wh-1",
  url: "https://hooks.example.com/storage",
  description: "",
  event_types: ["object.available", "dataset.snapshot_created"],
  enabled: true,
  created_by: "user-1",
  created_at: "2026-10-28T10:00:00Z",
  updated_at: "2026-10-28T10:00:00Z",
};

describe("WebhooksScreen", () => {
  it("adds an endpoint and shows its secret once", async () => {
    let list: (typeof hook)[] = [];
    let sent: unknown;
    server.use(
      http.get(`${API}/webhooks`, () => HttpResponse.json({ webhooks: list, event_types: EVENTS })),
      http.post(`${API}/webhooks`, async ({ request }) => {
        sent = await request.json();
        list = [hook];
        return HttpResponse.json({ ...hook, secret: "whsec_c2VjcmV0" }, { status: 201 });
      }),
      http.get(`${API}/webhooks/wh-1/deliveries`, () => HttpResponse.json({ deliveries: [] })),
    );
    renderWithProviders(<WebhooksScreen />);

    await userEvent.click(await screen.findByRole("button", { name: "Add endpoint" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/^URL/), "https://hooks.example.com/storage");
    // Every event but the test one is chosen by default.
    expect(within(dialog).getByRole("checkbox", { name: /webhook.test/ })).not.toBeChecked();
    await userEvent.click(within(dialog).getByRole("button", { name: "Add endpoint" }));

    expect(await screen.findByText("whsec_c2VjcmV0")).toBeInTheDocument();
    expect(sent).toEqual({ url: "https://hooks.example.com/storage", description: "", event_types: ["object.available", "dataset.snapshot_created"] });
    await userEvent.click(screen.getByRole("button", { name: "I have stored it" }));
    expect(screen.queryByText("whsec_c2VjcmV0")).not.toBeInTheDocument();
    expect(await screen.findByText("No deliveries yet")).toBeInTheDocument();
  });

  it("keeps the URL check in the form", async () => {
    server.use(http.get(`${API}/webhooks`, () => HttpResponse.json({ webhooks: [], event_types: EVENTS })));
    renderWithProviders(<WebhooksScreen />);
    await userEvent.click(await screen.findByRole("button", { name: "Add endpoint" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/^URL/), "hooks.example.com");
    expect(within(dialog).getByRole("button", { name: "Add endpoint" })).toBeDisabled();
  });

  it("hides changes from a role without org.manage", async () => {
    server.use(
      http.get(`${API}/webhooks`, () => HttpResponse.json({ webhooks: [hook], event_types: EVENTS })),
      http.get(`${API}/webhooks/wh-1/deliveries`, () => HttpResponse.json({ deliveries: [] })),
    );
    renderWithProviders(<WebhooksScreen />, { permissions: ["datasets.read"] });
    expect(await screen.findByText("No deliveries yet")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add endpoint" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rotate secret" })).not.toBeInTheDocument();
  });
});
