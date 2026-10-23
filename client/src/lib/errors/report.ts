// Sends a crash to the server log and hands back a short reference to show the
// person. The details are never written to the browser console.

export function newReference() {
  return crypto.randomUUID().slice(0, 8);
}

// For a failure that is caught and handled, but whose details (a storage
// provider's reply, an unexpected exception) must not be shown to the person.
export function reportProblem(source: string, message: string) {
  const reference = newReference();
  reportError(new Error(message), { reference, source });
  return reference;
}

const recent = new Map<string, number>();

export function reportError(
  error: unknown,
  context: { reference: string; source: string; digest?: string },
) {
  if (typeof window === "undefined") return;
  const known = error instanceof Error ? error : new Error(String(error));
  // The same fault repeating in a loop is reported once.
  const key = `${known.name}:${known.message}`;
  const last = recent.get(key);
  if (last && Date.now() - last < 5_000) return;
  recent.set(key, Date.now());

  const body = JSON.stringify({
    reference: context.reference,
    source: context.source,
    digest: context.digest,
    path: window.location.pathname,
    name: known.name,
    message: known.message,
    stack: known.stack,
  });
  fetch("/api/client-error", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
    keepalive: true,
    credentials: "same-origin",
  }).catch(() => {
    // Nothing more can be done from here, and nothing should surface.
  });
}
