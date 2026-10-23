import { abortError } from "./sha256";

export class UploadHttpError extends Error {
  readonly status: number;
  readonly body: string;

  constructor(status: number, body: string) {
    super(`The storage provider answered ${status}`);
    this.name = "UploadHttpError";
    this.status = status;
    this.body = body;
  }

  // A presigned URL that has expired or was signed for a skewed clock. Ask the API for a new URL.
  get urlExpired() {
    return this.status === 403 && /expired|RequestTimeTooSkewed|AccessDenied/i.test(this.body);
  }
}

export class UploadNetworkError extends Error {
  constructor() {
    super("The connection to the storage provider failed.");
    this.name = "UploadNetworkError";
  }
}

export class MissingEtagError extends Error {
  constructor() {
    super(
      "The storage provider did not expose the ETag header, so the part cannot be confirmed. " +
        "Add ETag to the bucket's CORS ExposeHeaders.",
    );
    this.name = "MissingEtagError";
  }
}

// Network drops, throttling and server errors are worth another try; the
// rest (a rejected signature, a bad request) will fail the same way again.
export function isTransientPutError(error: unknown) {
  if (error instanceof UploadNetworkError) return true;
  if (error instanceof UploadHttpError) return error.status >= 500 || error.status === 408 || error.status === 429;
  return false;
}

type PutOptions = {
  headers?: Record<string, string>;
  signal?: AbortSignal;
  onProgress?: (bytesSent: number) => void;
  timeoutMs?: number;
};

// fetch() cannot report upload progress, so presigned PUTs use XMLHttpRequest.
// No cookies and no Authorization header are sent: the signature is in the URL.
export function putBlob(url: string, body: Blob, options: PutOptions = {}): Promise<{ status: number; etag: string | null }> {
  const { headers = {}, signal, onProgress, timeoutMs = 10 * 60_000 } = options;

  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(abortError());

    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url);
    xhr.withCredentials = false;
    xhr.timeout = timeoutMs;
    for (const [name, value] of Object.entries(headers)) xhr.setRequestHeader(name, value);

    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress?.(event.loaded);
    };
    xhr.onload = () => {
      cleanup();
      if (xhr.status >= 200 && xhr.status < 300) {
        onProgress?.(body.size);
        resolve({ status: xhr.status, etag: xhr.getResponseHeader("ETag") });
      } else {
        reject(new UploadHttpError(xhr.status, (xhr.responseText ?? "").slice(0, 300)));
      }
    };
    xhr.onerror = xhr.ontimeout = () => {
      cleanup();
      reject(new UploadNetworkError());
    };
    xhr.onabort = () => {
      cleanup();
      reject(abortError());
    };

    const onAbort = () => xhr.abort();
    const cleanup = () => signal?.removeEventListener("abort", onAbort);
    signal?.addEventListener("abort", onAbort, { once: true });

    xhr.send(body);
  });
}
