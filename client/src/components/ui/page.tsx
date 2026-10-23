"use client";

import { CaretRight } from "@phosphor-icons/react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { cn } from "@/lib/utils/cn";
import { Button } from "./button";

export type Crumb = { label: string; href?: string };

export function PageHeader({
  title,
  description,
  crumbs,
  actions,
}: {
  title: ReactNode;
  description?: ReactNode;
  crumbs?: Crumb[];
  actions?: ReactNode;
}) {
  return (
    <header className="mb-6 flex flex-col gap-3">
      {crumbs && crumbs.length > 0 && (
        <nav aria-label="Breadcrumb">
          <ol className="flex flex-wrap items-center gap-1 text-sm text-muted">
            {crumbs.map((crumb, i) => (
              <li key={`${crumb.label}-${i}`} className="flex items-center gap-1">
                {i > 0 && <CaretRight aria-hidden className="size-3" />}
                {crumb.href ? (
                  <Link href={crumb.href} className="underline-offset-2 hover:underline">
                    {crumb.label}
                  </Link>
                ) : (
                  <span aria-current="page">{crumb.label}</span>
                )}
              </li>
            ))}
          </ol>
        </nav>
      )}
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold tracking-tight [overflow-wrap:anywhere]">{title}</h1>
          {description && <p className="mt-1 max-w-prose text-base text-muted">{description}</p>}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
    </header>
  );
}

// Tabs that are real routes, so each one has a URL that can be shared and reloaded.
export function TabNav({ tabs, label }: { tabs: { href: string; label: string }[]; label: string }) {
  const pathname = usePathname();
  // A nested page (a version under Versions) keeps its tab lit: the tab whose
  // href is the longest prefix of the path is the current one.
  const active = tabs
    .filter((tab) => pathname === tab.href || pathname.startsWith(`${tab.href}/`))
    .sort((a, b) => b.href.length - a.href.length)[0]?.href;
  return (
    <nav aria-label={label} className="mb-6">
      <ul className="neo-in-sm inline-flex max-w-full flex-wrap gap-1 p-1">
        {tabs.map((tab) => {
          const current = tab.href === active;
          return (
            <li key={tab.href}>
              <Link
                href={tab.href}
                aria-current={current ? "page" : undefined}
                className={cn(
                  "block rounded-sm px-3.5 py-1.5 text-base font-medium transition-colors",
                  current ? "bg-ink text-ground" : "hover:bg-faint",
                )}
              >
                {tab.label}
              </Link>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}

export function LoadMore({
  hasNext,
  loading,
  onLoad,
  shown,
}: {
  hasNext: boolean;
  loading: boolean;
  onLoad: () => void;
  shown: number;
}) {
  return (
    <div className="mt-4 flex items-center justify-between gap-3 text-sm text-muted">
      <span aria-live="polite">
        {shown} loaded{hasNext ? ", more available" : ", end of list"}
      </span>
      {hasNext && (
        <Button onClick={onLoad} loading={loading}>
          Load more
        </Button>
      )}
    </div>
  );
}

export function DefList({ items }: { items: { label: string; value: ReactNode }[] }) {
  return (
    <dl className="grid grid-cols-1 gap-x-8 gap-y-3 sm:grid-cols-2">
      {items.map((item) => (
        <div key={item.label} className="min-w-0">
          <dt className="text-sm text-muted">{item.label}</dt>
          <dd className="mt-0.5 text-base font-medium [overflow-wrap:anywhere]">{item.value}</dd>
        </div>
      ))}
    </dl>
  );
}

// A usage meter. Used / limit are stated in text; the bar is a second cue.
export function Meter({ label, used, limit, format }: { label: string; used: number; limit: number; format: (n: number) => string }) {
  const ratio = limit > 0 ? Math.min(used / limit, 1) : 0;
  return (
    <div>
      <div className="flex items-baseline justify-between gap-3 text-base">
        <span className="font-medium">{label}</span>
        <span className="text-muted tabular-nums">
          {format(used)} of {limit > 0 ? format(limit) : "no limit set"}
        </span>
      </div>
      <div
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={limit}
        aria-valuenow={used}
        className="neo-in-sm mt-1.5 h-3 overflow-hidden p-0.5"
      >
        <div className={cn("h-full", ratio > 0.9 ? "bg-accent outline outline-1 outline-ink" : "bg-ink")} style={{ width: `${ratio * 100}%` }} />
      </div>
    </div>
  );
}
