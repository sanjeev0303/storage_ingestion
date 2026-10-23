"use client";

import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import { FormProvider, useForm, useWatch } from "react-hook-form";
import { Button } from "@/components/ui/button";
import { Modal } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { SubmitButton } from "@/components/ui/submit-button";
import { describeError, isApiError } from "@/lib/api/errors";
import { zodResolver } from "@/lib/validation/resolver";
import { rotateCredentials, testResultFromError, updateConnection } from "../api";
import { useRefreshStorage } from "../hooks";
import {
    ENDPOINT_MAX,
  NAME_MAX,
  REGION_MAX,
  buildCredentials,
  buildUpdateRequest,
  changesReachability,
  editDefaults,
  editSchema,
  emptyCredentials,
  fieldsFor,
  rotateSchema,
  type EditForm,
  type RotateForm,
} from "../schema";
import type { StorageConnection, TestResponse } from "../types";
import { CredentialFields } from "./credential-fields";
import { TestResult } from "./test-result";

type DialogProps = {
  connection: StorageConnection;
  open: boolean;
  onOpenChange: (open: boolean) => void;
};

const STALE =
  "Someone else changed this connection while you were editing. It has been reloaded; check the values and try again.";

export function EditDialog({ connection, open, onOpenChange }: DialogProps) {
  const [notice, setNotice] = useState<string | null>(null);

  function handleOpenChange(next: boolean) {
    if (!next) setNotice(null);
    onOpenChange(next);
  }

  return (
    <Modal
      open={open}
      onOpenChange={handleOpenChange}
      title="Edit connection"
      description="The provider, authentication method, bucket and prefixes cannot be changed. Create a new connection to move data."
    >
      <EditBody
        key={connection.revision}
        connection={connection}
        notice={notice}
        onStale={() => setNotice(STALE)}
        onClose={() => handleOpenChange(false)}
      />
    </Modal>
  );
}

