import type { ReactNode } from "react";
import { cn } from "@/lib/utils/cn";

// The page's one content width; every section's edges line up on it.
export function Wrap({ className, children }: { className?: string; children: ReactNode }) {
  return <div className={cn("mx-auto w-full max-w-[var(--content-max)] px-5 sm:px-8 lg:px-12", className)}>{children}</div>;
}
