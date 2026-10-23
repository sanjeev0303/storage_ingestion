import { z } from "zod";
import { cleanText } from "@/lib/utils/forgive";
import type {
  AuthMethod,
  CreateConnectionRequest,
  ProviderType,
  StagingMode,
  StorageConnection,
  StorageCredentials,
  UpdateConnectionRequest,
} from "./types";

export const CREATABLE_PROVIDERS: ProviderType[] = [
  "AWS_S3",
  "CLOUDFLARE_R2",
  "GOOGLE_CLOUD_STORAGE",
  "AZURE_BLOB",
  "S3_COMPATIBLE",
];

export const PROVIDER_LABELS: Record<ProviderType, string> = {
  PLATFORM_MANAGED: "Platform-managed storage",
  AWS_S3: "Amazon S3",
  CLOUDFLARE_R2: "Cloudflare R2",
  GOOGLE_CLOUD_STORAGE: "Google Cloud Storage",
  AZURE_BLOB: "Azure Blob Storage",
  S3_COMPATIBLE: "S3-compatible service",
};

export const AUTH_LABELS: Record<AuthMethod, string> = {
  PLATFORM: "Platform",
  ACCESS_KEY: "Access key",
  ASSUME_ROLE: "Assume an IAM role",
  SERVICE_ACCOUNT_KEY: "Service account key",
  ACCOUNT_KEY: "Account key",
  SAS_TOKEN: "SAS token",
};

// Used when the provider catalog has not loaded; the catalog is the source of truth.
export const DEFAULT_AUTH_METHODS: Record<string, AuthMethod[]> = {
  AWS_S3: ["ACCESS_KEY", "ASSUME_ROLE"],
  CLOUDFLARE_R2: ["ACCESS_KEY"],
  S3_COMPATIBLE: ["ACCESS_KEY"],
  GOOGLE_CLOUD_STORAGE: ["SERVICE_ACCOUNT_KEY"],
  AZURE_BLOB: ["ACCOUNT_KEY", "SAS_TOKEN"],
};

type Need = "required" | "optional" | null;

export type VisibleFields = {
  region: Need;
  endpoint: Need;
  account_id: Need;
  force_path_style: boolean;
  role_arn: boolean;
  bucketLabel: string;
};

// Which configuration fields apply to a provider and authentication method.
export function fieldsFor(provider: ProviderType | "", method: AuthMethod | ""): VisibleFields {
  return {
    region: provider === "AWS_S3" ? "required" : provider === "S3_COMPATIBLE" ? "optional" : null,
    endpoint: provider === "S3_COMPATIBLE" ? "required" : provider ? "optional" : null,
    account_id: provider === "CLOUDFLARE_R2" || provider === "AZURE_BLOB" ? "required" : null,
    force_path_style: provider === "AWS_S3" || provider === "S3_COMPATIBLE",
    role_arn: provider === "AWS_S3" && method === "ASSUME_ROLE",
    bucketLabel: provider === "AZURE_BLOB" ? "Container" : "Bucket",
  };
}

export type CredentialKey = keyof StorageCredentials;

export type CredentialField = { name: CredentialKey; label: string; optional?: boolean; multiline?: boolean };

export const CREDENTIAL_FIELDS: Record<AuthMethod, CredentialField[]> = {
  PLATFORM: [],
  ASSUME_ROLE: [],
  ACCESS_KEY: [
    { name: "access_key_id", label: "Access key ID" },
    { name: "secret_access_key", label: "Secret access key" },
    { name: "session_token", label: "Session token", optional: true },
  ],
  SERVICE_ACCOUNT_KEY: [{ name: "service_account_json", label: "Service account key (JSON)", multiline: true }],
  ACCOUNT_KEY: [{ name: "account_key", label: "Account key" }],
  SAS_TOKEN: [{ name: "sas_token", label: "SAS token" }],
};

export const emptyCredentials = {
  access_key_id: "",
  secret_access_key: "",
  session_token: "",
  service_account_json: "",
  account_key: "",
  sas_token: "",
};

export type CredentialValues = typeof emptyCredentials;

