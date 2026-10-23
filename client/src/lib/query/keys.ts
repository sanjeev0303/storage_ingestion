// Every query about an organization's data starts with ["org", orgId, ...].
// Switching organization or signing out removes everything under "org", so a
// cached answer for one organization is never shown for another.
export function orgKey(orgId: string | null, ...parts: unknown[]) {
  return ["org", orgId, ...parts] as const;
}

export const accountKey = (...parts: unknown[]) => ["account", ...parts] as const;
