// Server-side error log. This is the only place failure details are written:
// the browser gets a generic message and a reference, never the cause.

// Tokens and credentials must not land in a log file either.
// Download links carry a bare token after downloads/, with no prefix.
const SECRET = /\b(?:sat|srt|sct|sit)_[A-Za-z0-9_-]+|downloads\/[A-Za-z0-9_-]+|Bearer\s+\S+|("?(?:access_token|refresh_token|password|secret\w*|token)"?\s*[:=]\s*)"?[^"\s,}]+/gi;

export function redact(text: string, max = 2000) {
  return text.replace(SECRET, (_match, key) => (key ? `${key}[redacted]` : "[redacted]")).slice(0, max);
}

export function logServerError(event: string, details: Record<string, unknown>) {
  const safe: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(details)) {
    safe[key] = typeof value === "string" ? redact(value) : value;
  }
  console.error(JSON.stringify({ level: "error", time: new Date().toISOString(), event, ...safe }));
}
