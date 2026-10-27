import type { HTMLAttributes, TdHTMLAttributes, ThHTMLAttributes } from "react";
import { cn } from "@/lib/utils/cn";

export function Table({ className, ...rest }: HTMLAttributes<HTMLTableElement>) {
  return (
    // relative: an absolutely placed child (an sr-only label) is clipped by
    // this scroller instead of widening the whole page.
    <div className="relative overflow-x-auto border border-faint" data-lenis-prevent-horizontal tabIndex={0} role="region" aria-label="Table">
      <table className={cn("w-full min-w-full border-collapse text-left text-base", className)} {...rest} />
    </div>
  );
}

export function Th({ className, ...rest }: ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <th
      scope="col"
      className={cn("border-b-2 border-faint px-4 py-2.5 text-sm font-semibold whitespace-nowrap text-muted", className)}
      {...rest}
    />
  );
}

export function Td({ className, ...rest }: TdHTMLAttributes<HTMLTableCellElement>) {
  return <td className={cn("border-b border-faint px-4 py-2.5 align-top", className)} {...rest} />;
}
