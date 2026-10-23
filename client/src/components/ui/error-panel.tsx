"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { newReference, reportError } from "@/lib/errors/report";
import { Button, buttonClass } from "./button";

// The page for an error a boundary caught. It says what happened, why, and what
// to do, and it never prints the error itself: that went to the server log under
// the reference shown here.
export function ErrorPanel({
  error,
  reset,
  home,
}: {
  error: Error & { digest?: string };
  reset: () => void;
  home: { href: string; label: string };
}) {
  const [generated] = useState(newReference);
  // A server error already has a digest that matches Next's own server log line.
  const reference = error.digest ?? generated;

  useEffect(() => {
    reportError(error, { reference, source: "boundary", digest: error.digest });
  }, [error, reference]);

  return (
    <div role="alert" className="neo-in mx-auto my-12 flex max-w-xl flex-col items-start gap-4 p-8">
      <h1 className="text-xl font-semibold">This page ran into a problem.</h1>
      <p className="text-base text-muted">An unexpected error stopped it from loading. Nothing you did caused it, and it has been recorded.</p>
      <p className="text-base font-medium">Try again. If it keeps happening, go back and quote the reference below.</p>
      <p className="mono-data text-sm text-muted">Reference {reference}</p>
      <div className="flex flex-wrap gap-2">
        <Button variant="primary" onClick={reset}>
          Try again
        </Button>
        <Link href={home.href} className={buttonClass("secondary")}>
          {home.label}
        </Link>
      </div>
    </div>
  );
}
