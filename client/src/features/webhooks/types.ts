export type Webhook = {
  id: string;
  url: string;
  description: string;
  event_types: string[];
  enabled: boolean;
  created_by: string;
  created_at: string;
  updated_at: string;
  // Only on create: shown once.
  secret?: string;
};

export type WebhookList = { webhooks: Webhook[]; event_types: string[] };

export type DeliveryStatus = "PENDING" | "DELIVERED" | "FAILED";

export type Delivery = {
  id: string;
  event_id: string;
  event_type: string;
  status: DeliveryStatus;
  attempts: number;
  next_attempt_at?: string;
  last_status_code?: number;
  last_error?: string;
  last_attempt_at?: string;
  delivered_at?: string;
  created_at: string;
  payload: Record<string, unknown>;
};

export type WebhookInput = {
  url?: string;
  description?: string;
  event_types?: string[];
  enabled?: boolean;
};
