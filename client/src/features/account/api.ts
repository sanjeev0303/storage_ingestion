import { accountApi } from "@/lib/api/client";
import type { Session } from "@/lib/api/types/identity";

export const getSessions = (signal?: AbortSignal) => accountApi<{ sessions: Session[] }>("/auth/sessions", { signal });

export const revokeSession = (id: string) => accountApi<void>(`/auth/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });

export const changePassword = (body: { current_password: string; new_password: string }) =>
  accountApi<void>("/auth/password", { method: "POST", body });
