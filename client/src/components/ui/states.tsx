import { LockSimple, Warning } from "@phosphor-icons/react/dist/ssr";
import type { ReactNode } from "react";
import { explainError, isApiError } from "@/lib/api/errors";
import { cn } from "@/lib/utils/cn";
import { Button } from "./button";

export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={cn("neo-in-sm h-4 animate-pulse", className)} />;
}

export function SkeletonRows({ rows = 5 }: { rows?: number }) {
  return (
    <div role="status" aria-label="Loading" className="flex flex-col gap-3">
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-9" />
      ))}
    </div>
  );
}

type EmptyProps = {
  title: string;
  // Teach the screen: what goes here and how to get it started.
  children?: ReactNode;
  action?: ReactNode;
};

export function EmptyState({ title, children, action }: EmptyProps) {
  return (
    <div className="neo-in flex flex-col items-start gap-3 p-8">
      <h3 className="text-md font-semibold">{title}</h3>
      {children && <p className="max-w-prose text-base text-muted">{children}</p>}
      {action}
    </div>
  );
}

export function ErrorState({ error, onRetry, title }: { error: unknown; onRetry?: () => void; title?: string }) {
  if (isApiError(error) && error.isForbidden) {
    return <PermissionDenied detail={error.message} />;
  }
  const { what, why, action, reference } = explainError(error);
  return (
    <div role="alert" className="neo-in flex flex-col items-start gap-3 p-8">
      <div className="flex items-center gap-2">
        <Warning aria-hidden />
        <h3 className="text-md font-semibold">{title ?? what}</h3>
      </div>
      <p className="max-w-prose text-base text-muted">{why}</p>
      <p className="max-w-prose text-base font-medium">{action}</p>
      {reference && <p className="mono-data text-sm text-muted">Reference {reference}</p>}
      {onRetry && <Button onClick={onRetry}>Try again</Button>}
    </div>
  );
}

export function PermissionDenied({ detail }: { detail?: string }) {
  return (
    <div role="alert" className="neo-in flex flex-col items-start gap-3 p-8">
      <div className="flex items-center gap-2">
        <LockSimple aria-hidden />
        <h3 className="text-md font-semibold">Your role cannot do this</h3>
      </div>
      <p className="max-w-prose text-base text-muted">
        {detail && detail !== "permission denied" ? detail : "Ask an organization admin if you need access."}
      </p>
    </div>
  );
}
