"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { isApiError } from "@/lib/api/errors";
import { useOrganizationId } from "@/lib/auth/session";
import { accountKey, orgKey } from "@/lib/query/keys";
import {
  actOnDeadLetter,
  deleteAsset,
  getAsset,
  getDeadLetter,
  getLifecycle,
  getProcessing,
  listConnections,
  listDeadLetters,
  reconcileQuota,
  releaseQuarantine,
  restoreAsset,
} from "./api";

export function useAsset(id: string) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "operations", "asset", id),
    queryFn: ({ signal }) => getAsset(id, signal),
    enabled: Boolean(organizationId) && id !== "",
    retry: false,
  });
}

export function useLifecycle(id: string, enabled: boolean) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "operations", "lifecycle", id),
    queryFn: ({ signal }) => getLifecycle(id, signal),
    enabled: Boolean(organizationId) && id !== "" && enabled,
    retry: false,
  });
}

// A 404 here is normal: assets that were not uploaded through processing have no job.
export function useProcessing(id: string, enabled: boolean) {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "operations", "processing", id),
    queryFn: ({ signal }) => getProcessing(id, signal),
    enabled: Boolean(organizationId) && id !== "" && enabled,
    retry: false,
  });
}

export function useAssetActions(id: string) {
  const client = useQueryClient();
  const organizationId = useOrganizationId();
  const refresh = () => client.invalidateQueries({ queryKey: orgKey(organizationId, "operations") });

  return {
    remove: useMutation({ mutationFn: () => deleteAsset(id), onSuccess: refresh }),
    restore: useMutation({ mutationFn: () => restoreAsset(id), onSuccess: refresh }),
    release: useMutation({ mutationFn: () => releaseQuarantine(id), onSuccess: refresh }),
  };
}

export function useDeadLetters(status: string, enabled: boolean) {
  return useQuery({
    queryKey: accountKey("dead-letters", status),
    queryFn: ({ signal }) => listDeadLetters(status, signal),
    enabled,
    retry: false,
  });
}

export function useDeadLetter(id: string | null) {
  return useQuery({
    queryKey: accountKey("dead-letter", id),
    queryFn: ({ signal }) => getDeadLetter(id as string, signal),
    enabled: id !== null,
    retry: false,
  });
}

export function useDeadLetterAction() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (input: { id: string; action: Parameters<typeof actOnDeadLetter>[1]; reason: string }) =>
      actOnDeadLetter(input.id, input.action, input.reason),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: accountKey("dead-letters") });
      void client.invalidateQueries({ queryKey: accountKey("dead-letter") });
    },
  });
}

export function useConnections() {
  const organizationId = useOrganizationId();
  return useQuery({
    queryKey: orgKey(organizationId, "operations", "connections"),
    queryFn: ({ signal }) => listConnections(signal),
    enabled: Boolean(organizationId),
    retry: false,
  });
}

export function useReconcileQuota() {
  const client = useQueryClient();
  const organizationId = useOrganizationId();
  return useMutation({
    mutationFn: () => reconcileQuota(organizationId as string),
    onSuccess: () => client.invalidateQueries({ queryKey: orgKey(organizationId, "quota") }),
  });
}

export const isNotFound = (error: unknown) => isApiError(error) && error.isNotFound;
