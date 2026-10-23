import type { Role } from "./types";

// The server refuses to grant or manage anything beyond your own permissions.
// This only hides what would be refused; unknown permissions allow everything.
export function holdsAll(permissions: ReadonlySet<string> | null, role: Role | undefined) {
  if (!permissions || !role) return true;
  return role.permissions.every((p) => permissions.has(p));
}
