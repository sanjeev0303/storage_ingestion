"use client";

import { useRef, useState } from "react";
import { useFormContext } from "react-hook-form";
import { Button } from "@/components/ui/button";
import { Field, Textarea } from "@/components/ui/field";
import { PasswordInput } from "@/components/ui/password-field";
import { cn } from "@/lib/utils/cn";
import { CREDENTIAL_FIELDS, SECRET_MAX, SERVICE_ACCOUNT_MAX, type CredentialField, type CredentialValues } from "../schema";
import type { AuthMethod } from "../types";

type FormShape = { credentials: CredentialValues };

// Secrets are typed or pasted once. They are masked by default, never autofilled,
// and the parent form clears them after every submit.
export function CredentialFields({ method }: { method: AuthMethod | "" }) {
  if (!method || CREDENTIAL_FIELDS[method].length === 0) return null;
  return (
    <div className="flex flex-col gap-4">
      {CREDENTIAL_FIELDS[method].map((field) => (
        <SecretField key={field.name} field={field} />
      ))}
    </div>
  );
}

function SecretField({ field }: { field: CredentialField }) {
  const { register, setValue, formState } = useFormContext<FormShape>();
  const [shown, setShown] = useState(false);
  const [fileError, setFileError] = useState<string | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const error = formState.errors.credentials?.[field.name]?.message;
  const registered = register(`credentials.${field.name}`);

  async function readFile(file: File | undefined) {
    if (!file) return;
    if (file.size > SERVICE_ACCOUNT_MAX) {
      setFileError("That file is larger than 16 KiB, so it is not a service account key.");
      return;
    }
    setFileError(null);
    setValue(`credentials.${field.name}`, await file.text(), { shouldValidate: true });
    if (fileInput.current) fileInput.current.value = "";
  }

  return (
    <Field
      label={field.label}
      required={!field.optional}
      error={error ?? fileError ?? undefined}
      hint={field.multiline ? "Paste the key file, or choose it from disk. It is read in this page and sent once; it is never shown again." : undefined}
    >
      {(props) =>
        field.multiline ? (
          <div className="flex flex-col gap-2">
            <Textarea
              {...props}
              {...registered}
              rows={5}
              maxLength={SERVICE_ACCOUNT_MAX}
              autoComplete="off"
              spellCheck={false}
              className={cn("mono-data", !shown && "[-webkit-text-security:disc]")}
            />
            <div className="flex flex-wrap gap-2">
              <Button aria-pressed={shown} aria-label={`${shown ? "Hide" : "Show"} ${field.label.toLowerCase()}`} onClick={() => setShown(!shown)}>
                {shown ? "Hide" : "Show"}
              </Button>
              <Button onClick={() => fileInput.current?.click()}>Choose key file</Button>
              <input
                ref={fileInput}
                type="file"
                accept=".json,application/json"
                className="sr-only"
                tabIndex={-1}
                aria-label="Service account key file"
                onChange={(e) => readFile(e.target.files?.[0])}
              />
            </div>
          </div>
        ) : (
          <PasswordInput
            {...props}
            {...registered}
            maxLength={SECRET_MAX}
            autoComplete="off"
            spellCheck={false}
            autoCapitalize="off"
          />
        )
      }
    </Field>
  );
}
