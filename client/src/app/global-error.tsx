"use client";

import { ErrorPanel } from "@/components/ui/error-panel";
import "./globals.css";

// Used only when the root layout itself fails, so it brings its own html and body.
export default function GlobalError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return (
    <html lang="en">
      <body>
        <ErrorPanel error={error} reset={reset} home={{ href: "/", label: "Go to the home page" }} />
      </body>
    </html>
  );
}
