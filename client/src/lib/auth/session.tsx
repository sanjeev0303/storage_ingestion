"use client";

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { usePathname, useRouter } from "next/navigation";
import { createContext, useCallback, useContext, useEffect, useMemo, useSyncExternalStore, type ReactNode } from "react";
import { accountApi, api, onUnauthenticated, setActiveOrganization } from "@/lib/api/client";
import type { Me, Membership, Role, User } from "@/lib/api/types/identity";
import type { Permission } from "@/lib/permissions";
import { accountKey, orgKey } from "@/lib/query/keys";
import { safeNext } from "./redirect";

const ORG_STORAGE_KEY = "osp.organization";

// The chosen organization is per tab, so two tabs can work in different ones.
// It is an id, not a secret.
function readStoredOrganization() {
  try {
    return sessionStorage.getItem(ORG_STORAGE_KEY);
  } catch {
    return null;
  }
}

export function storeOrganization(id: string | null) {
  try {
    if (id) sessionStorage.setItem(ORG_STORAGE_KEY, id);
    else sessionStorage.removeItem(ORG_STORAGE_KEY);
  } catch {
    // storage can be blocked; the choice then lasts until reload
  }
  for (const listener of organizationListeners) listener();
}

const organizationListeners = new Set<() => void>();
function subscribeToOrganization(listener: () => void) {
  organizationListeners.add(listener);
  return () => {
    organizationListeners.delete(listener);
  };
}

// Set once the session has ended so several failing requests redirect only once.
let leavingSession = false;

export type SessionValue = {
  user: User;
  memberships: Membership[];
  organizationId: string | null;
  membership: Membership | null;
  permissions: ReadonlySet<string> | null;
  // True when the user holds the permission, or when permissions could not be
  // read: the backend still answers 403 for anything not allowed.
  can: (permission: Permission) => boolean;
  canAny: (permissions: Permission[]) => boolean;
  isPlatformAdmin: boolean;
  isPlatformAuditor: boolean;
  switchOrganization: (id: string) => void;
  signOut: () => Promise<void>;
};

export const SessionContext = createContext<SessionValue | null>(null);

export function useSession() {
  const value = useContext(SessionContext);
  if (!value) throw new Error("useSession must be used inside SessionProvider");
  return value;
}

export function useOrganizationId() {
  return useSession().organizationId;
}

export function SessionProvider({ children, fallback }: { children: ReactNode; fallback: ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const client = useQueryClient();
  const chosen = useSyncExternalStore(subscribeToOrganization, readStoredOrganization, () => null);

  useEffect(() => {
    leavingSession = false;
  }, []);

  const me = useQuery({
    queryKey: accountKey("me"),
    queryFn: ({ signal }) => accountApi<Me>("/auth/me", { signal }),
    retry: false,
    staleTime: 60_000,
  });

  const active = useMemo(
    () => me.data?.memberships.filter((m) => m.status === "ACTIVE") ?? [],
    [me.data],
  );

  const organizationId = useMemo(() => {
    if (chosen && active.some((m) => m.organization_id === chosen)) return chosen;
    if (active.length === 1) return active[0].organization_id;
    return null;
  }, [chosen, active]);

  // Set while rendering so child queries, which run after this render, always
  // send the organization this render shows.
  setActiveOrganization(organizationId);

  const membership = active.find((m) => m.organization_id === organizationId) ?? null;

  const roles = useQuery({
    queryKey: orgKey(organizationId, "roles"),
    queryFn: ({ signal }) => api<{ roles: Role[] }>("/organization/roles", { signal }),
    enabled: Boolean(organizationId),
    retry: false,
    staleTime: 5 * 60_000,
  });

  const permissions = useMemo(() => {
    if (!membership || !roles.data) return null;
    const role = roles.data.roles.find((r) => r.id === membership.role_id);
    return role ? new Set(role.permissions) : null;
  }, [membership, roles.data]);

  const leaveToLogin = useCallback(() => {
    if (leavingSession) return;
    leavingSession = true;
    client.clear();
    setActiveOrganization(null);
    const next = pathname && pathname.startsWith("/app") ? `?next=${encodeURIComponent(pathname)}` : "";
    router.replace(`/login${next}`);
  }, [client, pathname, router]);

  useEffect(() => onUnauthenticated(leaveToLogin), [leaveToLogin]);

  const switchOrganization = useCallback(
    (id: string) => {
      void client.cancelQueries({ queryKey: ["org"] });
      client.removeQueries({ queryKey: ["org"] });
      storeOrganization(id);
      router.push("/app/overview");
    },
    [client, router],
  );

  const signOut = useCallback(async () => {
    leavingSession = true;
    await fetch("/api/session/logout", { method: "POST", credentials: "same-origin" }).catch(() => undefined);
    client.clear();
    setActiveOrganization(null);
    storeOrganization(null);
    router.replace("/login");
  }, [client, router]);

  const value = useMemo<SessionValue | null>(() => {
    if (!me.data) return null;
    const user = me.data.user;
    const can = (permission: Permission) => (permissions ? permissions.has(permission) : true);
    return {
      user,
      memberships: active,
      organizationId,
      membership,
      permissions,
      can,
      canAny: (list) => list.length === 0 || list.some(can),
      isPlatformAdmin: user.platform_role === "ADMIN",
      isPlatformAuditor: user.platform_role === "AUDITOR" || user.platform_role === "ADMIN",
      switchOrganization,
      signOut,
    };
  }, [me.data, active, organizationId, membership, permissions, switchOrganization, signOut]);

  if (me.isError) {
    // 401 is handled by the listener above; anything else is a real failure.
    if (me.error && (me.error as { status?: number }).status !== 401) {
      return <SessionError onRetry={() => me.refetch()} />;
    }
    return <>{fallback}</>;
  }
  if (!value) return <>{fallback}</>;
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

function SessionError({ onRetry }: { onRetry: () => void }) {
  return (
    <main className="mx-auto flex min-h-dvh max-w-md flex-col justify-center gap-4 p-6">
      <h1 className="text-xl font-semibold">The console could not load your account</h1>
      <p className="text-muted">The storage API did not answer. Check that it is running, then try again.</p>
      <button
        type="button"
        onClick={onRetry}
        className="neo-sm w-fit px-4 py-2 font-medium transition-shadow active:shadow-neo-in"
      >
        Try again
      </button>
    </main>
  );
}

export { safeNext };
