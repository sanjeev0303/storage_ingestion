"use client";

import { Field, Select } from "@/components/ui/field";
import type { ProviderType, StorageConnection } from "@/features/storage-connections/types";
import { useSession } from "@/lib/auth/session";

export const PROVIDER_LABEL: Record<ProviderType, string> = {
  PLATFORM_MANAGED: "Platform-managed storage",
  AWS_S3: "Amazon S3",
  S3_COMPATIBLE: "S3-compatible",
  CLOUDFLARE_R2: "Cloudflare R2",
  GOOGLE_CLOUD_STORAGE: "Google Cloud Storage",
  AZURE_BLOB: "Azure Blob Storage",
};

export function describeConnection(connection: StorageConnection | undefined, fallbackId?: string) {
  if (!connection) return fallbackId ?? "Organization default";
  return `${connection.name} (${PROVIDER_LABEL[connection.provider] ?? connection.provider})`;
}

type Props = {
  label: string;
  connections: StorageConnection[];
  defaultConnectionId: string;
  value: string;
  onChange: (id: string) => void;
  disabled?: boolean;
};

export function ConnectionSelect({ label, connections, defaultConnectionId, value, onChange, disabled }: Props) {
  const { can } = useSession();
  const usable = connections.filter((c) => c.status === "ACTIVE");
  const selected = usable.find((c) => c.id === value);
  const mayUseOthers = can("storage.use");

  if (usable.length === 0) {
    return (
      <p className="text-sm text-muted">
        Files go to the organization&apos;s default storage. Your role cannot list storage connections.
      </p>
    );
  }

  return (
    <Field
      label={label}
      hint={
        <>
          {selected ? describeConnection(selected) : "Organization default"}
          {!mayUseOthers && " Your role can only use the default connection."}
        </>
      }
    >
      {(props) => (
        <Select {...props} value={value} disabled={disabled} onChange={(event) => onChange(event.target.value)}>
          {usable.map((connection) => (
            <option key={connection.id} value={connection.id} disabled={!mayUseOthers && connection.id !== defaultConnectionId}>
              {describeConnection(connection)}
              {connection.id === defaultConnectionId ? ", default" : ""}
            </option>
          ))}
        </Select>
      )}
    </Field>
  );
}
