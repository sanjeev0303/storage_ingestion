// The backend answers errors in three shapes: {error, code}, a bare {error},
// and plain text for malformed JSON. ApiError flattens all of them.
export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly retryAfter?: number;
  readonly requestId?: string;
  readonly body?: unknown;

  constructor(init: {
    status: number;
    message: string;
    code?: string;
    retryAfter?: number;
    requestId?: string;
    body?: unknown;
  }) {
    super(init.message);
    this.name = "ApiError";
    this.status = init.status;
    this.code = init.code;
    this.retryAfter = init.retryAfter;
    this.requestId = init.requestId;
    this.body = init.body;
  }

  get isNetwork() {
    return this.status === 0;
  }
  get isUnauthenticated() {
    return this.status === 401;
  }
  get isForbidden() {
    return this.status === 403;
  }
  get isNotFound() {
    return this.status === 404;
  }
  get isConflict() {
    return this.status === 409;
  }
  get isValidation() {
    return this.status === 400 || this.status === 422;
  }
  get isRateLimited() {
    return this.status === 429;
  }
}

export function isApiError(error: unknown): error is ApiError {
  return error instanceof ApiError;
}

// Reads a failed response whatever shape the body has.
export async function readError(res: Response): Promise<ApiError> {
  const text = await res.text().catch(() => "");
  let body: unknown = text;
  let message = text.trim();
  let code: string | undefined;

  try {
    body = JSON.parse(text);
    const record = body as { error?: unknown; code?: unknown };
    if (typeof record.error === "string") message = record.error;
    if (typeof record.code === "string") code = record.code;
  } catch {
    // plain text body
  }

  // The server's own words for an internal failure stay on the server. The proxy
  // already replaces them; this covers any path that reaches the API directly.
  if (res.status >= 500) {
    message = "";
    body = undefined;
  }
  if (!message) message = defaultMessage(res.status);
  const retry = Number(res.headers.get("retry-after"));

  return new ApiError({
    status: res.status,
    message,
    code,
    retryAfter: Number.isFinite(retry) && retry > 0 ? retry : undefined,
    requestId: res.headers.get("x-request-id") ?? undefined,
    body,
  });
}

export function defaultMessage(status: number) {
  switch (status) {
    case 401:
      return "Your session has ended. Sign in again.";
    case 403:
      return "You do not have permission to do that.";
    case 404:
      return "That could not be found.";
    case 409:
      return "That conflicts with the current state.";
    case 429:
      return "Too many requests. Try again shortly.";
    case 502:
    case 503:
    case 504:
      return "The storage API is unavailable. Try again shortly.";
    default:
      return "The request could not be completed.";
  }
}

// Every failure is told in three parts: what happened, why, and what to do next.
// Internal details never reach this far: a failed server call carries only a status
// and a reference, and the real cause is in the server log under that reference.
export type Explained = { what: string; why: string; action: string; reference?: string };

export function explainError(error: unknown): Explained {
  if (!isApiError(error)) {
    return {
      what: "Something went wrong on our side.",
      why: "The page hit an unexpected problem. Nothing you did caused it.",
      action: "Try again in a moment.",
    };
  }
  if (error.code === "timeout") {
    return {
      what: "The request took too long.",
      why: "The server was slow to answer.",
      action: "Try again in a moment.",
    };
  }
  if (error.isNetwork) {
    return {
      what: "Could not reach the server.",
      why: "The connection dropped, or the server is not answering.",
      action: "Check your connection and try again.",
    };
  }
  if (error.isUnauthenticated) {
    return {
      what: "Your session has ended.",
      why: "You were signed out, or the session ran out of time.",
      action: "Sign in again to continue.",
    };
  }
  if (error.isForbidden) {
    return {
      what: "Your role cannot do this.",
      why: "Your role does not include the permission this needs.",
      action: "Ask an organization admin for access.",
    };
  }
  if (error.isNotFound) {
    return {
      what: "That could not be found.",
      why: "It may have been deleted, or the link is out of date.",
      action: "Go back and open it from the list.",
    };
  }
  if (error.isRateLimited) {
    return {
      what: "Too many requests.",
      why: "A lot of requests came in a short time.",
      action: error.retryAfter ? `Wait ${error.retryAfter} seconds and try again.` : "Wait a moment and try again.",
    };
  }
  if (error.isConflict) {
    return {
      what: "That could not be saved.",
      why: error.message,
      action: "Refresh the page and try again.",
    };
  }
  if (error.isValidation) {
    return {
      what: "Some details were not accepted.",
      why: error.message,
      action: "Fix them and try again.",
    };
  }
  return {
    what: "Something went wrong on our side.",
    why: "The server hit an unexpected problem. Nothing you did caused it.",
    action: error.requestId ? "Try again in a moment. If it keeps happening, quote the reference." : "Try again in a moment.",
    reference: error.requestId,
  };
}

// One sentence for toasts and inline messages. A rejected form field keeps the
// server's own wording, which already says what to fix.
export function describeError(error: unknown): string {
  if (isApiError(error) && (error.isValidation || error.isConflict)) return error.message;
  const { what, why, action, reference } = explainError(error);
  return `${what} ${why} ${action}${reference ? ` Reference ${reference}.` : ""}`;
}

export function isRetryable(error: unknown): boolean {
  if (!isApiError(error)) return false;
  return [0, 429, 502, 503, 504].includes(error.status);
}