export const SERVICE_ACCOUNT_MAX = 16 * 1024;
const BAD_CHARS = /[\r\n\0]/;
const CONTROL_CHARS = /[\u0000-\u001f\u007f]/;
const BUCKET = /^[a-z0-9][a-z0-9.\-_]{1,220}[a-z0-9]$/;
const REGION = /^[a-z0-9-]{0,64}$/;
const ACCOUNT_ID = /^[A-Za-z0-9-]{0,64}$/;
const PREFIX = /^[a-z0-9][a-z0-9\-_]{0,62}$/;
const ROLE_ARN = /^arn:aws[a-z-]*:iam::\d{12}:role\/[A-Za-z0-9+=,.@_\-/]{1,512}$/;

type Issue = (path: (string | number)[], message: string) => void;

// Pasted values often carry spaces, quotes or capitals. These are cleaned before
// they are checked and again before they are sent.
const unquote = (value: string) => value.trim().replace(/^["'`]+|["'`]+$/g, "").trim();
export const cleanBucket = (value: string) => unquote(value).toLowerCase();
export const cleanRegion = (value: string) => unquote(value).toLowerCase();
export const cleanEndpoint = (value: string) => unquote(value).toLowerCase().replace(/\/+$/, "");
export const cleanPrefix = (value: string) => unquote(value).toLowerCase().replace(/^\/+|\/+$/g, "");

export function bucketError(value: string, label = "Bucket") {
  const bucket = cleanBucket(value);
  if (BUCKET.test(bucket) && !bucket.includes("..")) return undefined;
  return `${label} names are 3 to 222 characters: lowercase letters, digits, . - _, starting and ending with a letter or digit, no "..".`;
}

export function regionError(value: string) {
  if (REGION.test(cleanRegion(value))) return undefined;
  return "Use lowercase letters, digits and - (up to 64).";
}

export const NAME_MAX = 128;
export const BUCKET_MAX = 222;
export const REGION_MAX = 64;
export const ENDPOINT_MAX = 512;
export const ACCOUNT_ID_MAX = 64;
export const PREFIX_MAX = 63;
export const SECRET_MAX = 4096;

function checkName(value: string, issue: Issue) {
  const name = cleanText(value);
  if (name.length < 1 || name.length > NAME_MAX) issue(["name"], "Use 1 to 128 characters.");
  else if (CONTROL_CHARS.test(name)) issue(["name"], "Remove control characters.");
}

function checkEndpoint(value: string, required: boolean, issue: Issue) {
  const text = cleanEndpoint(value);
  if (!text) {
    if (required) issue(["endpoint"], "Enter the service endpoint, for example https://s3.example.com.");
    return;
  }
  if (text.length > ENDPOINT_MAX || BAD_CHARS.test(text)) {
    issue(["endpoint"], "Use at most 512 characters, on one line.");
    return;
  }
  let url: URL;
  try {
    url = new URL(text);
  } catch {
    issue(["endpoint"], "Enter a full address such as https://s3.example.com.");
    return;
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    issue(["endpoint"], "The address must start with https://.");
  } else if (url.username || url.password) {
    issue(["endpoint"], "Remove the user name and password from the address.");
  } else if ((url.pathname !== "/" && url.pathname !== "") || url.search || url.hash) {
    issue(["endpoint"], "Use the host only, with no path, query or fragment.");
  }
}

function checkPrefixes(staging: string, ready: string, issue: Issue) {
  const stagingText = cleanPrefix(staging);
  const readyText = cleanPrefix(ready);
  if (stagingText && !PREFIX.test(stagingText)) {
    issue(["staging_prefix"], "Start with a letter or digit; use lowercase letters, digits, - and _ (up to 63).");
  }
  if (readyText && !PREFIX.test(readyText)) {
    issue(["ready_prefix"], "Start with a letter or digit; use lowercase letters, digits, - and _ (up to 63).");
  }
  if (stagingText && stagingText === readyText) {
    issue(["ready_prefix"], "The staging and ready prefixes must be different.");
  }
}

export function checkCredentials(
  method: AuthMethod | "",
  values: Partial<CredentialValues>,
  issue: Issue,
  prefix: (string | number)[] = ["credentials"],
) {
  if (!method) return;
  for (const field of CREDENTIAL_FIELDS[method]) {
    const value = values[field.name] ?? "";
    const path = [...prefix, field.name];
    if (!value.trim()) {
      if (!field.optional) issue(path, `Enter the ${field.label.toLowerCase()}.`);
      continue;
    }
    if (field.name === "service_account_json") {
      if (value.length > SERVICE_ACCOUNT_MAX) issue(path, "The key file is larger than 16 KiB.");
      else if (!isJsonObject(value)) issue(path, "Paste the whole key file; it must be a JSON object.");
      continue;
    }
    if (value.length > SECRET_MAX) issue(path, "Use at most 4096 characters.");
    else if (BAD_CHARS.test(value)) issue(path, "Remove line breaks from the value.");
  }
}

function isJsonObject(text: string) {
  try {
    const parsed = JSON.parse(text);
    return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed);
  } catch {
    return false;
  }
}

export const rotateSchema = (method: AuthMethod) =>
  z.object({ credentials: z.object({}).loose() }).superRefine((value, ctx) => {
    const issue: Issue = (path, message) => ctx.addIssue({ code: "custom", path, message });
    checkCredentials(method, value.credentials as Partial<CredentialValues>, issue);
  });

export type RotateForm = { credentials: CredentialValues };

export type CreateForm = {
  provider: ProviderType | "";
  auth_method: AuthMethod | "";
  name: string;
  bucket: string;
  region: string;
  endpoint: string;
  account_id: string;
  force_path_style: boolean;
  role_arn: string;
  staging_prefix: string;
  ready_prefix: string;
  staging_mode: StagingMode;
  credentials: CredentialValues;
};

export const createDefaults: CreateForm = {
  provider: "",
  auth_method: "",
  name: "",
  bucket: "",
  region: "",
  endpoint: "",
  account_id: "",
  force_path_style: false,
  role_arn: "",
  staging_prefix: "quarantine",
  ready_prefix: "assets",
  staging_mode: "CONNECTION",
  credentials: { ...emptyCredentials },
};

export const createSchema = z
  .object({
    provider: z.string(),
    auth_method: z.string(),
    name: z.string(),
    bucket: z.string(),
    region: z.string(),
    endpoint: z.string(),
    account_id: z.string(),
    force_path_style: z.boolean(),
    role_arn: z.string(),
    staging_prefix: z.string(),
    ready_prefix: z.string(),
    staging_mode: z.enum(["CONNECTION", "PLATFORM"]),
    credentials: z.object({}).loose(),
  })
  .superRefine((value, ctx) => {
    const issue: Issue = (path, message) => ctx.addIssue({ code: "custom", path, message });
    const provider = value.provider as ProviderType | "";
    const method = value.auth_method as AuthMethod | "";

    if (!provider || !CREATABLE_PROVIDERS.includes(provider)) {
      issue(["provider"], "Choose a provider.");
      return;
    }
    if (!method || !DEFAULT_AUTH_METHODS[provider].includes(method)) {
      issue(["auth_method"], "Choose how the platform signs in.");
      return;
    }
    const fields = fieldsFor(provider, method);

    checkName(value.name, issue);

    const bucketProblem = bucketError(value.bucket, fields.bucketLabel);
    if (bucketProblem) issue(["bucket"], bucketProblem);

    const region = cleanRegion(value.region);
    if (fields.region && !REGION.test(region)) issue(["region"], "Use lowercase letters, digits and - (up to 64).");
    else if (fields.region === "required" && !region) issue(["region"], "Enter the region, for example eu-west-1.");

    checkEndpoint(value.endpoint, fields.endpoint === "required", issue);

    const accountId = value.account_id.trim();
    if (fields.account_id && !ACCOUNT_ID.test(accountId)) issue(["account_id"], "Use letters, digits and - (up to 64).");
    else if (fields.account_id === "required" && !accountId) {
      issue(["account_id"], provider === "AZURE_BLOB" ? "Enter the storage account name." : "Enter the Cloudflare account ID.");
    }

    if (fields.role_arn && !ROLE_ARN.test(value.role_arn.trim())) {
      issue(["role_arn"], "Enter a role ARN such as arn:aws:iam::123456789012:role/storage.");
    }

    checkPrefixes(value.staging_prefix, value.ready_prefix, issue);

    if (method === "SAS_TOKEN" && value.staging_mode !== "PLATFORM") {
      issue(["staging_mode"], "A SAS token cannot sign upload links, so staging must be Platform.");
    }

    checkCredentials(method, value.credentials as Partial<CredentialValues>, issue);
  });

// The service account key is sent as one line; the form accepts the file as saved.
function compactJson(text: string) {
  try {
    return JSON.stringify(JSON.parse(text));
  } catch {
    return text;
  }
}

function credentialsFor(method: AuthMethod, values: CredentialValues): StorageCredentials | undefined {
  const result: StorageCredentials = {};
  for (const field of CREDENTIAL_FIELDS[method]) {
    const value = values[field.name];
    if (!value.trim()) continue;
    result[field.name] = field.name === "service_account_json" ? compactJson(value) : value.trim();
  }
  return Object.keys(result).length > 0 ? result : undefined;
}

// Only fields that apply to the provider and method are sent; the API rejects the rest.
export function buildCreateRequest(form: CreateForm): CreateConnectionRequest {
  const provider = form.provider as ProviderType;
  const method = form.auth_method as AuthMethod;
  const fields = fieldsFor(provider, method);
  const request: CreateConnectionRequest = {
    name: cleanText(form.name),
    provider,
    auth_method: method,
    bucket: cleanBucket(form.bucket),
    staging_prefix: cleanPrefix(form.staging_prefix) || undefined,
    ready_prefix: cleanPrefix(form.ready_prefix) || undefined,
    staging_mode: method === "SAS_TOKEN" ? "PLATFORM" : form.staging_mode,
  };
  if (fields.region && cleanRegion(form.region)) request.region = cleanRegion(form.region);
  if (fields.endpoint && cleanEndpoint(form.endpoint)) request.endpoint = cleanEndpoint(form.endpoint);
  if (fields.account_id) request.account_id = form.account_id.trim();
  if (fields.force_path_style) request.force_path_style = form.force_path_style;
  if (fields.role_arn) request.role_arn = form.role_arn.trim();
  const credentials = credentialsFor(method, form.credentials);
  if (credentials) request.credentials = credentials;
  return request;
}

export function buildCredentials(method: AuthMethod, values: CredentialValues) {
  return credentialsFor(method, values) ?? {};
}

export type EditForm = {
  name: string;
  region: string;
  endpoint: string;
  force_path_style: boolean;
  staging_mode: StagingMode;
};

export function editDefaults(connection: StorageConnection): EditForm {
  return {
    name: connection.name,
    region: connection.region ?? "",
    endpoint: connection.endpoint ?? "",
    force_path_style: connection.force_path_style,
    staging_mode: connection.staging_mode ?? "CONNECTION",
  };
}

export const editSchema = (connection: StorageConnection) =>
  z
    .object({
      name: z.string(),
      region: z.string(),
      endpoint: z.string(),
      force_path_style: z.boolean(),
      staging_mode: z.enum(["CONNECTION", "PLATFORM"]),
    })
    .superRefine((value, ctx) => {
      const issue: Issue = (path, message) => ctx.addIssue({ code: "custom", path, message });
      const fields = fieldsFor(connection.provider, connection.auth_method);
      checkName(value.name, issue);
      if (fields.region && !REGION.test(cleanRegion(value.region))) issue(["region"], "Use lowercase letters, digits and - (up to 64).");
      else if (fields.region === "required" && !cleanRegion(value.region)) issue(["region"], "Enter the region, for example eu-west-1.");
      checkEndpoint(value.endpoint, fields.endpoint === "required", issue);
      if (connection.auth_method === "SAS_TOKEN" && value.staging_mode !== "PLATFORM") {
        issue(["staging_mode"], "A SAS token cannot sign upload links, so staging must be Platform.");
      }
    });

// Sends only what changed, with the revision from the latest read.
export function buildUpdateRequest(connection: StorageConnection, form: EditForm): UpdateConnectionRequest {
  const original = editDefaults(connection);
  const request: UpdateConnectionRequest = { revision: connection.revision };
  if (cleanText(form.name) !== original.name) request.name = cleanText(form.name);
  if (cleanRegion(form.region) !== original.region) request.region = cleanRegion(form.region);
  const endpoint = cleanEndpoint(form.endpoint);
  if (endpoint !== original.endpoint) request.endpoint = endpoint;
  if (form.force_path_style !== original.force_path_style) request.force_path_style = form.force_path_style;
  if (form.staging_mode !== original.staging_mode) request.staging_mode = form.staging_mode;
  return request;
}

// Changing these on an ACTIVE or DISABLED connection makes the backend re-test it first.
export function changesReachability(request: UpdateConnectionRequest) {
  return (
    request.region !== undefined || request.endpoint !== undefined || request.force_path_style !== undefined
  );
}
