import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { axe } from "vitest-axe";
import { vi } from "vitest";
import { renderWithProviders } from "@/test/render";
import { API, server } from "@/test/server";
import type { ProviderCatalog, StorageConnection, TestResponse } from "../types";
import { ConnectionDetail } from "./connection-detail";
import { ConnectionsPage } from "./connections-page";
import { CreateConnection } from "./create-connection";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));

const catalog: ProviderCatalog = {
  byos_enabled: true,
  providers: [
    { provider: "PLATFORM_MANAGED", name: "Platform-managed storage", auth_methods: [{ method: "PLATFORM", secret_fields: [] }], required_fields: [], notes: [] },
    { provider: "AWS_S3", name: "Amazon S3", auth_methods: [{ method: "ACCESS_KEY", secret_fields: [] }, { method: "ASSUME_ROLE", secret_fields: [] }], required_fields: ["bucket", "region"], notes: [] },
    { provider: "CLOUDFLARE_R2", name: "Cloudflare R2", auth_methods: [{ method: "ACCESS_KEY", secret_fields: [] }], required_fields: ["bucket", "account_id"], notes: [] },
    { provider: "S3_COMPATIBLE", name: "S3-compatible service", auth_methods: [{ method: "ACCESS_KEY", secret_fields: [] }], required_fields: ["bucket", "endpoint"], notes: [] },
    { provider: "GOOGLE_CLOUD_STORAGE", name: "Google Cloud Storage", auth_methods: [{ method: "SERVICE_ACCOUNT_KEY", secret_fields: [] }], required_fields: ["bucket"], notes: [] },
    { provider: "AZURE_BLOB", name: "Azure Blob Storage", auth_methods: [{ method: "ACCOUNT_KEY", secret_fields: [] }, { method: "SAS_TOKEN", secret_fields: [] }], required_fields: ["bucket", "account_id"], notes: [] },
  ],
};

