import type { AssetStatus } from "./types";

const IMAGE_TYPES = ["image/png", "image/jpeg", "image/gif", "image/webp"];
const TEXT_TYPES = ["text/plain", "application/json"];
export const MAX_IMAGE_BYTES = 20 * 1024 * 1024;
export const MAX_TEXT_BYTES = 256 * 1024;

export type PreviewPlan = { kind: "image" | "text" } | { kind: "none"; reason: string };

// Only types a browser cannot run as script are previewed. SVG, HTML and XML
// are never rendered inline; they stay download-only.
export function planPreview(contentType: string, sizeBytes: number, status: AssetStatus): PreviewPlan {
  if (status !== "AVAILABLE") return { kind: "none", reason: "Only available versions can be previewed." };
  const type = contentType.split(";")[0].trim().toLowerCase();
  if (IMAGE_TYPES.includes(type)) {
    if (sizeBytes > MAX_IMAGE_BYTES) {
      return { kind: "none", reason: "Too large to preview (limit 20 MiB). Download it instead." };
    }
    return { kind: "image" };
  }
  if (TEXT_TYPES.includes(type)) {
    if (sizeBytes > MAX_TEXT_BYTES) {
      return { kind: "none", reason: "Too large to preview (limit 256 KiB). Download it instead." };
    }
    return { kind: "text" };
  }
  return { kind: "none", reason: "This type is not previewed in the browser. Download it instead." };
}

export function isTombstone(status: AssetStatus) {
  return status === "SOFT_DELETED" || status === "DELETED";
}

// Token links are path-only and go through the same-origin proxy, which adds
// no Authorization header the token does not need. Other links are absolute.
export function downloadHref(url: string, delivery: string) {
  if (delivery === "token") return url.replace(/^\/api\/v1\//, "/api/proxy/");
  return url;
}
