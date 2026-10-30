"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Field, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { SubmitButton } from "@/components/ui/submit-button";
import { notify } from "@/components/ui/toast";
import { useSession } from "@/lib/auth/session";
import { orgKey } from "@/lib/query/keys";
import type { Classification } from "@/features/operations/types";
import { classifyAsset } from "../api";

export const CLASS_TEXT: Record<Classification, string> = {
  PUBLIC: "Public",
  INTERNAL: "Internal",
  CONFIDENTIAL: "Confidential: links last at most 1 hour",
  RESTRICTED: "Restricted: no share links, presigned links at most 15 minutes",
};

export function ClassificationPanel({ assetId, current }: { assetId: string; current?: Classification }) {
  const { organizationId, can } = useSession();
  const [value, setValue] = useState<Classification | "">(current ?? "");
  const client = useQueryClient();
  const save = useMutation({ mutationFn: classifyAsset });

  return (
    <Panel
      title="Classification"
      description="How sensitive this version is. Content checks raise it on their own: credentials make it restricted."
    >
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (!value) return;
          save.mutate(
            { id: assetId, classification: value },
            {
              onSuccess: async () => {
                await client.invalidateQueries({ queryKey: orgKey(organizationId, "operations", "asset", assetId) });
                notify.success("Classification saved");
              },
              onError: notify.error,
            },
          );
        }}
      >
        <Field label="Classification">
          {(props) => (
            <Select
              {...props}
              name="classification"
              value={value}
              disabled={!can("catalog.write")}
              onChange={(e) => setValue(e.target.value as Classification)}
            >
              {!current && <option value="">Not classified</option>}
              {(Object.keys(CLASS_TEXT) as Classification[]).map((c) => (
                <option key={c} value={c}>
                  {CLASS_TEXT[c]}
                </option>
              ))}
            </Select>
          )}
        </Field>
        {can("catalog.write") ? (
          <SubmitButton valid={value !== "" && value !== current} loading={save.isPending} hint="Choose a different classification.">
            Save classification
          </SubmitButton>
        ) : (
          <p className="text-sm text-muted">Changing it needs catalog.write.</p>
        )}
      </form>
    </Panel>
  );
}
