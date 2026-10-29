import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { API, server } from "@/test/server";
import { renderWithProviders } from "@/test/render";
import { SsoScreen } from "./sso-screen";

const saml = { entity_id: "urn:object-storage:sp:org-1", acs_url: "http://localhost:3100/api/session/saml/acs" };

const configured = {
  configured: true,
  protocol: "OIDC",
  issuer: "https://login.example.com",
  client_id: "console",
  has_idp_metadata: false,
  allowed_domains: ["example.com"],
  default_role: "",
  enabled: true,
  updated_at: "2026-10-28T10:00:00Z",
  redirect_uris: ["http://localhost:3100/api/session/sso/callback"],
  saml,
};

const blank = {
  configured: false,
  has_idp_metadata: false,
  allowed_domains: [],
  default_role: "",
  enabled: false,
  redirect_uris: ["http://localhost:3100/api/session/sso/callback"],
  saml,
};

const noToken = () => http.get(`${API}/organization/scim-token`, () => HttpResponse.json({ enabled: false }));

describe("SsoScreen", () => {
  it("saves without the secret, which is never filled in again", async () => {
    let sent: Record<string, unknown> | undefined;
    server.use(
      http.get(`${API}/organization/sso`, () => HttpResponse.json(configured)),
      http.get(`${API}/organization/roles`, () => HttpResponse.json({ roles: [] })),
      noToken(),
      http.put(`${API}/organization/sso`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(configured);
      }),
    );
    renderWithProviders(<SsoScreen />);

    expect(await screen.findByText("http://localhost:3100/api/session/sso/callback")).toBeInTheDocument();
    expect(screen.getByLabelText(/Client secret/)).toHaveValue("");
    await userEvent.type(screen.getByLabelText(/Email domains/), ", example.org");
    await userEvent.click(screen.getByRole("button", { name: "Save and check the provider" }));

    await waitFor(() => expect(sent).toBeDefined());
    expect(sent).toMatchObject({ protocol: "OIDC", client_secret: "", allowed_domains: ["example.com", "example.org"], enabled: true });
  });

  it("needs a secret the first time", async () => {
    server.use(
      http.get(`${API}/organization/sso`, () => HttpResponse.json(blank)),
      http.get(`${API}/organization/roles`, () => HttpResponse.json({ roles: [] })),
      noToken(),
    );
    renderWithProviders(<SsoScreen />);
    await userEvent.type(await screen.findByLabelText(/^Issuer/), "https://login.example.com");
    await userEvent.type(screen.getByLabelText(/^Client ID/), "console");
    await userEvent.type(screen.getByLabelText(/Email domains/), "example.com");
    expect(screen.getByRole("button", { name: "Turn on single sign-on" })).toBeDisabled();
    await userEvent.type(screen.getByLabelText(/Client secret/), "shh");
    expect(screen.getByRole("button", { name: "Turn on single sign-on" })).toBeEnabled();
  });

  it("sets up SAML with the provider's metadata and shows what to register there", async () => {
    let sent: Record<string, unknown> | undefined;
    server.use(
      http.get(`${API}/organization/sso`, () => HttpResponse.json(blank)),
      http.get(`${API}/organization/roles`, () => HttpResponse.json({ roles: [] })),
      noToken(),
      http.put(`${API}/organization/sso`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ ...configured, protocol: "SAML", has_idp_metadata: true });
      }),
    );
    renderWithProviders(<SsoScreen />);

    await userEvent.click(await screen.findByRole("radio", { name: /SAML 2.0/ }));
    expect(screen.getByText(saml.entity_id)).toBeInTheDocument();
    expect(screen.getByText(saml.acs_url)).toBeInTheDocument();
    expect(screen.queryByLabelText(/Client secret/)).not.toBeInTheDocument();

    await userEvent.type(screen.getByLabelText(/Email domains/), "example.com");
    const save = screen.getByRole("button", { name: "Turn on single sign-on" });
    expect(save).toBeDisabled();
    await userEvent.click(screen.getByLabelText(/Identity provider metadata/));
    await userEvent.paste("<EntityDescriptor entityID='https://idp.example.com'/>");
    await userEvent.click(save);

    await waitFor(() => expect(sent).toBeDefined());
    expect(sent).toEqual({
      protocol: "SAML",
      idp_metadata: "<EntityDescriptor entityID='https://idp.example.com'/>",
      allowed_domains: ["example.com"],
      default_role: "",
      enabled: true,
    });
  });

  it("shows a new SCIM token once", async () => {
    let enabled = false;
    server.use(
      http.get(`${API}/organization/sso`, () => HttpResponse.json(configured)),
      http.get(`${API}/organization/roles`, () => HttpResponse.json({ roles: [] })),
      http.get(`${API}/organization/scim-token`, () =>
        HttpResponse.json(enabled ? { enabled, created_at: "2026-10-28T10:00:00Z", last_used_at: null } : { enabled }),
      ),
      http.post(`${API}/organization/scim-token`, () => {
        enabled = true;
        return HttpResponse.json({ token: "sst_abc123" }, { status: 201 });
      }),
    );
    renderWithProviders(<SsoScreen />);

    expect(await screen.findByText(`${window.location.origin}/scim/v2`)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Make a token" }));
    expect(await screen.findByText("sst_abc123")).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Replace token" })).toBeInTheDocument();
    expect(screen.getByText(/Last used/)).toHaveTextContent("never");

    await userEvent.click(screen.getByRole("button", { name: "Hide the token" }));
    expect(screen.queryByText("sst_abc123")).not.toBeInTheDocument();
  });
});
