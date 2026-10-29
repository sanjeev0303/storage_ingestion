import { api } from "@/lib/api/client";
import type { CursorPage } from "@/lib/query/paged";
import type { Delivery, DeliveryStatus, Webhook, WebhookInput, WebhookList } from "./types";

const path = (id: string) => `/webhooks/${encodeURIComponent(id)}`;

export const getWebhooks = (signal?: AbortSignal) => api<WebhookList>("/webhooks", { signal });

export const createWebhook = (body: WebhookInput) => api<Webhook>("/webhooks", { method: "POST", body });

export const updateWebhook = ({ id, ...body }: WebhookInput & { id: string }) =>
  api<Webhook>(path(id), { method: "PATCH", body });

export const deleteWebhook = (id: string) => api<void>(path(id), { method: "DELETE" });

export const rotateSecret = (id: string) => api<{ secret: string }>(`${path(id)}/rotate-secret`, { method: "POST" });

export const sendTest = (id: string) => api<Delivery>(`${path(id)}/test`, { method: "POST" });

export const redeliver = ({ id, deliveryId }: { id: string; deliveryId: string }) =>
  api<Delivery>(`${path(id)}/deliveries/${encodeURIComponent(deliveryId)}/retry`, { method: "POST" });

export async function getDeliveriesPage(
  id: string,
  status: DeliveryStatus | "",
  before: string | undefined,
  signal?: AbortSignal,
): Promise<CursorPage<Delivery, string>> {
  const res = await api<{ deliveries: Delivery[]; next_before?: string }>(`${path(id)}/deliveries`, {
    query: { status, before, limit: 25 },
    signal,
  });
  return { items: res.deliveries ?? [], next: res.next_before || undefined };
}
