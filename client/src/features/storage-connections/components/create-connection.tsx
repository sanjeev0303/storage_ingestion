"use client";

import { useMutation } from "@tanstack/react-query";
import Link from "next/link";
import { useEffect, useState } from "react";
import { FormProvider, useForm, useWatch } from "react-hook-form";
import { Button, ButtonLink } from "@/components/ui/button";
import { SubmitButton } from "@/components/ui/submit-button";
import { Checkbox, Field, Input, Select } from "@/components/ui/field";
import { Panel } from "@/components/ui/panel";
import { PermissionDenied, SkeletonRows, ErrorState } from "@/components/ui/states";
import { StateBadge } from "@/components/ui/status";
import { CopyButton } from "@/components/ui/copy-button";
import { describeError, isApiError } from "@/lib/api/errors";
import { useSession } from "@/lib/auth/session";
import { zodResolver } from "@/lib/validation/resolver";
import { createConnection, testConnection } from "../api";
import { useRefreshStorage, useStorageProviders } from "../hooks";
import {
  ACCOUNT_ID_MAX,
  AUTH_LABELS,
  BUCKET_MAX,
  CREATABLE_PROVIDERS,
  DEFAULT_AUTH_METHODS,
  ENDPOINT_MAX,
  NAME_MAX,
  PREFIX_MAX,
  PROVIDER_LABELS,
  REGION_MAX,
  buildCreateRequest,
  createDefaults,
  createSchema,
  emptyCredentials,
  fieldsFor,
  type CreateForm,
} from "../schema";
import type { AuthMethod, ProviderType, StorageConnection, TestResponse } from "../types";
import { CredentialFields } from "./credential-fields";
import { TestResult } from "./test-result";

