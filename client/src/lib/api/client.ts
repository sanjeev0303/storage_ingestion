import { ApiError, readError } from "./errors";

const BASE = "/api/proxy";
const DEFAULT_TIMEOUT_MS = 30_000;

type Query = Record<string, string | number | boolean | undefined | null | string[]>;

export type RequestOptions = {
  method?: "GET" | "HEAD" | "POST" | "PUT" | "PATCH" | "DELETE";
  query?: Query;
  // Objects are sent as JSON. Strings, Blobs and buffers are sent as they are.
  body?: unknown;
  headers?: Record<string, string>;
  signal?: AbortSignal;
  // The organization the request is for. Omit to use the active one; pass
  // false for account routes, which reject a header that names an organization.
  org?: string | false;
  timeoutMs?: number;
};

let activeOrganization: string | null = null;
const unauthenticatedListeners = new Set<() => void>();

export function setActiveOrganization(id: string | null) {
  activeOrganization = id;
}

export function getActiveOrganization() {
  return activeOrganization;
}

export function onUnauthenticated(listener: () => void) {
  unauthenticatedListeners.add(listener);
  return () => {
    unauthenticatedListeners.delete(listener);
  };
}

function buildUrl(path: string, query?: Query) {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value === undefined || value === null || value === "") continue;
    if (Array.isArray(value)) {
      for (const item of value) params.append(key, item);
    } else {
      params.set(key, String(value));
    }
  }
  const search = params.toString();
  return `${BASE}${path}${search ? `?${search}` : ""}`;
}

function isRawBody(body: unknown) {
  return (
    typeof body === "string" ||
    body instanceof Blob ||
    body instanceof ArrayBuffer ||
    ArrayBuffer.isView(body)
  );
}

// Sends the request and returns the Response untouched when it succeeded.
// Failures are thrown as ApiError.
export async function apiRaw(path: string, options: RequestOptions = {}): Promise<Response> {
  const { method = "GET", query, body, signal, org, timeoutMs = DEFAULT_TIMEOUT_MS } = options;
  const headers = new Headers(options.headers);

  const organization = org === undefined ? activeOrganization : org;
  if (organization) headers.set("X-Organization-ID", organization);

  let payload: BodyInit | undefined;
  if (body !== undefined) {
    if (isRawBody(body)) {
      payload = body as BodyInit;
    } else {
      payload = JSON.stringify(body);
      if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
    }
  }

  const timeout = AbortSignal.timeout(timeoutMs);
  const combined = signal ? AbortSignal.any([signal, timeout]) : timeout;

  let res: Response;
  try {
    res = await fetch(buildUrl(path, query), {
      method,
      headers,
      body: payload,
      signal: combined,
      credentials: "same-origin",
      cache: "no-store",
    });
  } catch (error) {
    if (signal?.aborted) throw error;
    const timedOut = timeout.aborted;
    throw new ApiError({
      status: 0,
      message: timedOut
        ? "The request took too long. Check the connection and try again."
        : "Could not reach the server. Check the connection and try again.",
      code: timedOut ? "timeout" : "network",
    });
  }

  if (res.ok) return res;

  const error = await readError(res);
  if (error.status === 401) {
    for (const listener of unauthenticatedListeners) listener();
  }
  throw error;
}

export async function api<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const res = await apiRaw(path, options);
  if (res.status === 204 || res.status === 205) return undefined as T;
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

// Account routes (sign-in state, sessions, password) take no organization.
export const accountApi = <T>(path: string, options: RequestOptions = {}) =>
  api<T>(path, { ...options, org: false });

export function newIdempotencyKey() {
  return crypto.randomUUID();
}
