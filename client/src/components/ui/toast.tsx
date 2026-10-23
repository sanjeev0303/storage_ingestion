"use client";

import { Toaster as Sonner, toast } from "sonner";
import { describeError } from "@/lib/api/errors";

export function Toaster() {
  return (
    <Sonner
      position="bottom-right"
      toastOptions={{
        unstyled: true,
        classNames: {
          toast: "neo flex w-80 items-start gap-3 p-4 text-base text-ink",
          title: "font-semibold",
          description: "text-sm text-muted",
        },
      }}
    />
  );
}

export const notify = {
  success: (message: string) => toast(message),
  error: (error: unknown) => toast(describeError(error)),
  info: (message: string, description?: string) => toast(message, { description }),
};