export function CreateConnection() {
  const { can } = useSession();
  const providers = useStorageProviders();
  const refresh = useRefreshStorage();
  const [created, setCreated] = useState<StorageConnection | null>(null);
  const [formError, setFormError] = useState<string | null>(null);

  const form = useForm<CreateForm>({ resolver: zodResolver<CreateForm>(createSchema), defaultValues: createDefaults, mode: "onTouched" });
  const create = useMutation({ mutationFn: createConnection });

  const provider = useWatch({ control: form.control, name: "provider" });
  const method = useWatch({ control: form.control, name: "auth_method" });
  const fields = fieldsFor(provider, method);
  const methodsForProvider = provider ? methodsOf(provider, providers.data?.providers) : [];
  const values = useWatch({ control: form.control });
  const problem = createSchema.safeParse(values).error?.issues[0]?.message;

  useEffect(() => {
    // a single provider needs no choosing
    const only = providers.data?.providers.filter((p) => CREATABLE_PROVIDERS.includes(p.provider));
    if (!form.getValues("provider") && only?.length === 1) form.setValue("provider", only[0].provider);
  }, [providers.data, form]);

  useEffect(() => {
    if (!provider) return;
    const current = form.getValues("auth_method");
    if (current && methodsForProvider.includes(current)) return;
    form.setValue("auth_method", methodsForProvider.length === 1 ? methodsForProvider[0] : "");
    form.setValue("credentials", { ...emptyCredentials });
    // methodsForProvider is derived from provider and the catalog
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [provider, providers.data]);

  useEffect(() => {
    if (!provider) return;
    form.setValue("region", DEFAULT_REGIONS[provider] ?? "");
    form.setValue("force_path_style", provider === "S3_COMPATIBLE");
  }, [provider, form]);

  useEffect(() => {
    // secrets typed for another method must not linger in the form
    form.setValue("credentials", { ...emptyCredentials });
    if (method === "SAS_TOKEN") form.setValue("staging_mode", "PLATFORM");
  }, [method, form]);

  if (!can("storage.manage")) return <PermissionDenied detail="Creating a storage connection needs the storage.manage permission." />;
  if (providers.isPending) return <SkeletonRows rows={6} />;
  if (providers.isError) return <ErrorState error={providers.error} onRetry={() => providers.refetch()} />;

  const byosEnabled = providers.data.byos_enabled;
  const creatable = providers.data.providers.filter((p) => CREATABLE_PROVIDERS.includes(p.provider));
  const info = creatable.find((p) => p.provider === provider);

  async function submit(values: CreateForm) {
    setFormError(null);
    try {
      const connection = await create.mutateAsync(buildCreateRequest(values));
      setCreated(connection);
      form.reset(createDefaults);
      refresh();
    } catch (error) {
      showError(error);
      form.setValue("credentials", { ...emptyCredentials });
    } finally {
      // the request body holds the credentials; do not keep it
      create.reset();
    }
  }

  function showError(error: unknown) {
    if (!isApiError(error)) return setFormError(describeError(error));
    if (error.code === "conflict") {
      return form.setError("name", { message: "A connection with this name already exists. Choose another name." });
    }
    if (error.code === "endpoint_rejected") {
      return form.setError("endpoint", {
        message: "The platform's network policy does not allow this endpoint. Use a public https address.",
      });
    }
    if (error.code === "byos_disabled") {
      return setFormError("Customer storage connections are disabled on this deployment.");
    }
    if (error.code === "invalid_request") {
      return setFormError(`${error.message}. Check the fields above, then re-enter the credentials.`);
    }
    setFormError(describeError(error));
  }

  if (created) return <Created connection={created} onAnother={() => setCreated(null)} />;

  return (
    <div className="grid gap-6 lg:grid-cols-3">
      <FormProvider {...form}>
        <form className="flex flex-col gap-6 lg:col-span-2" onSubmit={form.handleSubmit(submit)} noValidate autoComplete="off">
          {!byosEnabled && (
            <p role="alert" className="neo-in p-4 text-base font-medium">
              Customer connections are disabled on this deployment, so nothing can be created here. Uploads use the
              platform-managed storage.
            </p>
          )}
          <fieldset disabled={!byosEnabled || create.isPending} className="flex min-w-0 flex-col gap-6">
            <legend className="sr-only">New storage connection</legend>

            <Panel title="Provider" description="Where the bucket or container lives, and how the platform signs in to it.">
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="Provider" required error={form.formState.errors.provider?.message}>
                  {(props) => (
                    <Select {...props} {...form.register("provider")}>
                      <option value="">Choose a provider</option>
                      {creatable.map((p) => (
                        <option key={p.provider} value={p.provider}>
                          {p.name || PROVIDER_LABELS[p.provider]}
                        </option>
                      ))}
                    </Select>
                  )}
                </Field>
                <Field label="Authentication method" required error={form.formState.errors.auth_method?.message}>
                  {(props) => (
                    <Select {...props} {...form.register("auth_method")} disabled={!provider}>
                      <option value="">{provider ? "Choose a method" : "Choose a provider first"}</option>
                      {methodsForProvider.map((m) => (
                        <option key={m} value={m}>
                          {AUTH_LABELS[m]}
                        </option>
                      ))}
                    </Select>
                  )}
                </Field>
              </div>
            </Panel>

            {provider && method && (
              <Panel title="Where the data goes" description="Existing objects in this bucket are never read, changed or deleted by creating the connection.">
                <div className="grid gap-4 sm:grid-cols-2">
                  <Field label="Name" required error={form.formState.errors.name?.message} hint="Shown in the console and in pickers. Unique in this organization.">
                    {(props) => <Input {...props} {...form.register("name")} maxLength={NAME_MAX} autoComplete="off" />}
                  </Field>
                  <Field label={fields.bucketLabel} required error={form.formState.errors.bucket?.message}>
                    {(props) => <Input {...props} {...form.register("bucket")} maxLength={BUCKET_MAX} autoComplete="off" spellCheck={false} />}
                  </Field>
                  {fields.account_id && (
                    <Field
                      label={provider === "AZURE_BLOB" ? "Storage account" : "Account ID"}
                      required
                      error={form.formState.errors.account_id?.message}
                      hint={provider === "CLOUDFLARE_R2" ? "The Cloudflare account ID. The R2 endpoint is derived from it." : undefined}
                    >
                      {(props) => <Input {...props} {...form.register("account_id")} maxLength={ACCOUNT_ID_MAX} autoComplete="off" spellCheck={false} />}
                    </Field>
                  )}
                  {fields.region && (
                    <Field label="Region" required={fields.region === "required"} error={form.formState.errors.region?.message} hint="For example eu-west-1.">
                      {(props) => <Input {...props} {...form.register("region")} maxLength={REGION_MAX} autoComplete="off" spellCheck={false} />}
                    </Field>
                  )}
                  {fields.endpoint && (
                    <Field
                      label="Endpoint"
                      required={fields.endpoint === "required"}
                      error={form.formState.errors.endpoint?.message}
                      hint={
                        fields.endpoint === "required"
                          ? "Host only, for example https://s3.example.com. Plain http works only where this deployment allows private endpoints."
                          : "Leave empty to use the provider's own endpoint. A custom one must stay on the provider's domain."
                      }
                    >
                      {(props) => <Input {...props} {...form.register("endpoint")} maxLength={ENDPOINT_MAX} inputMode="url" autoComplete="off" spellCheck={false} />}
                    </Field>
                  )}
                  {fields.role_arn && (
                    <Field label="Role ARN" required error={form.formState.errors.role_arn?.message} hint="The IAM role the platform assumes. You add the trust policy after creating the connection.">
                      {(props) => <Input {...props} {...form.register("role_arn")} autoComplete="off" spellCheck={false} />}
                    </Field>
                  )}
                </div>
                {fields.force_path_style && (
                  <div className="mt-4">
                    <Checkbox
                      label="Use path-style addressing (bucket in the URL path). Needed for MinIO and many self-hosted services."
                      {...form.register("force_path_style")}
                    />
                  </div>
                )}
              </Panel>
            )}

            {provider && method && CREDENTIAL_NOTE[method] !== undefined && (
              <Panel title="Credentials" description={CREDENTIAL_NOTE[method]}>
                {method === "ASSUME_ROLE" ? (
                  <p className="text-base text-muted">
                    No secret is stored. After creating the connection you get an external ID to put in the role&apos;s trust policy.
                  </p>
                ) : (
                  <CredentialFields method={method} />
                )}
              </Panel>
            )}

            {provider && method && (
              <Panel title="Staging" description="Where an upload lands before it is verified. The defaults suit most connections.">
                <div className="grid gap-4 sm:grid-cols-3">
                  <Field label="Staging prefix" error={form.formState.errors.staging_prefix?.message}>
                    {(props) => <Input {...props} {...form.register("staging_prefix")} maxLength={PREFIX_MAX} autoComplete="off" spellCheck={false} />}
                  </Field>
                  <Field label="Ready prefix" error={form.formState.errors.ready_prefix?.message}>
                    {(props) => <Input {...props} {...form.register("ready_prefix")} maxLength={PREFIX_MAX} autoComplete="off" spellCheck={false} />}
                  </Field>
                  <Field label="Staging mode" error={form.formState.errors.staging_mode?.message}>
                    {(props) => (
                      <Select {...props} {...form.register("staging_mode")} disabled={method === "SAS_TOKEN"}>
                        <option value="CONNECTION">Connection</option>
                        <option value="PLATFORM">Platform</option>
                      </Select>
                    )}
                  </Field>
                </div>
                <p className="mt-3 max-w-prose text-sm text-muted">
                  {method === "SAS_TOKEN"
                    ? "A SAS token cannot sign upload links, so uploads stage in the platform's quarantine and only verified bytes are copied to your container."
                    : "Connection: uploads land in your bucket under the staging prefix before they are verified. Platform: they land in the platform's quarantine first and only verified bytes reach your bucket."}
                </p>
              </Panel>
            )}

            {formError && (
              <p role="alert" className="neo-in p-4 text-base font-medium">
                {formError}
              </p>
            )}

            <div className="flex flex-wrap items-center gap-3">
              <SubmitButton valid={byosEnabled && !problem} loading={create.isPending} hint={byosEnabled ? problem : "Customer connections are disabled on this deployment."}>
                Create connection
              </SubmitButton>
              <ButtonLink href="/app/storage" variant="ghost">
                Cancel
              </ButtonLink>
            </div>
          </fieldset>
        </form>
      </FormProvider>

      <aside className="flex flex-col gap-6">
        <Panel title="What happens next" tone="in">
          <ol className="flex list-decimal flex-col gap-2 pl-5 text-base text-muted">
            <li>The connection is created as Pending. Creating does not contact your provider.</li>
            <li>You test it. A passing test makes it Active.</li>
            <li>Only Active connections accept new uploads. Existing objects never move between connections.</li>
          </ol>
        </Panel>
        {info && (
          <Panel title={info.name || PROVIDER_LABELS[info.provider]} tone="in">
            {info.required_fields.length > 0 && (
              <p className="text-base">
                <span className="font-medium">Needs:</span> {info.required_fields.join(", ")}
              </p>
            )}
            {info.notes.length > 0 && (
              <ul className="mt-2 flex list-disc flex-col gap-1.5 pl-5 text-base text-muted">
                {info.notes.map((note) => (
                  <li key={note}>{note}</li>
                ))}
              </ul>
            )}
          </Panel>
        )}
      </aside>
    </div>
  );
}

// Pre-filled when a provider is chosen; the person can change it.
const DEFAULT_REGIONS: Partial<Record<ProviderType, string>> = { AWS_S3: "us-east-1" };

const CREDENTIAL_NOTE: Partial<Record<AuthMethod, string>> = {
  ACCESS_KEY: "Stored encrypted, shown to nobody afterwards. Use a key limited to this bucket.",
  ASSUME_ROLE: "The platform assumes a role in your AWS account instead of holding a key.",
  SERVICE_ACCOUNT_KEY: "Stored encrypted, shown to nobody afterwards. Use a key limited to this bucket.",
  ACCOUNT_KEY: "Stored encrypted, shown to nobody afterwards.",
  SAS_TOKEN: "A container SAS with read, add, create, write, delete and list permissions. Stored encrypted.",
};

function methodsOf(provider: ProviderType, catalog: { provider: ProviderType; auth_methods: { method: AuthMethod }[] }[] | undefined) {
  const entry = catalog?.find((p) => p.provider === provider);
  if (entry) return entry.auth_methods.map((m) => m.method);
  return DEFAULT_AUTH_METHODS[provider] ?? [];
}

function Created({ connection, onAnother }: { connection: StorageConnection; onAnother: () => void }) {
  const refresh = useRefreshStorage();
  const [result, setResult] = useState<TestResponse | null>(null);
  const test = useMutation({ mutationFn: () => testConnection(connection.id, false) });

  async function runTest() {
    setResult(null);
    try {
      setResult(await test.mutateAsync());
      refresh();
    } catch {
      // the error is shown below from the mutation state
    }
  }

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <Panel
        title={connection.name}
        actions={<StateBadge>{connection.status === "PENDING" ? "Pending" : connection.status}</StateBadge>}
        description="The connection exists but has not been tested. It cannot take uploads until a test passes."
      >
        <h3 className="text-md font-semibold">Test the connection</h3>
        <p className="mt-1 max-w-prose text-base text-muted">
          The test checks the endpoint, the credentials, the bucket and a listing. A pass makes the connection Active.
        </p>

        {connection.auth_method === "ASSUME_ROLE" && connection.external_id && (
          <div className="neo-in-sm mt-4 flex flex-col gap-2 p-4">
            <p className="text-base font-medium">Add this external ID to the role&apos;s trust policy first</p>
            <div className="flex flex-wrap items-center gap-3">
              <code className="mono-data break-all">{connection.external_id}</code>
              <CopyButton value={connection.external_id} label="Copy external ID" />
            </div>
            <p className="text-sm text-muted">
              In the role {connection.role_arn}, allow the platform&apos;s AWS principal to call sts:AssumeRole with the condition
              StringEquals on sts:ExternalId set to this value. The platform principal is listed in docs/storage-connections.md.
              The test fails until the trust policy is in place.
            </p>
          </div>
        )}

        <div className="mt-4 flex flex-wrap items-center gap-3">
          <Button variant="primary" onClick={runTest} loading={test.isPending}>
            Test connection
          </Button>
          <ButtonLink href={`/app/storage/${connection.id}`}>Open connection</ButtonLink>
          <Button variant="ghost" onClick={onAnother}>
            Add another
          </Button>
        </div>
        {test.isError && (
          <p role="alert" className="mt-4 text-base font-medium">
            {describeError(test.error)}
          </p>
        )}
        {result && (
          <div className="mt-4">
            <TestResult
              result={result}
              note={result.ok ? "The connection is now Active. Make it the default from its page if uploads should use it." : "The connection stays Pending. Fix the cause above and test again."}
            />
          </div>
        )}
      </Panel>
      <p className="text-sm text-muted">
        <Link href="/app/storage" className="underline underline-offset-2">
          Back to all connections
        </Link>
      </p>
    </div>
  );
}