const platform: StorageConnection = {
  id: "platform",
  name: "Platform-managed storage",
  provider: "PLATFORM_MANAGED",
  status: "ACTIVE",
  auth_method: "PLATFORM",
  force_path_style: false,
  is_default: true,
  has_credentials: false,
  credential_rotation: 0,
  revision: 1,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function connection(overrides: Partial<StorageConnection> = {}): StorageConnection {
  return {
    ...platform,
    id: "sc_1",
    name: "Team bucket",
    provider: "S3_COMPATIBLE",
    status: "DISABLED",
    auth_method: "ACCESS_KEY",
    bucket: "team-bucket",
    endpoint: "https://s3.example.com",
    force_path_style: true,
    staging_prefix: "quarantine",
    ready_prefix: "assets",
    staging_mode: "CONNECTION",
    is_default: false,
    has_credentials: true,
    credential_rotation: 2,
    revision: 7,
    ...overrides,
  };
}

const failedTest: TestResponse = {
  ok: false,
  category: "ACCESS_DENIED",
  message: "the credentials are not allowed to do this",
  checks: [
    { name: "endpoint", ok: true },
    { name: "credentials", ok: false, category: "ACCESS_DENIED" },
  ],
  tested_at: "2026-02-01T10:00:00Z",
};

function useCatalog(byos = true) {
  server.use(http.get(`${API}/storage-providers`, () => HttpResponse.json({ ...catalog, byos_enabled: byos })));
}

async function pick(label: RegExp, option: string) {
  await userEvent.selectOptions(await screen.findByLabelText(label), option);
}

describe("create form", () => {
  it("asks only for the fields that apply to the provider and method", async () => {
    useCatalog();
    renderWithProviders(<CreateConnection />);

    await pick(/^provider/i, "CLOUDFLARE_R2");
    expect(await screen.findByLabelText(/^account id/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/^region/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/role arn/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/path-style/i)).not.toBeInTheDocument();

    await pick(/^provider/i, "S3_COMPATIBLE");
    expect(await screen.findByLabelText(/^endpoint/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/^account id/i)).not.toBeInTheDocument();
    expect(screen.getByLabelText(/path-style/i)).toBeInTheDocument();

    await pick(/^provider/i, "AWS_S3");
    await pick(/authentication method/i, "ASSUME_ROLE");
    expect(await screen.findByLabelText(/role arn/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/^region/i)).toBeInTheDocument();
    expect(screen.queryByLabelText(/^access key id/i)).not.toBeInTheDocument();
  });

  it("offers only creatable providers", async () => {
    useCatalog();
    renderWithProviders(<CreateConnection />);
    const select = await screen.findByLabelText(/^provider/i);
    expect(within(select).queryByText(/platform-managed/i)).not.toBeInTheDocument();
    expect(within(select).getByText("Amazon S3")).toBeInTheDocument();
  });

  it("forces platform staging for an Azure SAS token", async () => {
    useCatalog();
    renderWithProviders(<CreateConnection />);
    await pick(/^provider/i, "AZURE_BLOB");
    await pick(/authentication method/i, "SAS_TOKEN");
    const staging = await screen.findByLabelText(/staging mode/i);
    expect(staging).toHaveValue("PLATFORM");
    expect(staging).toBeDisabled();
    expect(screen.getByLabelText(/^container/i)).toBeInTheDocument();
  });

  it("applies the documented validation rules", async () => {
    useCatalog();
    const post = vi.fn();
    server.use(http.post(`${API}/storage-connections`, () => post()));
    renderWithProviders(<CreateConnection />);
    const user = userEvent.setup();

    await pick(/^provider/i, "S3_COMPATIBLE");
    const submit = screen.getByRole("button", { name: /create connection/i });
    expect(submit).toBeDisabled();
    await user.click(await screen.findByLabelText(/^name/i));
    await user.tab();
    await user.type(screen.getByLabelText(/^bucket/i), "Bad..Bucket");
    await user.type(screen.getByLabelText(/^endpoint/i), "https://s3.example.com/path");
    await user.clear(screen.getByLabelText(/ready prefix/i));
    await user.type(screen.getByLabelText(/ready prefix/i), "quarantine");
    await user.tab();

    expect((await screen.findAllByText(/use 1 to 128 characters/i)).length).toBeGreaterThan(0);
    expect(submit).toBeDisabled();
    expect(screen.getByText(/bucket names are 3 to 222/i)).toBeInTheDocument();
    expect(screen.getByText(/no path, query or fragment/i)).toBeInTheDocument();
    expect(screen.getByText(/prefixes must be different/i)).toBeInTheDocument();
    expect(post).not.toHaveBeenCalled();
  });

  it("keeps Create connection disabled until valid and counts the name", async () => {
    useCatalog();
    renderWithProviders(<CreateConnection />);
    const user = userEvent.setup();
    await pick(/^provider/i, "AWS_S3");
    await pick(/authentication method/i, "ACCESS_KEY");
    const submit = screen.getByRole("button", { name: /create connection/i });
    expect(submit).toBeDisabled();
    await user.type(screen.getByLabelText(/^name/i), "Prod");
    expect(screen.getByText("4 / 128")).toBeInTheDocument();
    await user.type(screen.getByLabelText(/^bucket/i), "  My-Bucket ");
    await user.type(screen.getByLabelText(/access key id/i), "AKIA");
    await user.type(screen.getByLabelText(/secret access key/i), "secret");
    expect(submit).toBeEnabled();
    expect(screen.getAllByRole("button", { name: /show password/i }).length).toBeGreaterThan(0);
  });

  it("requires region for AWS and a role ARN in the documented format", async () => {
    useCatalog();
    renderWithProviders(<CreateConnection />);
    const user = userEvent.setup();
    await pick(/^provider/i, "AWS_S3");
    await pick(/authentication method/i, "ASSUME_ROLE");
    expect(await screen.findByLabelText(/^region/i)).toHaveValue("us-east-1");
    await user.clear(screen.getByLabelText(/^region/i));
    await user.type(screen.getByLabelText(/role arn/i), "not-an-arn");
    await user.tab();
    expect(await screen.findByText(/enter the region/i)).toBeInTheDocument();
    expect(screen.getByText(/enter a role arn such as/i)).toBeInTheDocument();
  });

  it("sends only applicable fields, clears secrets and offers the test step", async () => {
    useCatalog();
    let sent: Record<string, unknown> | null = null;
    server.use(
      http.post(`${API}/storage-connections`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(connection({ status: "PENDING", name: "Team bucket" }), { status: 201 });
      }),
    );
    renderWithProviders(<CreateConnection />);
    const user = userEvent.setup();

    await pick(/^provider/i, "S3_COMPATIBLE");
    await user.type(await screen.findByLabelText(/^name/i), "Team bucket");
    await user.type(screen.getByLabelText(/^bucket/i), "team-bucket");
    await user.type(screen.getByLabelText(/^endpoint/i), "https://s3.example.com");
    await user.type(screen.getByLabelText(/^access key id/i), "AKIAEXAMPLE");
    await user.type(screen.getByLabelText(/^secret access key/i), "super-secret-value");
    await user.click(screen.getByRole("button", { name: /create connection/i }));

    expect(await screen.findByRole("heading", { name: /test the connection/i })).toBeInTheDocument();
    expect(sent).toMatchObject({
      provider: "S3_COMPATIBLE",
      auth_method: "ACCESS_KEY",
      credentials: { access_key_id: "AKIAEXAMPLE", secret_access_key: "super-secret-value" },
    });
    expect(sent).not.toHaveProperty("account_id");
    expect(sent).not.toHaveProperty("role_arn");
    expect(document.body.innerHTML).not.toContain("super-secret-value");
    expect(document.body.innerHTML).not.toContain("AKIAEXAMPLE");
  });

  it("clears the secret fields when the backend refuses the connection", async () => {
    useCatalog();
    server.use(
      http.post(`${API}/storage-connections`, () =>
        HttpResponse.json({ error: "a storage connection with that name exists", code: "conflict" }, { status: 409 }),
      ),
    );
    renderWithProviders(<CreateConnection />);
    const user = userEvent.setup();

    await pick(/^provider/i, "CLOUDFLARE_R2");
    await user.type(await screen.findByLabelText(/^name/i), "Team");
    await user.type(screen.getByLabelText(/^bucket/i), "team-bucket");
    await user.type(screen.getByLabelText(/^account id/i), "abc123");
    await user.type(screen.getByLabelText(/^access key id/i), "AKIAEXAMPLE");
    await user.type(screen.getByLabelText(/^secret access key/i), "super-secret-value");
    await user.click(screen.getByRole("button", { name: /create connection/i }));

    expect(await screen.findByText(/name already exists/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/^secret access key/i)).toHaveValue("");
    expect(screen.getByLabelText(/^access key id/i)).toHaveValue("");
  });

  it("shows the external ID for an assumed role", async () => {
    useCatalog();
    server.use(
      http.post(`${API}/storage-connections`, () =>
        HttpResponse.json(
          connection({ provider: "AWS_S3", auth_method: "ASSUME_ROLE", status: "PENDING", external_id: "osp-abc123", role_arn: "arn:aws:iam::123456789012:role/x", has_credentials: false }),
          { status: 201 },
        ),
      ),
    );
    renderWithProviders(<CreateConnection />);
    const user = userEvent.setup();
    await pick(/^provider/i, "AWS_S3");
    await pick(/authentication method/i, "ASSUME_ROLE");
    await user.type(await screen.findByLabelText(/^name/i), "Role bucket");
    await user.type(screen.getByLabelText(/^bucket/i), "role-bucket");
    await user.type(screen.getByLabelText(/^region/i), "eu-west-1");
    await user.type(screen.getByLabelText(/role arn/i), "arn:aws:iam::123456789012:role/storage");
    await user.click(screen.getByRole("button", { name: /create connection/i }));
    expect(await screen.findByText("osp-abc123")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /copy external id/i })).toBeInTheDocument();
  });

  it("disables creation when customer connections are off", async () => {
    useCatalog(false);
    renderWithProviders(<CreateConnection />);
    expect((await screen.findAllByText(/customer connections are disabled/i)).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: /create connection/i })).toBeDisabled();
  });

  it("has no accessibility violations", async () => {
    useCatalog();
    const { container } = renderWithProviders(<CreateConnection />);
    const user = userEvent.setup();
    await pick(/^provider/i, "AWS_S3");
    await pick(/authentication method/i, "ACCESS_KEY");
    await user.clear(await screen.findByLabelText(/^region/i));
    await user.tab();
    await screen.findByText(/enter the region/i);
    expect(await axe(container)).toHaveNoViolations();
  });
});

