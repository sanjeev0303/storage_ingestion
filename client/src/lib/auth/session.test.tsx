import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/lib/api/client";
import { orgKey } from "@/lib/query/keys";
import { API, server } from "@/test/server";
import { SessionProvider, useSession } from "./session";

const replace = vi.fn();
const push = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace, push }),
  usePathname: () => "/app/catalog",
}));

const user = { id: "u1", email: "a@example.test", status: "ACTIVE", created_at: "2026-01-01T00:00:00Z" };
const membership = (id: string, name: string) => ({
  organization_id: id,
  organization_name: name,
  user_id: "u1",
  role_id: "developer",
  status: "ACTIVE",
  created_at: "2026-01-01T00:00:00Z",
});

function useBackend(extra?: () => void) {
  server.use(
    http.get(`${API}/auth/me`, () =>
      HttpResponse.json({ user, memberships: [membership("org-a", "Alpha"), membership("org-b", "Beta")] }),
    ),
    http.get(`${API}/organization/roles`, () =>
      HttpResponse.json({ roles: [{ id: "developer", name: "Developer", built_in: true, permissions: ["assets.read"] }] }),
    ),
  );
  extra?.();
}

function Probe() {
  const session = useSession();
  const objects = useQuery({
    queryKey: orgKey(session.organizationId, "objects"),
    queryFn: () => api<{ owner: string }>("/catalog/objects"),
    enabled: Boolean(session.organizationId),
  });
  return (
    <div>
      <p data-testid="org">{session.organizationId ?? "none"}</p>
      <p data-testid="owner">{objects.data?.owner ?? "empty"}</p>
      <p data-testid="can-delete">{String(session.can("assets.delete"))}</p>
      <p data-testid="can-read">{String(session.can("assets.read"))}</p>
      <button onClick={() => session.switchOrganization("org-b")}>switch</button>
      <button onClick={() => objects.refetch()}>reload</button>
    </div>
  );
}

function mount(client: QueryClient) {
  return render(
    <QueryClientProvider client={client}>
      <SessionProvider fallback={<p>loading</p>}>
        <Probe />
      </SessionProvider>
    </QueryClientProvider>,
  );
}

const newClient = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });

beforeEach(() => {
  replace.mockClear();
  push.mockClear();
  sessionStorage.clear();
});

describe("session and organization scoping", () => {
  it("asks for an organization instead of guessing when the user belongs to several", async () => {
    useBackend();
    mount(newClient());
    await waitFor(() => expect(screen.getByTestId("org")).toHaveTextContent("none"));
  });

  it("derives permissions from the role and treats unknown ones as denied only when they are known", async () => {
    useBackend();
    sessionStorage.setItem("osp.organization", "org-a");
    mount(newClient());
    // Until the role loads, can() allows everything; after it loads, only the role's permissions.
    await waitFor(() => expect(screen.getByTestId("can-delete")).toHaveTextContent("false"));
    expect(screen.getByTestId("can-read")).toHaveTextContent("true");
  });

  it("drops every cached answer for the old organization when switching, and sends the new header", async () => {
    const headers: (string | null)[] = [];
    useBackend(() =>
      server.use(
        http.get(`${API}/catalog/objects`, ({ request }) => {
          const org = request.headers.get("x-organization-id");
          headers.push(org);
          return HttpResponse.json({ owner: `data for ${org}` });
        }),
      ),
    );
    sessionStorage.setItem("osp.organization", "org-a");
    const client = newClient();
    mount(client);

    await waitFor(() => expect(screen.getByTestId("owner")).toHaveTextContent("data for org-a"));
    expect(client.getQueryData(orgKey("org-a", "objects"))).toBeDefined();

    await userEvent.click(screen.getByText("switch"));

    // Nothing of Alpha's survives under the org namespace.
    expect(client.getQueryData(orgKey("org-a", "objects"))).toBeUndefined();
    await waitFor(() => expect(screen.getByTestId("owner")).toHaveTextContent("data for org-b"));
    expect(headers).toEqual(["org-a", "org-b"]);
    expect(push).toHaveBeenCalledWith("/app/overview");
  });

  it("clears all cached data and goes to sign in when the session ends", async () => {
    useBackend(() =>
      server.use(
        http.get(`${API}/catalog/objects`, () => HttpResponse.json({ owner: "secret" })),
      ),
    );
    sessionStorage.setItem("osp.organization", "org-a");
    const client = newClient();
    mount(client);
    await waitFor(() => expect(screen.getByTestId("owner")).toHaveTextContent("secret"));

    server.use(
      http.get(`${API}/catalog/objects`, () => HttpResponse.json({ error: "x", code: "unauthenticated" }, { status: 401 })),
    );
    await act(async () => {
      await userEvent.click(screen.getByText("reload"));
    });

    await waitFor(() => expect(replace).toHaveBeenCalledWith("/login?next=%2Fapp%2Fcatalog"));
    expect(client.getQueryCache().getAll()).toHaveLength(0);
  });
});
