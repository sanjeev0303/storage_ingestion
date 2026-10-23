"use client";

import { ErrorPanel } from "@/components/ui/error-panel";

// Inside the console, so the navigation stays and only the page area is replaced.
export default function ConsoleError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return <ErrorPanel error={error} reset={reset} home={{ href: "/app/overview", label: "Go to the overview" }} />;
}
