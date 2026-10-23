// Only same-site console paths are allowed as a post-login destination.
export function safeNext(value: string | null | undefined, fallback = "/app/overview") {
  if (!value) return fallback;
  if (!value.startsWith("/app")) return fallback;
  if (value.startsWith("//") || value.includes("\\") || value.includes("://")) return fallback;
  return value;
}
