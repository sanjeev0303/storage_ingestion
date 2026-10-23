import type { NextConfig } from "next";

const isDev = process.env.NODE_ENV !== "production";

// Uploads and downloads go straight to the storage provider, whose host is
// unknown ahead of time, so connect-src has to allow any https origin. A
// provider reached over plain http (local MinIO) must be named explicitly in
// CSP_STORAGE_ORIGINS, space separated, e.g. "http://127.0.0.1:9000".
const extraStorageOrigins = process.env.CSP_STORAGE_ORIGINS ?? "";

// 'wasm-unsafe-eval' lets the streaming SHA-256 module (WebAssembly) load. It
// permits WebAssembly only, not eval() of strings.
const csp = [
  "default-src 'self'",
  `script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'${isDev ? " 'unsafe-eval'" : ""}`,
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data: blob:",
  "font-src 'self'",
  `connect-src 'self' https: ${extraStorageOrigins} ${isDev ? "http://localhost:* http://127.0.0.1:* ws://localhost:*" : ""}`.trim(),
  "frame-ancestors 'none'",
  "base-uri 'self'",
  "form-action 'self'",
  "object-src 'none'",
].join("; ");

const nextConfig: NextConfig = {
  poweredByHeader: false,
  turbopack: {
    rules: {
      "*.css": {
        loaders: ["@tailwindcss/turbopack"],
        as: "*.css",
      },
    },
  },
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "Content-Security-Policy", value: csp },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "X-Frame-Options", value: "DENY" },
          { key: "Referrer-Policy", value: "same-origin" },
          { key: "Permissions-Policy", value: "camera=(), microphone=(), geolocation=()" },
        ],
      },
    ];
  },
};

export default nextConfig;
