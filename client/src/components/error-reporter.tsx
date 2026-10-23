"use client";

import { useEffect } from "react";
import { newReference, reportError } from "@/lib/errors/report";

// Errors nobody caught would print to the browser console. In production they go
// to the server log instead, and the console stays quiet.
export function ErrorReporter() {
  useEffect(() => {
    if (process.env.NODE_ENV !== "production") return;

    const onError = (event: ErrorEvent) => {
      event.preventDefault();
      reportError(event.error ?? new Error(event.message), { reference: newReference(), source: "window" });
    };
    const onRejection = (event: PromiseRejectionEvent) => {
      event.preventDefault();
      // A cancelled request is not a fault.
      if (event.reason instanceof DOMException && event.reason.name === "AbortError") return;
      reportError(event.reason, { reference: newReference(), source: "promise" });
    };

    window.addEventListener("error", onError);
    window.addEventListener("unhandledrejection", onRejection);
    return () => {
      window.removeEventListener("error", onError);
      window.removeEventListener("unhandledrejection", onRejection);
    };
  }, []);

  return null;
}
