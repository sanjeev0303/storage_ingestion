import type { ReactNode } from "react";
import { Button } from "./button";

// The form's submit button. It stays disabled until the form is valid, and says
// why in words so a disabled button is never a mystery.
export function SubmitButton({
  valid,
  loading,
  children,
  hint = "Fill in the required fields to continue.",
  className,
}: {
  valid: boolean;
  loading?: boolean;
  children: ReactNode;
  hint?: string;
  className?: string;
}) {
  return (
    <div className="flex flex-col items-start gap-1.5">
      <Button type="submit" variant="primary" disabled={!valid} loading={loading} className={className}>
        {children}
      </Button>
      {!valid && !loading && <p className="text-sm text-muted">{hint}</p>}
    </div>
  );
}
