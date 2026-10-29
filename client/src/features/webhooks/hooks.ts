"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import { usePagedQuery } from "@/lib/query/paged";
import * as api from "./api";
import type { DeliveryStatus } from "./types";

export function useWebhooks() {
  const { organizationId } = useSession();
  return useQuery({
    queryKey: orgKey(organizationId, "webhooks"),
    queryFn: ({ signal }) => api.getWebhooks(signal),
    enabled: Boolean(organizationId),
  });
}

export function useDeliveries(id: string, status: DeliveryStatus | "") {
  const { organizationId } = useSession();
  return usePagedQuery({
    queryKey: orgKey(organizationId, "webhooks", id, "deliveries", status),
    fetchPage: (cursor, signal) => api.getDeliveriesPage(id, status, cursor, signal),
    enabled: Boolean(organizationId),
  });
}

export function useWebhookMutation<Vars, Result>(run: (vars: Vars) => Promise<Result>) {
  const client = useQueryClient();
  const { organizationId } = useSession();
  return useMutation({
    mutationFn: run,
    onSuccess: () => client.invalidateQueries({ queryKey: orgKey(organizationId, "webhooks") }),
  });
}