describe("list", () => {
  it("marks the platform connection and teaches when it is the only one", async () => {
    useCatalog();
    server.use(
      http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [platform], default_connection_id: "platform" })),
      http.get(`${API}/storage-providers/capabilities`, () => HttpResponse.json({ providers: [] })),
    );
    renderWithProviders(<ConnectionsPage />);
    expect(await screen.findByText("Platform-managed")).toBeInTheDocument();
    expect(screen.getByText(/only platform-managed storage so far/i)).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: /add connection/i })).toBeInTheDocument();
  });

  it("hides Add connection without storage.manage", async () => {
    useCatalog();
    server.use(
      http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [platform], default_connection_id: "platform" })),
      http.get(`${API}/storage-providers/capabilities`, () => HttpResponse.json({ providers: [] })),
    );
    renderWithProviders(<ConnectionsPage />, { permissions: ["storage.read"] });
    await screen.findByText("Platform-managed");
    expect(screen.queryByRole("link", { name: /add connection/i })).not.toBeInTheDocument();
  });

  it("disables Add connection when customer connections are off", async () => {
    useCatalog(false);
    server.use(
      http.get(`${API}/storage-connections`, () => HttpResponse.json({ connections: [platform], default_connection_id: "platform" })),
      http.get(`${API}/storage-providers/capabilities`, () => HttpResponse.json({ providers: [] })),
    );
    renderWithProviders(<ConnectionsPage />);
    expect(await screen.findByText(/disabled on this deployment/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add connection/i })).toBeDisabled();
  });
});

