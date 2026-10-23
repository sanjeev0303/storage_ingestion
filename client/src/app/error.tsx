"use client";

import { ErrorPanel } from "@/components/ui/error-panel";

export default function RootError({ error, reset }: { error: Error & { digest?: string }; reset: () => void }) {
  return <ErrorPanel error={error} reset={reset} home={{ href: "/", label: "Go to the home page" }} />;
}
