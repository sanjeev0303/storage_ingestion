import type { ElementType, HTMLAttributes, ReactNode } from "react";
import { cn } from "@/lib/utils/cn";

type PanelProps = Omit<HTMLAttributes<HTMLElement>, "title"> & {
  as?: ElementType;
  title?: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  // "in" is a recessed well; use it for content that belongs to a raised panel.
  tone?: "raised" | "in";
  padded?: boolean;
};

export function Panel({
  as: Tag = "section",
  title,
  description,
  actions,
  tone = "raised",
  padded = true,
  className,
  children,
  ...rest
}: PanelProps) {
  return (
    <Tag className={cn(tone === "raised" ? "neo" : "neo-in", "min-w-0", className)} {...rest}>
      {(title || actions) && (
        <header className={cn("flex flex-wrap items-start justify-between gap-3", padded && "px-5 pt-5")}>
          <div className="min-w-0">
            {title && <h2 className="text-lg font-semibold">{title}</h2>}
            {description && <p className="mt-0.5 text-sm text-muted">{description}</p>}
          </div>
          {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={cn(padded && "p-5", (title || actions) && padded && "pt-4")}>{children}</div>
    </Tag>
  );
}