describe("detail", () => {
  function serve(c: StorageConnection) {
    server.use(http.get(`${API}/storage-connections/${c.id}`, () => HttpResponse.json(c)));
  }

  it("renders a 200 test with ok:false as a failure", async () => {
    const c = connection({ status: "ACTIVE" });
    serve(c);
    server.use(http.post(`${API}/storage-connections/sc_1/test`, () => HttpResponse.json(failedTest)));
    renderWithProviders(<ConnectionDetail id="sc_1" />);
    await userEvent.click(await screen.findByRole("button", { name: /test connection/i }));
    expect(await screen.findByText("Test failed")).toBeInTheDocument();
    expect(screen.queryByText("Test passed")).not.toBeInTheDocument();
    expect(screen.getByText(/checks stop at the first failure/i)).toBeInTheDocument();
  });

  it("keeps a disabled connection disabled when enabling fails the test", async () => {
    serve(connection());
    server.use(http.post(`${API}/storage-connections/sc_1/enable`, () => HttpResponse.json(failedTest)));
    renderWithProviders(<ConnectionDetail id="sc_1" />);
    await userEvent.click(await screen.findByRole("button", { name: "Enable" }));
    expect(await screen.findByText("Test failed")).toBeInTheDocument();
    expect(screen.getByText(/stays disabled/i)).toBeInTheDocument();
  });

  it("clears the secret fields after a refused rotation and says the old credentials stay", async () => {
    serve(connection({ status: "ACTIVE" }));
    server.use(http.post(`${API}/storage-connections/sc_1/rotate-credentials`, () => HttpResponse.json(failedTest, { status: 422 })));
    renderWithProviders(<ConnectionDetail id="sc_1" />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: /rotate credentials/i }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByLabelText(/^secret access key/i)).toHaveValue("");
    await user.type(within(dialog).getByLabelText(/^access key id/i), "AKIANEW");
    await user.type(within(dialog).getByLabelText(/^secret access key/i), "new-secret-value");
    await user.click(within(dialog).getByRole("button", { name: /^rotate credentials$/i }));

    expect(await within(dialog).findByText(/old credentials are still in use/i)).toBeInTheDocument();
    expect(within(dialog).getByLabelText(/^secret access key/i)).toHaveValue("");
    expect(within(dialog).getByLabelText(/^access key id/i)).toHaveValue("");
    expect(document.body.innerHTML).not.toContain("new-secret-value");
  });

  it("explains why a connection cannot be deleted", async () => {
    serve(connection());
    server.use(
      http.delete(`${API}/storage-connections/sc_1`, () =>
        HttpResponse.json(
          { error: "in use", code: "connection_in_use", active_uploads: 2, live_assets: 14, datasets: 1, is_default: false },
          { status: 409 },
        ),
      ),
    );
    renderWithProviders(<ConnectionDetail id="sc_1" />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("alertdialog");
    await user.type(within(dialog).getByRole("textbox"), "Team bucket");
    await user.click(within(dialog).getByRole("button", { name: /delete connection/i }));
    const message = await within(dialog).findByRole("alert");
    expect(message).toHaveTextContent(/2 unfinished uploads/);
    expect(message).toHaveTextContent(/14 assets stored on it/);
    expect(message).toHaveTextContent(/1 dataset that upload to it/);
    expect(message).toHaveTextContent(/never moved to another provider/i);
    expect(message).toHaveTextContent(/never touches the contents of your bucket/i);
  });

  it("tells the user to set another default before disabling the default", async () => {
    serve(connection({ status: "ACTIVE", is_default: true }));
    server.use(
      http.post(`${API}/storage-connections/sc_1/disable`, () =>
        HttpResponse.json({ error: "it is the organization's default", code: "connection_in_use" }, { status: 409 }),
      ),
    );
    renderWithProviders(<ConnectionDetail id="sc_1" />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Disable" }));
    const dialog = await screen.findByRole("alertdialog");
    await user.click(within(dialog).getByRole("button", { name: /disable connection/i }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(/set another connection as the default first/i);
  });

  it("reloads and explains a revision conflict on edit", async () => {
    let reads = 0;
    server.use(
      http.get(`${API}/storage-connections/sc_1`, () => {
        reads += 1;
        return HttpResponse.json(connection({ status: "PENDING", revision: reads > 1 ? 8 : 7 }));
      }),
      http.patch(`${API}/storage-connections/sc_1`, () =>
        HttpResponse.json({ error: "the connection changed", code: "revision_conflict" }, { status: 412 }),
      ),
    );
    renderWithProviders(<ConnectionDetail id="sc_1" />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog");
    await user.clear(within(dialog).getByLabelText(/^name/i));
    await user.type(within(dialog).getByLabelText(/^name/i), "Renamed");
    await user.click(within(dialog).getByRole("button", { name: /save changes/i }));
    expect(await screen.findByText(/someone else changed this connection/i)).toBeInTheDocument();
    await waitFor(() => expect(reads).toBeGreaterThan(1));
  });

  it("shows the refused settings when an edit fails the re-test, and says nothing was stored", async () => {
    serve(connection({ status: "ACTIVE" }));
    let body: Record<string, unknown> | null = null;
    server.use(
      http.patch(`${API}/storage-connections/sc_1`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(failedTest, { status: 422 });
      }),
    );
    renderWithProviders(<ConnectionDetail id="sc_1" />);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog");
    const endpoint = within(dialog).getByLabelText(/^endpoint/i);
    await user.clear(endpoint);
    await user.type(endpoint, "https://other.example.com");
    expect(within(dialog).getByText(/re-tests this active connection/i)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: /save changes/i }));
    expect(await within(dialog).findByText(/nothing was stored/i)).toBeInTheDocument();
    expect(body).toEqual({ revision: 7, endpoint: "https://other.example.com" });
  });

  it("hides management actions without storage.manage", async () => {
    serve(connection({ status: "ACTIVE" }));
    renderWithProviders(<ConnectionDetail id="sc_1" />, { permissions: ["storage.read"] });
    await screen.findByRole("heading", { name: "Team bucket" });
    for (const name of ["Edit", "Disable", "Test connection", "Rotate credentials", "Set as default"]) {
      expect(screen.queryByRole("button", { name })).not.toBeInTheDocument();
    }
  });

  it("never shows a secret and only offers set-default on the platform connection", async () => {
    serve({ ...platform, is_default: false });
    renderWithProviders(<ConnectionDetail id="platform" />);
    expect(await screen.findByRole("button", { name: "Set as default" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
  });
});
