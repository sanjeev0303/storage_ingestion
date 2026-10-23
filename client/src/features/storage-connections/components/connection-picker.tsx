"use client";

import { useEffect } from "react";
import { Select } from "@/components/ui/field";
import { useStorageConnections } from "../hooks";

// A select of the platform connection and the Active ones, with the organization's
// default chosen until the caller picks something else. Pass the field's id from <Field>.
export function ConnectionPicker({
  value,
  onChange,
  id,
  disabled,
  "aria-describedby": describedBy,
}: {
  value: string;
  onChange: (connectionId: string) => void;
  id?: string;
  disabled?: boolean;
  "aria-describedby"?: string;
}) {
  const { data, isPending, isError } = useStorageConnections();
  const options = (data?.connections ?? []).filter((c) => c.provider === "PLATFORM_MANAGED" || c.status === "ACTIVE");
  const defaultId = data?.default_connection_id;

  useEffect(() => {
    if (!value && defaultId) onChange(defaultId);
  }, [value, defaultId, onChange]);

  return (
    <Select
      id={id}
      aria-describedby={describedBy}
      value={value}
      disabled={disabled || isPending || isError}
      onChange={(e) => onChange(e.target.value)}
    >
      {isPending && <option value="">Loading connections</option>}
      {isError && <option value="">Connections could not be loaded</option>}
      {options.map((c) => (
        <option key={c.id} value={c.id}>
          {c.name}
          {c.id === defaultId ? " (default)" : ""}
        </option>
      ))}
    </Select>
  );
}
