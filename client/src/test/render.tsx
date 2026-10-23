import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, type RenderOptions } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";
import { setActiveOrganization } from "@/lib/api/client";
import type { Membership, User } from "@/lib/api/types/identity";
import { SessionContext, type SessionValue } from "@/lib/auth/session";
import { PERMISSIONS, type Permission } from "@/lib/permissions";

export const testUser: User = {
  id: "user-1",
  email: "dev@example.test",
  display_name: "Dev",
  status: "ACTIVE",
  created_at: "2026-01-01T00:00:00Z",
};

type Options = {
  organizationId?: string | null;
  permissions?: readonly Permission[] | null;
  user?: Partial<User>;
  memberships?: Membership[];
};

export function makeSession(options: Options = {}): SessionValue {
  const organizationId = options.organizationId === undefined ? "org-1" : options.organizationId;
  const permissions = options.permissions === undefined ? PERMISSIONS : options.permissions;
  const set = permissions ? new Set<string>(permissions) : null;
  const can = (p: Permission) => (set ? set.has(p) : true);
  const user = { ...testUser, ...options.user };
  const membership: Membership | null = organizationId
    ? {
        organization_id: organizationId,
        organization_name: "Test organization",
        user_id: user.id,
        role_id: "developer",
        status: "ACTIVE",
        created_at: "2026-01-01T00:00:00Z",
      }
    : null;

  setActiveOrganization(organizationId);
  return {
    user,
    memberships: options.memberships ?? (membership ? [membership] : []),
    organizationId,
    membership,
    permissions: set,
    can,
    canAny: (list) => list.length === 0 || list.some(can),
    isPlatformAdmin: user.platform_role === "ADMIN",
    isPlatformAuditor: user.platform_role === "AUDITOR" || user.platform_role === "ADMIN",
    switchOrganization: () => undefined,
    signOut: async () => undefined,
  };
}

export function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
}

export function renderWithProviders(ui: ReactElement, options: Options & RenderOptions & { client?: QueryClient } = {}) {
  const { client = makeQueryClient(), organizationId, permissions, user, memberships, ...renderOptions } = options;
  const session = makeSession({ organizationId, permissions, user, memberships });

  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={client}>
        <SessionContext.Provider value={session}>{children}</SessionContext.Provider>
      </QueryClientProvider>
    );
  }
  return { client, session, ...render(ui, { wrapper: Wrapper, ...renderOptions }) };
}