function EditBody({
  connection,
  notice,
  onStale,
  onClose,
}: {
  connection: StorageConnection;
  notice: string | null;
  onStale: () => void;
  onClose: () => void;
}) {
  const refresh = useRefreshStorage();
  const [formError, setFormError] = useState<string | null>(null);
  const [refused, setRefused] = useState<TestResponse | null>(null);
  const form = useForm<EditForm>({
    resolver: zodResolver<EditForm>(editSchema(connection)),
    defaultValues: editDefaults(connection),
    mode: "onTouched",
  });
  const save = useMutation({ mutationFn: (values: EditForm) => updateConnection(connection.id, buildUpdateRequest(connection, values)) });

  const fields = fieldsFor(connection.provider, connection.auth_method);
  const current = useWatch({ control: form.control });
  const retests = connection.status !== "PENDING" && changesReachability(buildUpdateRequest(connection, { ...editDefaults(connection), ...current }));

  async function submit(values: EditForm) {
    setFormError(null);
    setRefused(null);
    if (Object.keys(buildUpdateRequest(connection, values)).length === 1) return onClose();
    try {
      await save.mutateAsync(values);
      await refresh();
      onClose();
    } catch (error) {
      const result = testResultFromError(error);
      if (result) return setRefused(result);
      if (isApiError(error) && error.code === "revision_conflict") {
        refresh();
        return onStale();
      }
      if (isApiError(error) && error.code === "conflict") {
        return form.setError("name", { message: "A connection with this name already exists." });
      }
      setFormError(describeError(error));
    }
  }

  return (
    <form className="flex flex-col gap-4" onSubmit={form.handleSubmit(submit)} noValidate>
      {notice && (
        <p role="alert" className="neo-in-sm p-3 text-base font-medium">
          {notice}
        </p>
      )}
      <Field label="Name" required error={form.formState.errors.name?.message}>
        {(props) => <Input {...props} {...form.register("name")} maxLength={NAME_MAX} autoComplete="off" />}
      </Field>
      {fields.region && (
        <Field label="Region" required={fields.region === "required"} error={form.formState.errors.region?.message}>
          {(props) => <Input {...props} {...form.register("region")} maxLength={REGION_MAX} autoComplete="off" spellCheck={false} />}
        </Field>
      )}
      {fields.endpoint && (
        <Field label="Endpoint" required={fields.endpoint === "required"} error={form.formState.errors.endpoint?.message}>
          {(props) => <Input {...props} {...form.register("endpoint")} maxLength={ENDPOINT_MAX} inputMode="url" autoComplete="off" spellCheck={false} />}
        </Field>
      )}
      {fields.force_path_style && <Checkbox label="Use path-style addressing" {...form.register("force_path_style")} />}
      <Field
        label="Staging mode"
        error={form.formState.errors.staging_mode?.message}
        hint={connection.auth_method === "SAS_TOKEN" ? "A SAS token cannot sign upload links, so staging stays on Platform." : undefined}
      >
        {(props) => (
          <Select {...props} {...form.register("staging_mode")} disabled={connection.auth_method === "SAS_TOKEN"}>
            <option value="CONNECTION">Connection</option>
            <option value="PLATFORM">Platform</option>
          </Select>
        )}
      </Field>

      {retests && (
        <p className="neo-in-sm p-3 text-base">
          Changing the region, endpoint or addressing style re-tests this {connection.status.toLowerCase()} connection with its
          stored credentials before saving. Uploads and reads that use it switch to the new settings at once, so a wrong value can
          interrupt work in progress. If the test fails, nothing is stored.
        </p>
      )}
      {refused && <TestResult result={refused} note="Nothing was stored. The connection still uses its previous settings." />}
      {formError && (
        <p role="alert" className="text-base font-medium">
          {formError}
        </p>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        <Button onClick={onClose}>Cancel</Button>
        <SubmitButton valid={form.formState.isValid} loading={save.isPending} hint="Fix the fields marked above to save.">
          Save changes
        </SubmitButton>
      </div>
    </form>
  );
}

export function RotateDialog({
  connection,
  open,
  onOpenChange,
  onRotated,
}: DialogProps & { onRotated: (result: TestResponse) => void }) {
  const [notice, setNotice] = useState<string | null>(null);

  function handleOpenChange(next: boolean) {
    if (!next) setNotice(null);
    onOpenChange(next);
  }

  return (
    <Modal
      open={open}
      onOpenChange={handleOpenChange}
      title="Rotate credentials"
      description="The new credentials are tested first and stored only if the test passes. Until then the old credentials stay in use. Stored credentials are never shown, so this form is always empty."
    >
      <RotateBody
        connection={connection}
        notice={notice}
        onStale={() => setNotice(STALE)}
        onClose={() => handleOpenChange(false)}
        onRotated={onRotated}
      />
    </Modal>
  );
}

function RotateBody({
  connection,
  notice,
  onStale,
  onClose,
  onRotated,
}: {
  connection: StorageConnection;
  notice: string | null;
  onStale: () => void;
  onClose: () => void;
  onRotated: (result: TestResponse) => void;
}) {
  const refresh = useRefreshStorage();
  const [formError, setFormError] = useState<string | null>(null);
  const [refused, setRefused] = useState<TestResponse | null>(null);
  const form = useForm<RotateForm>({
    resolver: zodResolver<RotateForm>(rotateSchema(connection.auth_method)),
    defaultValues: { credentials: { ...emptyCredentials } },
    mode: "onTouched",
  });
  const rotate = useMutation({
    mutationFn: (values: RotateForm) =>
      rotateCredentials(connection.id, connection.revision, buildCredentials(connection.auth_method, values.credentials)),
  });

  async function submit(values: RotateForm) {
    setFormError(null);
    setRefused(null);
    try {
      const result = await rotate.mutateAsync(values);
      await refresh();
      onRotated(result);
      onClose();
    } catch (error) {
      const result = testResultFromError(error);
      if (result) setRefused(result);
      else if (isApiError(error) && error.code === "revision_conflict") {
        refresh();
        onStale();
      } else setFormError(describeError(error));
    } finally {
      // the mutation holds the request body, which holds the new secret
      rotate.reset();
      form.reset({ credentials: { ...emptyCredentials } });
    }
  }

  return (
    <FormProvider {...form}>
      <form className="flex flex-col gap-4" onSubmit={form.handleSubmit(submit)} noValidate autoComplete="off">
        {notice && (
          <p role="alert" className="neo-in-sm p-3 text-base font-medium">
            {notice}
          </p>
        )}
        <CredentialFields method={connection.auth_method} />
        {refused && (
          <TestResult
            result={refused}
            note="The new credentials were not stored. The old credentials are still in use. Enter them again to retry."
          />
        )}
        {formError && (
          <p role="alert" className="text-base font-medium">
            {formError}
          </p>
        )}
        <div className="flex flex-wrap justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <SubmitButton valid={form.formState.isValid} loading={rotate.isPending} hint="Enter the new credentials to rotate.">
            Rotate credentials
          </SubmitButton>
        </div>
      </form>
    </FormProvider>
  );
}
