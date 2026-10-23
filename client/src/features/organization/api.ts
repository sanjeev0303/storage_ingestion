import { accountApi, api } from "@/lib/api/client";
import type { CursorPage } from "@/lib/query/paged";
import type { CredentialToken, Invitation, Membership, Organization, OrganizationUpdate, Quota, Role, User } from "./types";

export const PAGE_SIZE = 50;

export const getOrganization = (signal?: AbortSignal) => api<Organization>("/organization", { signal });

export const updateOrganization = (body: OrganizationUpdate) =>
  api<Organization>("/organization", { method: "PATCH", body });

export const deactivateOrganization = () => api<Organization>("/organization/deactivate", { method: "POST" });

export const getQuota = (signal?: AbortSignal) => api<Quota>("/organization/quota", { signal });

export const getRoles = (signal?: AbortSignal) => api<{ roles: Role[] }>("/organization/roles", { signal });

export type RoleInput = { name: string; description?: string; permissions: string[] };

export const createRole = (body: RoleInput) => api<Role>("/organization/roles", { method: "POST", body });

export const updateRole = (id: string, body: RoleInput) =>
  api<Role>(`/organization/roles/${id}`, { method: "PUT", body });

export const deleteRole = (id: string) => api<void>(`/organization/roles/${id}`, { method: "DELETE" });

// Members are paged by the last user_id; a full page means there may be more.
export async function getMembersPage(after: string | undefined, signal?: AbortSignal): Promise<CursorPage<Membership, string>> {
  const res = await api<{ members: Membership[] }>("/organization/members", {
    query: { after, limit: PAGE_SIZE },
    signal,
  });
  const items = res.members ?? [];
  return { items, next: items.length === PAGE_SIZE ? items[items.length - 1].user_id : undefined };
}

const memberPath = (userId: string) => `/organization/members/${encodeURIComponent(userId)}`;

export const changeMemberRole = (userId: string, roleId: string) =>
  api<Membership>(`${memberPath(userId)}/role`, { method: "PUT", body: { role_id: roleId } });

export const suspendMember = (userId: string) => api<Membership>(`${memberPath(userId)}/suspend`, { method: "POST" });

export const reactivateMember = (userId: string) =>
  api<Membership>(`${memberPath(userId)}/reactivate`, { method: "POST" });

export const removeMember = (userId: string) => api<void>(memberPath(userId), { method: "DELETE" });

export async function getInvitationsPage(after: string | undefined, signal?: AbortSignal): Promise<CursorPage<Invitation, string>> {
  const res = await api<{ invitations: Invitation[] }>("/organization/invitations", {
    query: { after, limit: PAGE_SIZE },
    signal,
  });
  const items = res.invitations ?? [];
  return { items, next: items.length === PAGE_SIZE ? items[items.length - 1].id : undefined };
}

export const createInvitation = (body: { email: string; role_id: string }) =>
  api<Invitation>("/organization/invitations", { method: "POST", body });

export const revokeInvitation = (id: string) => api<void>(`/organization/invitations/${id}`, { method: "DELETE" });

// Platform administration. Account routes: no organization header.
export async function getAdminOrganizationsPage(after: string | undefined, signal?: AbortSignal): Promise<CursorPage<Organization, string>> {
  const res = await accountApi<{ organizations: Organization[] }>("/admin/organizations", {
    query: { after, limit: PAGE_SIZE },
    signal,
  });
  const items = res.organizations ?? [];
  return { items, next: items.length === PAGE_SIZE ? items[items.length - 1].id : undefined };
}

export const createAdminOrganization = (body: { name: string; slug: string; owner_user_id: string }) =>
  accountApi<Organization>("/admin/organizations", { method: "POST", body });

export const setAdminOrganizationStatus = (id: string, status: string) =>
  accountApi<Organization>(`/admin/organizations/${id}/status`, { method: "PUT", body: { status } });

export const getAdminQuota = (id: string, signal?: AbortSignal) =>
  accountApi<Quota>(`/admin/organizations/${id}/quota`, { signal });

export const setAdminQuota = (id: string, limits: Record<string, number>) =>
  accountApi<Quota>(`/admin/organizations/${id}/quota`, { method: "PUT", body: limits });

export async function getAdminUsersPage(after: string | undefined, signal?: AbortSignal): Promise<CursorPage<User, string>> {
  const res = await accountApi<{ users: User[] }>("/admin/users", { query: { after, limit: PAGE_SIZE }, signal });
  const items = res.users ?? [];
  return { items, next: items.length === PAGE_SIZE ? items[items.length - 1].id : undefined };
}

export const createAdminUser = (body: { email: string; display_name?: string; platform_role?: string }) =>
  accountApi<{ user: User; activation: CredentialToken }>("/admin/users", { method: "POST", body });

export const deactivateAdminUser = (id: string) => accountApi<User>(`/admin/users/${id}/deactivate`, { method: "POST" });

export const reactivateAdminUser = (id: string) => accountApi<User>(`/admin/users/${id}/reactivate`, { method: "POST" });

export const setAdminUserPlatformRole = (id: string, platform_role: string) =>
  accountApi<void>(`/admin/users/${id}/platform-role`, { method: "PUT", body: { platform_role } });

export const issueAdminPasswordReset = (id: string) =>
  accountApi<CredentialToken>(`/admin/users/${id}/password-reset`, { method: "POST" });
