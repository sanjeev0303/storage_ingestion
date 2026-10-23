"use client";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { isApiError, isRetryable } from "@/lib/api/errors";

function makeClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 30_000,
        gcTime: 5 * 60_000,
        refetchOnWindowFocus: false,
        retry: (count, error) => count < 2 && isRetryable(error),
        retryDelay: (count, error) => {
          const wait = isApiError(error) && error.retryAfter ? error.retryAfter * 1000 : 800 * 2 ** count;
          return Math.min(wait, 10_000);
        },
      },
      // Mutations are never retried automatically: most are not idempotent.
      mutations: { retry: false },
    },
  });
}

export function QueryProvider({ children }: { children: ReactNode }) {
  const [client] = useState(makeClient);
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}
